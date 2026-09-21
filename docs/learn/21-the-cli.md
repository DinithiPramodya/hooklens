# 21 — The CLI: turning a frame back into an HTTP request

*Phase 3, unit 5. The client half of everything built in units 18–20: `hooklens forward
--to localhost:3000`. Failure mode 2 is implemented here, from the side that can actually
observe it.*

## Brief

**What is this thing?** A program that runs on the developer's laptop, holds a WebSocket
open to the hooklens server, and for every `request` frame that arrives makes a real HTTP
request to a local address and sends the answer back as a `response` frame.

**What problem does it exist to solve?** Everything so far has a hole in the middle. The
server can capture a webhook and can hand it to a tunnel, but nothing has ever been on the
other end of that tunnel. This is the piece that makes the URL genuinely point at the
developer's machine — the thing the whole project claims to do.

**How does it actually work underneath?** The client is a **reverse proxy with an unusual
transport**. A normal reverse proxy receives an HTTP request on a socket and re-issues it
to a backend. This one receives a JSON frame on a WebSocket, rebuilds an HTTP request from
its fields, issues it, then flattens the response back into a frame. The middle step —
`http.Client` against `http://localhost:3000` — is the boring part; the two translations
around it are where the mistakes live.

Rebuilding is not just setting method and URL. The captured request carries headers that
described *its* journey to our server, and some of them are now lies. `Host` named our
domain, not the laptop. `Content-Length` will be recomputed. Hop-by-hop headers described a
connection that ended at our server.

The credentials are the other half. The URL must survive a restart — a tunnel whose address
changes every time you run it is worthless for pasting into a provider's dashboard — so the
slug and token are written to a config file on disk, which makes file permissions part of
the design rather than an afterthought.

**What are the sharp edges?**

- **The `Host` header.** Frameworks route on it, and several reject requests whose `Host`
  they do not recognise — Django's `ALLOWED_HOSTS`, Rails' host authorisation. Forwarding
  the provider's `Host` verbatim gets you a 400 from your own app for reasons that look
  insane. Rewriting it loses information the app might legitimately want, so the original
  belongs in `X-Forwarded-Host`.
- **Redirects must not be followed.** Go's `http.Client` follows them by default. A 302 is
  a real answer that the provider should see; following it silently replaces the
  developer's response with whatever was at the other end.
- **Transparent compression.** Go's transport adds `Accept-Encoding: gzip` and
  decompresses the reply without telling you, so the bytes you relay are not the bytes the
  app sent, while the `Content-Encoding` header still claims they are.
- **Connection refused is not a 502.** The client must report "I could not reach it" as
  distinct from any status, because the server relies on that distinction — see
  [20](20-forwarding.md).
- **A local app is not the internet.** Nothing is retried, the timeout is short, and the
  target is usually plaintext HTTP on a loopback address.

**In hooklens:** `hooklens forward --to localhost:3000` reads or creates a config file
under the platform's user config directory, connects, prints the public URL, and serves frames until interrupted. A
failure to reach the local app becomes `Response.Error`, which the server turns into a
recorded `unreachable` rather than a 502 at the provider.

Two things are deliberately missing and arrive in unit 22: **reconnection** — losing the
connection currently ends the process — and a **bound on concurrent forwards**, which is
the same gap already recorded on the server side in [20](20-forwarding.md).

## Decisions

**The client lives in `internal/tunnel`, beside the server.**
The two halves speak one protocol, and keeping them in one package means the frame types,
`Encode`/`Decode` and the read limit have exactly one definition. Splitting them would
create the classic distributed-systems bug where a field is renamed on one side only —
which is precisely what the protocol version check exists to catch, and better not to need.
*Wrong call if* the CLI ever ships as a separate module with its own release cadence. It
does not; it is the same binary under a different subcommand.

**`forward` is dispatched before `config.Load()`.**
The client runs on a developer's laptop and talks to a remote server. Requiring a sensible
`DATABASE_URL` before it will start is nonsense, and it would surface as a confusing
startup error on a machine that has no database and should not need one.

