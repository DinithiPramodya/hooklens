# 14 — In-process pub/sub: fan-out, slow consumers, backpressure

*Phase 2, unit 4. Covers the curriculum bullet "in-process pub/sub: channels, fan-out, slow
consumers, backpressure, and what happens when a browser tab stops reading".*

## Brief

**What is it?** A small piece of shared state that lets one part of a program announce
something happened without knowing who, if anyone, is listening. Publishers publish,
subscribers subscribe, and the broker handles the fan-out.

**What problem does it solve?** Without one, the capture handler would need a reference to
every open SSE stream and would write to each directly. That welds the thing receiving
webhooks to the thing displaying them: ingest would have to understand HTTP streaming,
track subscriber lifetimes, and cope with a slow reader. The broker is the seam that keeps
those concerns apart.

**How does it work underneath?** A Go channel is a queue with parking attached. `ch <- v`
on an unbuffered channel blocks the sender until a receiver takes the value. With a buffer
of N it blocks only once N values are already waiting. "Blocks" means the runtime parks
that goroutine — deschedules it, costing almost nothing — and wakes it when space appears.
Parking is cheap. But it is still blocking, and a blocked goroutine is not doing whatever
else it was supposed to.

Fan-out needs **one channel per subscriber**. You cannot share a single channel, because a
channel delivers each value to exactly one receiver — three subscribers reading one channel
would each get a different *subset* of events, not a copy of all of them. So the broker
keeps a set of channels and writes to each in turn.

That set is mutated from several goroutines at once: a new stream subscribes, a closing one
unsubscribes, and a capture publishes. So it needs a lock. A Go map is not safe for
concurrent use, and the runtime deliberately detects this and crashes with
`concurrent map writes` rather than corrupting itself quietly.

**The central problem is slow consumers.** What happens when a subscriber stops reading?
Browsers throttle background tabs. A paused debugger stops reading entirely. A slow network
backs up. There are four possible answers and you must pick one:

- **Unbuffered send** — the publisher blocks until the tab reads. A backgrounded tab now
  stalls the capture path while a webhook provider waits on that HTTP response.
  Unacceptable.
- **Buffered, block when full** — the same failure, postponed by the buffer size.
- **Buffered, drop when full** — the publisher never blocks. The slow subscriber misses
  events; nobody else is affected.
- **Buffered, disconnect when full** — also defensible: force that client to reconnect and
  refetch, so it resynchronises instead of quietly missing things.

**Backpressure** is the name for this whole question: when a producer outruns a consumer,
something has to give. Block, buffer, drop, or shed. The mistake is not making the choice,
because the default is "block" — and that is almost never what you wanted.

**Sharp edges.**

- **A send nobody will ever receive blocks forever**, which is a goroutine leak. `select`
  with a `default` case is the non-blocking send, and it is the core idiom here.
- **Only a sender should close a channel, and only once.** Closing twice panics; sending
  after close panics. With several publishers this becomes genuinely hard to get right,
  which shapes who owns teardown.
- **Unsubscribe must happen on every exit path**, including panics. That means `defer`.
- **Never hold a lock across something that can block.** If a publisher holds the mutex
  while doing a blocking send, and the subscriber needs that same mutex to unsubscribe, you
  have a deadlock. Either send non-blocking under the lock, or copy the subscriber list and
  send outside it.

**In hooklens.** Captures publish, SSE streams subscribe. A provider is waiting on the
capture response, so publishing must never block — non-blocking send, drop when the buffer
is full, and count the drops so the UI can tell the user it missed some and should refetch
rather than silently showing an incomplete list.

## Decisions

**Buffered channel, non-blocking send, drop when full.**
The four options from the brief, and why this one. A provider is waiting on the capture
response, so *blocking* is off the table: a backgrounded browser tab would stall webhook
delivery, which is the one thing this system must never do. *Disconnecting* the slow
subscriber is the other defensible answer and arguably better UX — the client reconnects
and refetches, so it resynchronises rather than carrying a gap. It was rejected for now
because it needs the client to handle a resync path that unit 15 has not designed yet;
dropping plus a visible counter gets the same honesty for less machinery.
*The signal to switch:* if drops turn out to be common rather than pathological, the
counter will say so, and at that point disconnect-and-resync is the better contract.

**Buffer of 32.**
Large enough to absorb a burst — a provider retrying a backlog can deliver dozens of
webhooks in a second, and a browser should not miss those merely for being mid-render.
Small enough that a dead tab holds a bounded, trivial amount of memory. There is no
principled value; the principled part is that it is *finite*, which is what makes the
non-blocking send meaningful.

**Drops are counted and reported, not swallowed.**
This is the decision that makes dropping acceptable. A viewer shown an incomplete list with
no indication is strictly worse off than one shown a gap — they cannot tell the difference
between "nothing arrived" and "I missed it", so they draw the wrong conclusion about their
own integration. The stream emits a `dropped` event and the UI says *missed N — reload to
resync*.

**One mutex for the whole broker, including the per-subscriber drop counter.**
`Subscriber.dropped` is guarded by the *broker's* lock rather than one of its own. Two
mutexes would introduce lock ordering — the beginning of a deadlock story — for no benefit,
since every write to that field already happens inside `Publish`, which holds the broker
lock anyway. The cost is that reading a counter takes the global lock; at this contention
level that is free.

**The send happens while holding the lock, and that is only safe because it cannot block.**
A blocking send under the lock would deadlock against `Unsubscribe` the instant one
subscriber's buffer filled: the publisher waits for a reader, and the reader waits for the
lock the publisher is holding. The alternative — copy the subscriber set, release the lock,
then send — is the standard workaround when sends *can* block. With `select`/`default` it
is unnecessary, and one fewer allocation per publish.

