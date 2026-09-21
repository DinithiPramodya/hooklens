# 18 — WebSocket: an HTTP request that stops being HTTP

*Phase 3, unit 2. Covers the curriculum bullets "the WebSocket handshake as an HTTP
`Upgrade`; frames; why not plain HTTP" and "what an 'open connection' really is". Builds
directly on [17](17-nat-and-firewalls.md): NAT says the connection must be opened from the
laptop outward, and this note is about what that connection *is*.*

## Brief

**What is this thing?** A WebSocket is a long-lived, bidirectional, message-oriented
connection between two programs that **begins as an ordinary HTTP request and then stops
being HTTP**. After the handshake, the same TCP connection carries a different protocol.

**What problem does it exist to solve?** HTTP is request–response and client-initiated. The
server cannot speak first; it can only answer. For anything where the *server* has news —
a chat message, a stock tick, a webhook that needs forwarding — that is the wrong shape.

Before WebSocket, people faked it. **Polling**: ask every few seconds, and be wrong in both
directions at once — wasting requests when nothing happened and adding latency when it did.
**Long polling**: send a request and have the server hold it open until there is news, then
answer and have the client immediately re-request. That works, and it is still a reasonable
fallback, but every single message costs a full request cycle with a fresh set of headers,
and the connection churns constantly. **SSE** fixed half of it — the server can push freely
down one held-open response — but only half, because the channel is one-directional by
construction.

**How does it actually work underneath?** The handshake is a genuine HTTP `GET` that any
HTTP server or proxy can parse:

```
GET /tunnel HTTP/1.1
Host: hooklens.dev
Upgrade: websocket
Connection: Upgrade
Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==
Sec-WebSocket-Version: 13
```

The server answers `101 Switching Protocols` and echoes back
`Sec-WebSocket-Accept`, computed as base64(SHA-1(key + a fixed magic GUID from the spec)).
That computation is **not** security — the GUID is public and the key is sent in the clear.
It exists to prove the peer actually understood the handshake, rather than being a cache or
a naive proxy that replayed a stored 101 it did not comprehend.

After the 101, the TCP connection stays open and the bytes on it are **no longer HTTP**.
They are WebSocket frames: a compact binary header — a FIN bit, a 4-bit opcode, a mask bit,
a payload length encoded in 7, 16 or 64 bits, and an optional 4-byte masking key — followed
by the payload. Opcodes cover text, binary, close, ping and pong.

Client-to-server payloads must be XOR-**masked** with a random per-frame key. Again, not
secrecy: the key travels in the frame. It exists so that a malicious web page cannot craft
payload bytes that a confused intermediary proxy would misread as a second HTTP request —
a cache-poisoning attack that was demonstrated against earlier drafts.

One level down, this is all possible because **TCP is just an ordered stream of bytes with
no notion of a "request" at all**. HTTP is a convention layered on that stream. The upgrade
is nothing more than both sides agreeing to stop using that convention and start using
another one on the same stream. Nothing is reconnected; no new socket is opened.

**What are the sharp edges?**

- **Messages, not streams** — but a single message may be fragmented across several frames,
  and libraries differ in whether they hide that. Read at the message level or be careful.
- **No delivery guarantee beyond TCP's.** If the connection drops, in-flight messages are
  simply gone. Correlation, timeouts and retries are yours to build. This is most of
  Phase 3.
- **No automatic reconnection.** SSE specifies it; WebSocket does not. Every reconnect
  policy is hand-written.
- **Protocol-level ping/pong exists, but you usually cannot rely on it.** Browsers do not
  expose it to JavaScript, and intermediaries kill idle connections on their own schedule,
  so most real systems add an application-level keepalive anyway.
- **Intermediaries must be configured to pass `Upgrade` through.** Many proxies buffer,
  strip or time it out, and the symptom is a connection that works locally and dies in
  production.
- **Backpressure is easy to get wrong** in the same way it was for the broker in
  [14](14-pubsub.md): a peer that stops reading must not be able to stall the writer.

**In hooklens:** the tunnel is the one genuinely bidirectional channel in the system. The
server pushes `request` frames when a webhook lands; the CLI answers with `response` frames
carrying the status and body from the local app. SSE cannot do that, and the alternative —
SSE downward plus POSTs upward — means correlating two separate transports, which is
strictly more machinery than one connection carrying both directions.

The browser keeps SSE for the capture list, deliberately. It only ever receives, it gets
reconnection for free, and it is one less protocol to debug. Two transports, each chosen
for its own shape.

## Decisions

**`coder/websocket`, not `gorilla/websocket`.**
Measured rather than assumed, because the goose episode in Phase 0 taught that a
"small" library can drag in sixty modules: both pull **zero** dependencies, so weight was
not the tiebreaker after all. The tiebreaker is context. Every operation in
`coder/websocket` takes a `context.Context`; gorilla's API predates context and works in
`SetReadDeadline` terms. All of Phase 3 is deadlines and cancellation — failure mode 3 is a
per-request deadline, failure mode 4 is unblocking every pending handler the moment a
tunnel drops — so an API that already speaks that language composes directly instead of
needing a hand-written bridge, which is exactly where cancellation bugs breed.
*Wrong call if* we needed frame-level control, which is gorilla's remaining advantage. We
work at the message level.

