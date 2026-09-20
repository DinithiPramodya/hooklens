# 13 — Server-Sent Events

*Phase 2, unit 3. Covers the curriculum bullet "SSE: the wire format, built-in reconnection,
and when to pick it over WebSocket or polling".*

## Brief

**What is it?** A one-way stream of text events from server to browser, carried inside an
ordinary HTTP response that simply never ends.

The server replies with `Content-Type: text/event-stream` and then keeps writing. The
connection stays open, and each event is a few lines of text followed by a blank line. That
is the entire protocol.

**What problem does it solve?** HTTP is request/response: the browser asks, the server
answers, done. But plenty of things need the *server* to speak first — a webhook just
arrived, and the page should show it.

Before SSE you had two options. **Polling**: ask every N seconds. Wasteful, and your latency
floor is the interval. **Long polling**: hold the request open until something happens,
answer, and have the client immediately reconnect. That works, and is genuinely how this was
done for years, but you pay a full connection setup per event and the reconnection logic is
yours to write.

SSE is the minimal fix: stop closing the response. Write many events into one connection.

**How does it work underneath?** The wire format is deliberately trivial — it is plain text,
and you can read it with `curl`:

```
event: capture
data: {"id":"abc123"}
id: 42

: this line is a comment, and it is how you keep an idle connection alive

data: an event with no name, delivered as "message"
```

`data:` carries the payload, and multiple `data:` lines are joined with newlines. `event:`
names it so the client can listen for that type specifically. `id:` sets the last-event id.
`retry:` tells the browser how long to wait before reconnecting. A line starting with `:` is
a comment — ignored by the parser, but it is still *traffic*, which is exactly what an idle
connection needs. Events are separated by a blank line.

On the browser side it is `new EventSource(url)`. And here is why SSE is worth choosing at
all: **the browser reconnects automatically**. Drop the connection and it retries on its
own, after whatever `retry:` you specified. It also sends back a `Last-Event-ID:` header
naming the last event it received, so the server can resume rather than restart. You get
both for free. With a WebSocket you write both yourself, every time.

**Sharp edges.**

- **You must flush, explicitly.** Go buffers response writes. Write a perfect event and
  nothing reaches the browser until the buffer fills or the handler returns — and an SSE
  handler never returns. Without a flush the feature simply does not work, and it looks like
  the server is not sending anything.
- **Idle connections get killed by everything in the middle.** Proxies, load balancers and
  NAT tables all reap quiet connections. Comment heartbeats are not a nicety.
- **A buffering proxy breaks SSE entirely.** nginx buffers responses by default and will
  hold your events until the stream ends, which is never. `X-Accel-Buffering: no` is the
  escape hatch — the same class of problem as flushing, one layer further out.
- **Browsers cap HTTP/1.1 connections at six per origin.** An SSE stream holds one open
  indefinitely, so a few tabs can starve the rest of the app. HTTP/2 multiplexes and mostly
  dissolves this, but in practice that means TLS.
- **Text only, one direction.** Binary payloads need encoding, and anything the client wants
  to *say* goes over a normal request.

**In hooklens.** The browser only ever receives — it acts through ordinary REST calls — so
SSE is the right shape, and the automatic reconnection matters for a page someone leaves
open on a second monitor all day. The tunnel in Phase 3 is genuinely bidirectional and will
use a WebSocket. Choosing per direction rather than by habit is the decision; this is the
half where the browser just listens.

## Decisions

**`fetch` + `ReadableStream` on the client, NOT `EventSource`.**
This is the unit's real decision, and it costs us the thing the brief praises.

`EventSource` cannot send request headers. There is no API for it and there never has been.
Our streams are authenticated with a bearer token like every other read, so EventSource
would have forced one of three things:

- **the token in the query string** — a long-lived secret in access logs, proxy logs and
  browser history, which is precisely what unit 08 refused;
- **a cookie** — which brings CSRF defences to a system that currently needs none;
- **a short-lived ticket endpoint** — a real pattern, but a whole second auth mechanism
  built to satisfy one browser API, and it needs the ticket to stay valid across
  reconnects, which makes it a bearer token in a URL again with extra steps.

So we keep the SSE *protocol* — text, one-directional, debuggable with `curl` — and give up
the EventSource *convenience*, reimplementing reconnection in about thirty lines. The auth
model stays uniform, which matters more than the thirty lines.
*Wrong call if* the reconnection logic ever grows beyond backoff — resumption via
`Last-Event-ID`, for instance, is something EventSource does for free and we would have to
write. *The signal to revisit* is needing resume semantics, at which point the ticket
endpoint earns its complexity.

**Backoff with jitter, not a fixed delay.**
`base * (0.5 + random/2)`, capped at 30s. Without jitter every open tab reconnects at the
same instant after a server restart — the thundering herd that turns a two-second blip into
a sustained one, because the stampede is what keeps the server down. The same reasoning
will apply to the tunnel client in Phase 3.