**The `Host` header is rewritten to the target, and the original preserved.**
The sharp edge from the brief, made concrete. Frameworks validate `Host` — Django's
`ALLOWED_HOSTS`, Rails' host authorisation — so forwarding the provider's value verbatim
produces a **400 from the developer's own app** for a reason that looks insane. Rewriting
alone would discard information a multi-tenant app might legitimately want, so the original
goes to `X-Forwarded-Host`.

**Redirects are not followed.**
`http.Client` follows them by default, which would silently replace the developer's 302
with whatever is at the other end. A 302 is a real answer and the provider should see it.
One line, `CheckRedirect: ErrUseLastResponse`, and the absence of that line is invisible
until someone wonders why their redirect never reaches the provider.

**Compression is disabled on the transport.**
Go otherwise adds `Accept-Encoding: gzip` on its own initiative and decompresses the reply
without telling you — so the relayed bytes would not match the `Content-Encoding` header
travelling with them. Disabling it means whatever the provider asked for is passed straight
through.

**`Content-Length` is dropped on the way in, hop-by-hop headers on both ways.**
`Content-Length` described the body that reached *our server*; `net/http` computes a
correct one for the request actually being sent, and copying the original produces a
truncated or hanging request. Hop-by-hop headers described a connection that ended at our
server.

**`callLocal` never returns an error.**
A failure to reach the app *is* the answer, carried in `Response.Error`. The server depends
on that being distinguishable from a status — see [20](20-forwarding.md) — and a Go error
return here would invite a caller to conflate the two.

**The client's own timeout is 25s, under the server's 30s.**
Whoever gives up first decides what gets recorded, and only the client can say something
useful. "Your app did not answer in 25 seconds" points at the developer's handler; the
server's "the tunnel did not answer" is true and points at the wrong thing.

**`parseTarget` detects a missing scheme rather than trusting `url.Parse`.**
`localhost:3000` is the form people type, and `url.Parse` reads it as scheme `localhost`
with opaque path `3000` — entirely valid, and wrong in a way that produces a baffling
error much later. Prefixing `http://` when there is no `://` is the difference between
working and failing confusingly.

**`cleanDialError` keeps the last clause.**
Go's error is `Get "http://localhost:3000/hook": dial tcp 127.0.0.1:3000: connect:
connection refused`. It is accurate and nobody reads past the first clause. The actionable
part is `connection refused`, and that is what the developer sees in the UI.

**The config is keyed by server URL, not a single saved inbox.**
Someone running a local hooklens for development and a hosted one for real work has two
unrelated inboxes, and the second should not evict the first.

**`os.UserConfigDir()`, not a hardcoded `~/.hooklens`.**
It is `%AppData%` on Windows and honours `XDG_CONFIG_HOME` on Linux, so the file lands
where each platform's tooling expects. This project is developed on Windows and will be
demonstrated on Linux; hardcoding the Unix path would have worked on exactly one of them.

**Mode 0600, and a write-then-rename.**
The file holds tokens granting full access to an inbox's captured traffic, which routinely
contains other people's secrets. Permissions are part of the design, not an afterthought.
The rename is atomic within a directory, so a crash mid-write leaves the old file intact —
and half-written here means an **unrecoverable** token, because the server stores only its
hash.

**A corrupt config is fatal rather than silently replaced.**
Starting fresh would overwrite the token, and the token cannot be recovered. Better to stop
and say so.

**The inbox is saved to disk BEFORE connecting.**
A token that exists on the server but not on disk is lost forever. The ordering is not
stylistic.

**One goroutine per request, unbounded.**
Handling requests serially would let one slow local handler stall every other webhook —
the same serialisation problem unit 19 avoided on the server. Unbounded is the gap already
recorded in [20](20-forwarding.md); it is closed in unit 22 rather than half-solved here.

**Reconnection is deliberately absent.**
A dropped connection ends the process, which prints why and exits non-zero. That is honest
behaviour — the alternative, a CLI that silently sits there looking connected while
delivering nothing, is worse than one that stops. Backoff and jitter get taught properly in
unit 22.

## Walkthrough

### `internal/tunnel/client.go`

`NewClient` (`:53`) builds the `http.Client`, and all three of its unusual settings are
defences: `CheckRedirect` (`:71`), `DisableCompression` (`:80`), and `Timeout` (`:67`).

`parseTarget` (`:96`) — the missing-scheme detection and why.

`Run` (`:115`) dials, raises the read limit to match the server's, handshakes, then serves.

`handshake` (`:145`) has a `TypeClose` arm (`:176`) that prints the server's reason
verbatim. This is the payoff for unit 18's decision to send an application-level close
frame: the user sees "this server speaks protocol version 1, your client speaks 0 —
upgrade the CLI" rather than a socket error.

`serve` (`:196`) is the single read loop, for the same two reasons the server has one.

`callLocal` (`:273`) is the translation. The `Host` case (`:291`), the `Content-Length`
case (`:298`), and the hop-by-hop skip are the three header decisions, each commented at
the point of the decision rather than in a block at the top.

The `c.http.Do` error path (`:311`) returns `Response{Error: ...}` with **no status** —
the property the whole delivery-vs-application distinction rests on.

### `cmd/hooklens/forward.go`

`saveConfig` (`:81`) — temp file plus rename, and the permission constants above it.

`runForward` (`:143`) reads the config, creates an inbox only if there is not one for this
server, saves before connecting, and prints the banner from `OnConnect` so the URL appears
the moment it is true rather than after `Run` returns.

### `cmd/hooklens/main.go`

The `forward` branch (`:43`) sits above `config.Load()`, with its own signal context.

## Verified

**Ten unit tests** for the translation layer, with no WebSocket involved — `callLocal` is
tested directly against an `httptest` server, which is what makes each header decision
independently assertable.

Notably: the signature header survives (Phase 4 depends on that), `Host` becomes the target
while the original appears in `X-Forwarded-Host`, `Connection` and the original
`Content-Length` do not reach the app, duplicate `Set-Cookie` headers survive the return
trip, a 302 is relayed rather than followed, the transport adds no `Accept-Encoding`, a
dead port yields an `Error` with **status 0**, and a hung app produces an `Error` rather
than a hang.

**Then the whole product, on this machine.** A real hooklens server, a real local app on
`:3000`, the real CLI, and `curl` standing in for a provider.

```
  forwarding  http://632xap2u4zmm64zx3oqnxyhlzq.localhost/  ->  localhost:3000