**One JSON object per WebSocket message — a deliberate deviation from `PLAN.md`.**
The plan said "newline-delimited JSON frames over one WebSocket". That is redundant:
WebSocket is message-oriented, and a message boundary *is* a frame boundary — that is the
entire difference between it and a raw TCP stream. Layering NDJSON inside it means
implementing framing twice and inventing a way for the two layers to disagree about where
a frame ends. There is a test asserting no encoded frame contains a newline, so the plan's
version cannot creep back in by accident.

**Two-stage decode: envelope first, then payload.**
`Envelope{Type, json.RawMessage}` rather than one flat struct holding the union of every
frame's fields. The flat version is less code and is wrong in a specific way: it cannot
distinguish *absent* from *zero*, so a response frame claiming `status: 0` and one that
forgot to send a status decode identically. Reading the type first also means an unknown
frame type is rejected as an unknown type, rather than silently decoding into a struct full
of zero values.

**Authentication in the first frame, not an `Authorization` header.**
Following `PLAN.md`, and the tradeoff is real enough to write down. A header would let us
reject before upgrading, which is cheaper and matches the rest of the API. In-band auth
wins because it makes **every** failure the same shape: a `close` frame with a stable code
and a printable reason. With header auth, some failures are an HTTP status the CLI must
interpret separately, and the CLI's error handling forks in two.

The cost is that **the upgrade completes before anyone has authenticated**, which is a
security hole if left alone — see the next decision.

**A handshake deadline, and it is a security bound rather than a convenience.**
Between `Accept` and the hello there is a held-open connection belonging to nobody.
Without a deadline, opening thousands of connections and never speaking costs an attacker
nothing and costs us a file descriptor plus a goroutine each. Ten seconds, with the read
limit dropped to 4 KiB for the same window so a client cannot stream megabytes instead of
authenticating.

**The handshake deadline is a timer racing the read, NOT a context deadline on the read.**
This is the unit's real lesson and it cost a red test to learn. `coder/websocket` **closes
the connection when a read's context is cancelled.** So `context.WithTimeout` on the hello
read detects the timeout *and destroys the socket we wanted to explain ourselves on* — the
peer gets an unexplained EOF. The test that caught it asserts the close frame carries a
non-empty reason; without that assertion the bug would have shipped looking fine.

The same reasoning applies to the read loop, which is why it runs on
`context.WithoutCancel(ctx)`. Its exit condition is not a context but **the connection
closing**. Cancelling `ctx` ends the supervisor loop, the supervisor closes the connection,
and the closed connection ends the reader. One direction, no race.

**A process-lifetime context, separate from any request's.**
`http.Server.Shutdown` does not wait for — or even know about — hijacked connections, and a
WebSocket is a hijacked connection. Without a context that outlives individual requests, a
restart would drop every tunnel with no close frame and each CLI would find out only when
its next ping failed, up to 20 seconds later. This is why `server.New` now takes a `ctx`.

**`AuthFunc` declared in this package, not an interface over the store.**
The consumer defines the narrow contract it needs; `internal/tunnel` therefore has no
dependency on `internal/store` and its whole test suite runs on a four-line stub with no
database. The translation from `store.ErrUnauthorized` to `tunnel.ErrUnauthorized` happens
once, at the boundary in `internal/server`.

**One error for a bad token and an unknown slug.**
Carried over from [08](08-capability-urls.md) deliberately. Distinguishing them turns the
tunnel endpoint into an oracle for which inboxes exist. Verified on the wire: the two cases
produce byte-identical frames.

**Version checked before credentials.**
An old CLI that failed on authentication would send its owner hunting for a token problem
they do not have. The version error names both versions and says what to do.

**Close codes are strings, not numbers.**
They appear in logs and in CLI output, and `unauthorized` reads better than `4001` in both.

**An application-level close frame *and* a protocol close.**
The WebSocket close frame's reason is capped at 123 bytes and is mangled or dropped by
several intermediaries, so it cannot be relied on to explain anything. The app-level frame
has room for a sentence and a stable code the CLI can switch on; the protocol close that
follows is what actually ends the connection cleanly.

**Compression disabled.**
`permessage-deflate` holds a compression context per connection — real memory for every
idle tunnel — to save bytes on control frames that are already tiny. Revisit when large
bodies dominate the traffic, which is a Phase 3 question, not a Phase 2 one.

## Walkthrough

### `internal/tunnel/protocol.go`

`Envelope` (`:47`) and the two-stage decode; see the decision above for why the payload
stays raw.

`DecodeEnvelope` (`:117`) rejects a missing `type` explicitly rather than letting it
default to the empty string. Without that check, `{}` decodes cleanly into a zero envelope
and then matches no known type, producing "unexpected frame type" — which sends the reader
looking for a typo in a type name that was never sent.

`DecodePayload` (`:129`) rejects an empty payload for the same reason. A hello with no
payload would otherwise decode into an empty slug and token and be reported as an
*authentication* failure, which is a diagnosis pointing at the wrong thing entirely.