**Heartbeat interval as a `Server` field, not the constant.**
Purely so tests can drive it in milliseconds. A 20-second constant would mean either a
20-second test or no test, and "no test" is how heartbeats quietly stop working.

**`SetWriteDeadline(time.Time{})` per connection, not removing `WriteTimeout` globally.**
The server sets `WriteTimeout: 30s`, which is correct for ordinary responses and fatal for
a stream meant to live for hours — it would be cut at exactly 30 seconds, with no error the
client could tell apart from a network fault. Clearing the deadline for *this* connection
keeps Slowloris protection everywhere else. Removing the global timeout is the tempting and
wrong version. Pinned by `TestSSESurvivesWriteTimeout`, which runs a server with a 200ms
write timeout and asserts a ~750ms stream survives intact.

**`X-Accel-Buffering: no` even though nothing proxies us today.**
It is inert without nginx and it is the difference between working and silently broken
behind one. The failure mode — every event held until a stream that never ends, ends — is
close to undiagnosable if you have not met it before.

**The parser is a class with a buffer, not a function over a chunk.**
A chunk boundary can fall anywhere, including mid-field-name. A parser that assumed one
chunk is one event would work perfectly in development, where events are small and arrive
alone, and corrupt events under load. Pinned by a test that splits `event: capture` across
three chunks mid-word.

**Frontend tests on `node --test`, with no test-runner dependency.**
Node 24 strips TypeScript natively and ships a test runner, so nine parser tests cost one
line in `package.json` and zero packages. Vitest would be the conventional answer and buys
nothing we need here — the transport is covered end to end on the Go side, and the parser
is pure text in, events out.

## Walkthrough

### `internal/server/sse.go`

`sseWriter` exists as a type rather than a few `fmt.Fprintf` calls for one reason: **every
write must be followed by a flush**, and the flush lives inside `write` (`:113`) so it
cannot be forgotten. Forgetting it produces a stream that is perfectly correct on the
server and delivers nothing to the client.

`newSSEWriter` (`:36`) does the deadline clear *first*, before any header, because a
failure there means we should still be able to write a normal JSON error response. After
`w.WriteHeader(http.StatusOK)` that option is gone — which is why `handleStream` (`:119`)
has an explicit comment marking the line past which only logging is possible.

`event` (`:71`) splits the payload on newlines into several `data:` lines. This looks like
over-engineering until a pretty-printed JSON body arrives: a raw newline inside one `data:`
line ends the event early, so the payload would silently truncate at its first line break.

The `select` in `handleStream` (`:158`) watches `r.Context().Done()` for client
disconnection. That is the only reliable signal — a write to a dead connection can succeed
into a kernel buffer and report nothing wrong. `TestStreamStopsWhenClientLeaves` asserts the
goroutine count falls after the client goes away; without that branch every closed tab
would leak a goroutine for the life of the process.

### `internal/server/middleware.go` — `Unwrap`

The bug this unit found. `statusRecorder` wraps the `ResponseWriter` for every request.
`http.ResponseController` reaches optional interfaces — `Flusher`, `Hijacker`,
`SetWriteDeadline` — by walking an `Unwrap() http.ResponseWriter` method. A wrapper without
it is a wall, and the controller returns `errors.ErrUnsupported` for everything.

Measured before the fix, through our own middleware:

```
SetWriteDeadline: feature not supported
Flush:            feature not supported
```

So SSE was impossible, application-wide, and the failure mode is the worst available: the
handler runs, every write reports success, and the client receives nothing. Six lines of
method fix it, and `TestResponseControllerReachesThroughMiddleware` keeps it fixed.

This generalises well beyond SSE. Any middleware that wraps a `ResponseWriter` — for
logging, compression, metrics — silently disables flushing and hijacking unless it
implements `Unwrap`.

### `web/src/lib/sse.ts`

The long comment at the top is the EventSource decision, kept where someone will read it
while wondering why we hand-rolled something the platform provides.

`SSEParser.push` (`:52`) holds `buffer` across calls and only emits events terminated by a
blank line; anything after the last separator stays buffered. That single line of state is
the whole chunk-boundary correctness story.

`parseFrame` (`:70`) returns `null` for a frame with no `data:` lines, which is how comment
heartbeats are dropped before reaching the application.

`streamEvents` (`:117`) distinguishes a deliberate close from a failure by checking
`controller.signal.aborted` — without it, unmounting the component would log an error and
schedule a reconnect for a stream nobody wants.

### `web/src/App.tsx`

The `useEffect` returns `close` (`:70`). That cleanup is load-bearing: React StrictMode
deliberately double-mounts in development, so without it every mount leaks a live
connection, and the event list shows duplicates arriving from streams nobody can see.

