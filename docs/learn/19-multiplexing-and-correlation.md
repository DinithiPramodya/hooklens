# 19 — Multiplexing and correlation

*Phase 3, unit 3. Covers the curriculum bullets "multiplexing and correlation: request IDs,
the `map[id]chan` pattern, why a mutex and not a channel-of-channels" and "**goroutine
leaks** and why every exit path must delete its map entry". This note is failure modes 3,
4 and 5 from `PLAN.md`.*

## Brief

**What is this thing?** Two ideas that only make sense together. **Multiplexing** is
carrying many independent conversations over one connection. **Correlation** is matching
each reply to the request that caused it.

**What problem does it exist to solve?** Ordinary HTTP hands you correlation for free, and
it is easy not to notice. Under HTTP/1.1 a connection carries one exchange at a time: you
write a request, you read a response, and *the socket itself is the correlation* — the
reply is the reply because it is the only thing that could be.

That disappears the moment one long-lived connection carries many concurrent requests. Two
webhooks land at the same instant, both are forwarded down the same WebSocket, and two
responses come back. Nothing in the transport says which is which. The local app is free
to answer the second one first, and it will: a request that hits a cache returns in a
millisecond while one that touches a database takes two hundred.

The ways to avoid the problem are both bad. **Serialise** — forward one request at a time
and wait — and a single slow handler stalls every other webhook, turning a 200ms endpoint
into a queue. **Open a connection per request** and you have thrown away the entire reason
the tunnel exists, because each new connection has to be dialled out from a laptop that may
be on hotel Wi-Fi.

**How does it actually work underneath?** Give every forwarded request an id, and keep a
map from that id to somewhere the answer can be delivered.

Concretely: the sender creates a channel, stores it in the map under a fresh id, writes the
request frame, and then blocks receiving on that channel. The connection has exactly one
reader goroutine; it reads a response frame, pulls the id out of it, looks up the channel,
and sends the response. The blocked sender wakes up holding its answer.

That map is touched by the reader goroutine and by every waiting sender at once, so it
needs a lock.

One level down, on the choice of lock. Go's idiom is "share memory by communicating", and
the communicating version exists: a single manager goroutine owning the map, with
registration and lookup arriving as messages on a channel. It works. It is also slower and
longer — every lookup becomes a round trip through another goroutine — for state that is
genuinely just a map with microsecond-long critical sections. A mutex is the right tool
here, and knowing *why* the idiom does not apply matters more than the idiom.

**What are the sharp edges?** Nearly all of the bugs in this pattern are the same bug
wearing different clothes: something was added to the map and never removed.

- **Every exit path must delete its entry.** Success, timeout, write failure, connection
  drop, panic. Miss one and each affected request leaks a map entry and a channel for the
  life of the process. `defer` the delete at the moment of registration, not at the end of
  the happy path.
- **A send to a channel nobody is reading blocks forever.** If a sender gave up at its
  deadline and walked away, the reader goroutine delivering that late response would block
  — and it is the *only* reader for the whole connection, so one abandoned response freezes
  every other request on that tunnel. A buffer of one fixes it: the send always completes,
  whether or not anyone is still listening.
- **A connection can die with requests in flight.** Nothing will ever arrive for them.
  Someone has to walk the map and wake everyone, or they each sit out their full deadline
  for an answer that provably cannot come.
- **Ids must be unique per connection.** A collision crosses two wires and hands one
  request's response to another.

**In hooklens:** a hub holds the connected tunnels, one per inbox, and each connection
holds its own correlation map. When a webhook is captured, ingest asks the hub to forward
it and waits with a deadline. Failure mode 3 is the deadline, failure mode 4 is waking
everybody when the tunnel drops, and failure mode 5 is what happens when a second CLI
claims an inbox that already has one.

## Decisions

**A mutex-guarded map, not a manager goroutine owning it.**
The "share by communicating" version is real and it works: one goroutine owns the map,
registration and lookup arrive as messages. It was rejected because every lookup becomes a
round trip through another goroutine, and the state here is a plain map with critical
sections measured in microseconds. The idiom exists to stop people sharing *complicated*
mutable state; this is the case it does not buy anything for.
*Wrong call if* the registry grew behaviour — expiry, quotas, ordered fairness — at which
point a goroutine that owns the state and can run timers inside it becomes the simpler
thing.