```

A normal webhook:

```
HTTP/1.1 201 Created
Content-Type: application/json
Set-Cookie: session=abc
Set-Cookie: flavour=chocolate
X-Hooklens-Forward: delivered

{"handled":true,"path":"/hooks/stripe","bytes":28}
```

That is the local app's status, its two cookies and its body, at the provider. And what the
local app saw:

```
[local app] POST /hooks/stripe?live=1  sig="t=1,v1=deadbeef"
            host="localhost:3000"  fwd-host="localhost:8080"  body={"id":"evt_1","amount":2500}
```

Signature intact, `Host` rewritten, original preserved.

An application error relays unchanged — `500` and `kaboom: my handler is broken`. A
redirect relays as `302` with its `Location`, and the target was never hit.

**Failure mode 2, with the local app killed and the tunnel still up:**

```
HTTP/1.1 200 OK
X-Hooklens-Forward: unreachable
```

200, not 502 — the provider is not told to retry an event we already hold.

**The URL survives a restart.** Both processes stopped and restarted; the CLI printed the
same slug with no "created a new inbox" line, having read it from
`%AppData%/hooklens/config.json`. That is the half of failure mode 6 that makes the URL
worth pasting into a provider's dashboard.

**And the history reads correctly through the API**, with `forward_status` and
`forward_error` mutually exclusive exactly as the 00006 constraint requires:

| capture | recorded |
|---|---|
| `/back-up` | `forward_status: 201`, `forward_ms: 28` |
| `/hooks/stripe` (app down) | `forward_error: unreachable`, `forward_ms: 19` |
| `/redirect` | `forward_status: 302` |
| `/boom` | `forward_status: 500` |
| `/hooks/stripe` | `forward_status: 201`, `forward_ms: 16` |

One thing this exposed: `summarise` in `internal/server` was not returning the forward
columns at all, so the data recorded in unit 20 was invisible to any client. Added, with
the fields **omitted** rather than null when forwarding was never attempted — absent and
null mean different things, and a zero status would look like a real one.