## Verified

**16 Go tests in `internal/server`** (7 new) and **9 frontend tests**, all passing.

The stream, read with `curl` against the running binary:

```
$ curl -sN -H "Authorization: Bearer $TOK" .../api/endpoints/$SLUG/stream

retry: 3000

event: connected
data: {"inbox":"iye57lynv6tv3w7gqitl4wygqa"}

: keepalive
```

Headers: `Content-Type: text/event-stream`, `Cache-Control: no-store`,
`X-Accel-Buffering: no`. The `: keepalive` arrived 20 seconds in, which is the heartbeat
doing its job on an otherwise idle connection.

Go-side coverage:

- wire format byte-for-byte, including multi-line data and comment frames
- content actually **flushed before the handler returns** — the test reads four lines from a
  handler that is still blocked, and would hang without a flush
- a stream **survives a 200ms `WriteTimeout`** for ~750ms, proving the per-connection
  deadline clear
- 401 for no token and for a wrong token, and no upgrade to a stream in either case
- end to end: authenticated open, `connected` event, heartbeat
- the handler returns when the client disconnects, so goroutines do not leak

Parser coverage includes the case that matters most: `event: capture` split across three
chunks **mid-word**, reassembled correctly.

## Postscript: two things CI caught that local checks did not

Both were found by the first CI run after this unit, and both are worth keeping.

**1. The `.gitkeep` safety net was being deleted by every frontend build.**

Unit 11 committed `internal/webui/dist/.gitkeep` so that a clone with no npm build would
still compile. The mechanism is sound — verified directly: `//go:embed all:dist` with only
`.gitkeep` present does compile, and without it the error is
`cannot embed directory dist: contains no embeddable files`.

What was wrong is that the file did not survive. Vite's `emptyOutDir: true` wipes the
directory on every build, and a later `git add -A` faithfully recorded the deletion. So the
placeholder was removed from the repository by the very process it was meant to protect
against.

The fix is to put `.gitkeep` in `web/public/`, which Vite copies into `dist` *after*
emptying it. The placeholder is now recreated by every build rather than destroyed by one,
and the committed copy stays byte-identical so it never shows as modified.

Worth noting **why only CI saw it**: the lint job is the only job without a frontend build
step, so it is the only place the fresh-clone path is ever exercised. That was not
deliberate when it was written, and it is now the most valuable property that job has.

**2. `tsc -b` type-checks test files, and they import Node modules.**

`npm test` passed locally, so the test file was added and the unit moved on. `npm run build`
was never re-run, and it is `tsc -b` inside that script which fails:

```
src/lib/sse.test.ts(1,22): error TS2591: Cannot find name 'node:test'
```

The tempting fix is to add `"node"` to `tsconfig.app.json`'s `types`. That is wrong: it
makes `process.env` and `fs` typecheck as valid inside a React component, turning a build
error into a runtime one. Instead the tests get their own project — `tsconfig.test.json`
with `"types": ["node"]`, and `tsconfig.app.json` excludes `*.test.ts`. Verified by putting
`process.env.HOME` in browser code and confirming it still fails to compile.

One trap in that split: `extends` inherits `exclude`, so the test project initially excluded
exactly the files it existed to check (`TS18003: No inputs were found`). It needs
`"exclude": []` explicitly.

**The process lesson is the real one.** Running `npm test` and not `npm run build` after
adding a file is the same class of mistake as the `.gitignore` bug in Phase 0 — a local
check that passes because it exercises a different path than the one that ships. The
standing fix is the fresh-clone verification: `git archive` the index into a clean
directory and build *that*, which is what CI actually does.

**3. The race detector's first catch was in this unit's own test.**

`TestResponseControllerReachesThroughMiddleware` recorded two errors from inside the
handler and asserted on them afterwards:

```
WARNING: DATA RACE
Read at 0x00c00024f930 by goroutine 48:   sse_test.go:51   (the test)
Previous write by goroutine 52:            sse_test.go:36   (the handler)
```

The misconception is a common and specific one: **a completed `http.Get` does not mean the
handler has returned.** The client has the response the moment it is flushed; the handler
may still be executing. So reading variables the handler wrote, at that point, is a genuine
race — not a theoretical one.

The fix is a happens-before edge: `defer close(done)` at the end of the handler, and a
receive from `done` before the reads.

Worth sitting with the fact that this was only ever visible in CI. `go test -race` needs
cgo, cgo needs a C compiler, and the Windows development machine has neither — so every
local run was green. Phase 3 is goroutines around a shared map, where this stops being a
test-only concern.

Running the detector locally in a Linux container was attempted and abandoned: compiling
with `-race` over a Windows bind mount crashed Docker Desktop twice. The practical options
are CI, or `wsl --install -d Ubuntu` and running natively there.