**`close` lives in `Unsubscribe`, under the same lock as `Publish`.**
That pairing is the invariant the package rests on: a send can never race a close, because
both need the mutex. Closing from the subscriber's own goroutine — the intuitive place,
since it owns the reading — would be a panic waiting for the first concurrent publish.

**Empty topic maps are deleted, not left behind.**
`delete(b.subs, topic)` when the last subscriber leaves. Without it every inbox ever
watched keeps an empty map alive for the life of the process. Individually tiny, unbounded
in the number of inboxes, and invisible until a memory graph slopes upward for no reason.
Asserted directly in `TestConcurrentSubscribeUnsubscribePublish`.

**The event is a summary, not the request.**
Id, method, path, sizes, timestamp — no body, no headers. Streaming up to a megabyte to
every open tab on every capture turns one large webhook into a fan-out problem, and the
list view does not need it. A client wanting bytes fetches them by id.

**`Subscribers(id) == 0` short-circuits before serialising.**
Most inboxes have no browser attached most of the time, so the common case skips the
`json.Marshal` entirely. It also keeps the publish path honest about where it sits: below
the durability line, doing as little as possible.

**Publishing sits below the durability line and cannot fail.**
`publish` returns nothing. That is deliberate: an error return invites a caller to turn it
into a non-2xx, which is precisely the Phase 0 quiz Q1 bug — a stored request reported as
undelivered, and duplicated on retry. The only honest response to a problem here is a log
line.

## Walkthrough

### `internal/broker/broker.go`

`buffer = 32` (`:17`) and `Message` (`:25`) — the message carries a pre-rendered string
rather than an `any`, so the payload is serialised once for all subscribers and this
package stays free of application types.

`Subscriber.C()` (`:46`) returns a **receive-only** channel. A caller therefore cannot send
on it or close it, which is not politeness: closing is the broker's job, and doing it twice
panics.

`Publish` (`:118`) is the whole design in eight lines. The `select`/`default` is the
non-blocking send; the `default` branch is the backpressure policy, written out. Delete the
`default` and this becomes a blocking send that deadlocks against `Unsubscribe` — the
comment above it says so, because that edit looks harmless.

`Unsubscribe` (`:88`) checks membership before deleting so a second call is a no-op rather
than a second `close` and a panic. Tested directly.

### `internal/ingest/ingest.go:150` — where publish sits

Immediately below the `---- the request is durable from here on ----` marker, beside
`TouchEndpoint`. The comment at the call site records the two properties that make it safe
there: it cannot block, and it cannot fail.

`publish` (`:183`) short-circuits on no subscribers before doing any work.

### `internal/server/sse.go` — the consuming side

`sub := s.broker.Subscribe(ep.ID)` with `defer s.broker.Unsubscribe(...)` immediately after
(`:146`). The defer is the whole lifetime story: without it, a client that vanishes leaves a
subscriber in the broker forever and every subsequent capture writes into a queue nobody
reads.

Subscribing happens *after* the `connected` event is written, so the client is already
listening before anything can be queued for it.

The `select` now has three arms. The `case m, ok := <-sub.C()` arm checks `ok` because the
channel closing is a legitimate shutdown path, not an error — it only happens via our own
deferred `Unsubscribe`.

The `dropped` reporting on the heartbeat tick (`:175`) uses a `reported` high-water mark so
each drop is announced once. Reporting on the tick rather than only on the next capture
means a client that has fallen behind learns about it even if traffic stops.

## Verified

**5 broker tests, 2 new server tests**, all passing, and the whole suite clean under
`-race` in WSL — the first unit where that was checked locally rather than in CI.

Broker coverage is the four things the brief says matter:

- **fan-out** — three subscribers each receive *every* message, and a fourth on another
  topic receives none
- **the publisher never blocks** — 320 messages into a subscriber that never reads,
  with a 5s failure deadline
- **drops are counted, and land on the right subscriber** — a slow reader loses exactly
  `sent - buffer`, its neighbour loses zero, and the survivor's queue head is the *oldest*
  message, confirming we drop arrivals rather than evicting history
- **concurrency** — four publishers and eight subscribers churning for 200ms, then
  asserting no topic maps are left behind

End to end against the running binary, with a stream open and three webhooks sent:

```
event: connected
data: {"inbox":"bmvzxqqak3wnwh7i6sgfuetclm"}

event: capture
data: {"body_size":42,...,"method":"POST","path":"/webhook","query":""}

event: capture
data: {"body_size":3,...,"method":"PUT","path":"/orders/42","query":"force=1"}

event: capture
data: {"body_size":2,...,"method":"POST","path":"/bin","query":""}
```

Live, in order, with no polling — and each one a summary, with the body deliberately absent.

### An incident worth recording

The first attempt at that demo showed only `connected` and no captures. The cause was not
the code: a **stale server from an earlier run still held port 8080**, so every request was
being answered by a binary built before the broker existed. The new process logged
`bind: Only one usage of each socket address` and exited.

Two things that made it quick to diagnose rather than baffling. The bind error named the
problem exactly. And the shutdown ordering from [10](10-background-workers.md) behaved as
designed — `sweeper stopped` then a clean exit — rather than hanging, which is precisely
what the derived-context fix in `run()` was written to prevent. A startup failure that
hangs instead of printing its reason would have turned a thirty-second problem into a long
one.