**A per-connection counter for request ids, not random hex.**
Ids only need to be unique within one connection, because the map they key is
per-connection. A counter guarantees that, costs one atomic add, and makes a log line
readable: `req_id=47` beats `req_id=9f3ac1…`. Guessability is irrelevant — the only party
who can see or send these ids has already authenticated as the inbox owner.

**The response channel is buffered with exactly one slot.**
Not a performance tweak; it is what stops one abandoned request freezing the whole tunnel.
If a caller hits its deadline and leaves, the reader goroutine delivering the late response
would block forever on an unbuffered channel — and there is exactly **one** reader for the
connection, so every other in-flight request on that tunnel stops too. With a buffer the
send always completes and the value is collected.

**`deliver` takes the entry out of the map and removes it under the same lock.**
That single choice makes a duplicate `req_id` harmless. A buggy or hostile client echoing
one id twice finds nothing the second time, so two sends can never compete for one buffer
slot — which would deadlock the reader, i.e. the same freeze as above by another route.

**A `done` channel closed once, rather than closing every pending channel.**
Closing the individual response channels would also wake the waiters, but a receive from a
closed channel yields a nil `*Response`, and the caller cannot tell "disconnected" from
"the CLI sent nothing". A separate `done` keeps the two distinguishable, and closing it is
O(1) regardless of how many requests are in flight.
This is what makes failure mode 4 *immediate*. Without it a dropped tunnel leaves every
caller waiting out its full deadline for an answer that provably cannot come — thirty
seconds of a provider holding a connection open, per in-flight request.

**`defer c.forget(id)` on the line after registration, not at the end of the happy path.**
The exit paths from `send` are: encode error, write error, response, disconnect, timeout.
Five. Attaching cleanup to the successful one leaks a channel and a map entry for every
request that takes any other, forever. This is the bug the curriculum bullet names, and the
countermeasure is placement, not vigilance.

**The deadline belongs to the caller.**
`Forward` takes a context and does not invent a timeout. Ingest knows how long a provider
is willing to wait; this package does not, and a default buried here would be found by
someone six months from now wondering why their forward dies at exactly 30s.

**`Forward` releases the hub lock before sending.**
Holding it for the duration of a round trip to somebody's laptop would serialise every
forward in the entire process behind one mutex. The lock protects the map, not the network.

**Newest connection wins — and `unregister` checks identity.**
Refusing the second connection is the obvious alternative and it is worse in the case that
actually happens: a laptop sleeps, the old socket is dead but unnoticed, and refusing locks
the developer out of their own inbox until an invisible timeout elapses.
The identity check in `unregister` is what makes newest-wins safe. When B replaces A, A's
deferred unregister still runs, and without `if h.clients[id] == c` it would delete **B** —
silently disconnecting the tunnel that just won, while a perfectly healthy CLI sat there
believing it was live. Pinned by a test that calls the sequence directly rather than racing
goroutines.

**Evicting the displaced client happens off the new connection's critical path.**
Found by a failing test, and it is the best bug of the unit — see Verified. `old.close()`
runs inline because waking waiters is instant and they should fail over at once; the socket
teardown goes in a goroutine because it can block.

**`closeWith` ends with `CloseNow`, not `Close`.**
`Close` performs the WebSocket closing handshake: it writes a close frame and then *waits
for the peer's*. The peer here is very often a machine that has already gone away, so that
wait is paid in full for nothing. We can afford to skip it precisely because of the unit 18
decision to send the reason in an application-level frame — the protocol close would add
only a status code.

**`tunnel.Header` rather than reusing `capture.Header`.**
The same reasoning that gave the store its own `headerJSON`, applied to a different
boundary: this shape is a contract with a **separately versioned program**. A CLI installed
months ago is still speaking it, so renaming a field on an internal domain type must not
silently change the wire. Four lines is a cheap price for the two moving independently.

**`Response.Error` is distinct from a 5xx status.**
`Error` means the CLI could not reach the local app at all — connection refused, DNS
failure, its own timeout. A 5xx means the app was reached and answered badly. Collapsing
them would tell a developer their application is broken when in fact it is not running,
which is the single most misleading thing this tool could say.

## Walkthrough

### `internal/tunnel/hub.go`

`Hub` (`:30`) holds `map[endpointID]*client` and nothing else. It is the only surface the
rest of the application touches — ingest asks it to forward and never learns that sockets
are involved.