`ProtocolVersion` (`:21`) is checked, not merely recorded. A CLI installed months ago and
never updated is the normal case for a developer tool.

### `internal/tunnel/server.go`

`Handle` (`:112`) is the lifecycle. `defer c.CloseNow()` rather than `Close`: this runs on
every return including the panicking one, and `Close` performs a closing handshake that can
block on a peer which may already be gone.

The context at `:138` derives from `baseCtx`, not `r.Context()`. Once hijacked, the HTTP
server no longer manages the connection, so the request's context is the wrong lifetime.

`handshake` (`:155`) — the timer-versus-read race, and the comment above it explains the
failure it replaced. The goroutine's channel is buffered so it can always deliver its
result and exit even on the branch where nobody is left to receive; unbuffered, the timeout
path would leak a goroutine per abandoned connection, which is precisely the resource
exhaustion the deadline exists to prevent.

`serve` (`:261`) is a three-arm select: shutdown, the read loop ending, and the ping tick.
The read loop's channel is the connection's health signal — a peer that vanishes surfaces
there as a read error long before a ping is due.

The `Ping` call (`:303`) waits for the matching pong, so one call is both the keepalive
that stops an idle NAT row expiring ([17](17-nat-and-firewalls.md)) and the liveness check
that detects a peer which died without saying so.

`readLoop` (`:319`) must keep reading even though no frame type is actionable yet: **control
frames, including the pong `Ping` is waiting on, are only processed while a read is in
flight.** Stop reading and every ping times out on a perfectly healthy connection. This is
the least obvious line in the file and the comment says so.

`closeWith` (`:354`) builds its own short-deadline context with `context.WithoutCancel`,
because it runs on paths where the connection's context is already cancelled — and a close
frame written with a dead context never leaves.

### `internal/server/server.go`

`New` (`:59`) now takes the process context, and the comment says why. The `AuthFunc`
adapter (`:74`) is where `store.ErrUnauthorized` becomes `tunnel.ErrUnauthorized`, the one
place the two packages meet.

The route (`:152`) is `GET /api/tunnel`, registered before the `/api/` catch-all in
specificity, so a typo under `/api/` still gets JSON rather than the SPA.

## Verified

**14 tests in `internal/tunnel`**, passing under `-race` and on repeated runs, with no
database: the `AuthFunc` decision is what makes that possible.

Coverage is the close paths, because a tunnel that fails silently is useless:

| case | close code |
|---|---|
| bad token | `unauthorized` |
| unknown slug | `unauthorized` (identical frame — no existence oracle) |
| client one version behind | `unsupported_version` |
| first frame is not a hello | `malformed_frame` |
| not JSON / no `type` field / binary frame | `malformed_frame` |
| says nothing at all | `handshake_timeout` |
| process shutting down | `server_shutdown` |

Every one asserts a **non-empty reason**, which is the assertion that found the
context-cancellation bug.

Three tests exist for specific bugs rather than for features.
`TestHandshakeDeadlineDoesNotLeakIntoConnection` shortens the handshake timeout to 100ms
and then proves the connection is still alive 500ms later — guarding against the deadline
being reused for the serve loop, which would kill every healthy tunnel on a timer.
`TestPingKeepsConnectionAlive` survives roughly fifteen ping cycles.
`TestNoGoroutineLeak` opens twenty connections that vanish without a closing handshake —
the realistic case, since a laptop that sleeps does not say goodbye — and asserts the
goroutine count comes back.

**End to end against the real binary**, through the actual route rather than the package in
isolation:

```
<- {"type":"hello_ok","payload":{"slug":"5sts4x3x…","public_url":"http://5sts4x3x….localhost/","ping_seconds":20}}
   (still connected after 3s, idle as expected)

<- {"type":"close","payload":{"reason":"unknown inbox or bad token","code":"unauthorized"}}
<- {"type":"close","payload":{"reason":"this server speaks protocol version 1, your client speaks 0 — upgrade the CLI","code":"unsupported_version"}}
```

The server logged `tunnel connected` / `tunnel disconnected` with the inbox and endpoint id,
and the access log recorded `status=101` — which incidentally confirms the `statusRecorder`
from [13](13-server-sent-events.md) still reports correctly through a hijack.

### Two incidents worth recording

**The stale server, again.** The first end-to-end attempt returned 404 on `/api/tunnel`.
Not a routing bug: the binary from unit 16 still held port 8080, and it predates the route.
Same failure as unit 14, same tell — the new process logged `bind: Only one usage of each
socket address` and exited. Worth noticing what made it quick: the 404 came back as
**JSON**, not HTML, because of the `/api/` catch-all from [11](11-spa-and-go-embed.md). A
404 that arrived as a web page would have looked like a far stranger problem.

**The library closes the connection on read-context cancellation.** Described in the
decisions above. The general shape is worth carrying forward: *a cancellation mechanism
that also tears down the channel you need for your error message*. The countermeasure that
worked was not cleverness, it was asserting on the **content** of the failure — that the
close frame has a reason — rather than merely that a failure occurred.