`Forward` (`:44`) takes the lock, reads the client, releases it, *then* sends. The two
statements being separate is the decision above, not an accident of formatting.

`register` (`:73`) returns the displaced client instead of closing it, so the caller decides
when and how to evict. That is what let the eviction move off the critical path without
touching this function.

`unregister` (`:88`) — the identity check. Three lines, and deleting the condition
reintroduces a bug that presents as "the tunnel says connected but nothing is forwarded".

`client.send` (`:129`) is the pattern in full. Register, `defer forget`, write, then a
three-arm select: the response, the connection dying, the caller's deadline. Each arm is a
different error the caller reports differently.

The buffered channel at `:140` and the `defer` at `:155` are the two lines that keep this
from leaking; both carry comments saying so, because both look like style choices.

`deliver` (`:186`) does the take-and-delete under one lock. The `!ok` branch logs rather
than ignoring: a steady stream of unknown-id responses means the local app is consistently
slower than the deadline, which is worth being able to see.

`close` (`:219`) closes `done` **outside** the mutex and exactly once. Inside the mutex,
every woken goroutine would immediately contend for a lock it does not need; twice, and it
panics.

### `internal/tunnel/server.go`

`Handle` (`:169`–`:194`) is the connection's registration block. The deferred `cl.close()`
and `s.hub.unregister(ep, cl)` are declared *before* the register call, so they run however
the function exits — including the path where this connection is itself displaced later.

The eviction (`:176`) splits inline from asynchronous deliberately; the comment records the
bug that produced the split.

`readLoop` (`:374`) is now the correlation half: decode, then `cl.deliver`. The comment at
the top states the two independent reasons there is exactly one of these per connection —
the library forbids concurrent `Read`, and the design assumes a single reader.

`closeWith` (`:417`) — `CloseNow` and why.

## Verified

**24 tests in `internal/tunnel`**, green under `-race` and on repeated runs, still with no
database.

**The unit's central test is `TestForwardCorrelatesOutOfOrder`.** Twenty concurrent
requests are parked until all twenty have arrived, then answered in **reverse** order, each
carrying a body that names its own request. Every caller must receive its own answer. An
implementation that matched responses by arrival order would fail all twenty — and with
realistic payloads, which look alike, nobody would notice for months. It asserts the
correlation map is empty afterwards.

The failure modes, each with a test:

| # | failure | asserted |
|---|---|---|
| 1 | no tunnel connected | `ErrNoTunnel`, immediately |
| 3 | local app hangs | `ErrTimeout` at the caller's deadline, map left empty |
| 4 | tunnel drops mid-request | five in-flight callers get `ErrDisconnected` **in milliseconds**, against a 30s deadline |
| 5 | second CLI claims the inbox | the first gets a `replaced` close frame with a printable reason; the hub then routes to the second |

Failure mode 4's test is written so that a wrong implementation is slow rather than
merely incorrect: the callers' deadline is 30 seconds, so an implementation that let them
time out instead of waking them would take 30s to fail.

Three tests target specific bugs rather than features.
`TestUnregisterIgnoresStaleClient` calls register/register/unregister directly, pinning the
identity check without depending on goroutine timing.
`TestDuplicateResponseIsHarmless` has a client echo one `req_id` twice and asserts the
second delivery does not block.
`TestSendAfterCloseFails` asserts a closed client rejects new work rather than registering
an entry nobody will service.

### The bug worth remembering

`TestSecondClientWins` failed with **`ErrTimeout`, not `ErrNoTunnel`** — and that
distinction is the whole diagnosis. `ErrNoTunnel` would have meant the registry was wrong.
`ErrTimeout` meant the registry was *right*: the hub routed to the new client, the request
went out, and no response came back.

The cause was `closeWith` on the displaced connection. It ended in `Conn.Close`, which
performs the closing handshake and waits for the peer's close frame — and the peer being
displaced is, by definition, usually a machine that has gone away. So evicting it blocked
`Handle` before it reached `serve`, and the new tunnel was registered and visibly
"connected" while its read loop had not started.

Two things about this are worth carrying forward. It is a **bug that only exists in the
presence of a second connection**, so no amount of testing one happy tunnel would have
found it. And the production trigger — a laptop that slept, reconnecting — is not an edge
case, it is the single most common reason a second client ever appears.

The fix was two independent changes, each defensible alone: evict off the critical path,
and never wait on a closing handshake with a peer we have already written our reason to.
