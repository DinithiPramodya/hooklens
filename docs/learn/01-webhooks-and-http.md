# 01 — Webhooks, and the HTTP underneath

*Phase 0, unit 1: the HTTP skeleton. Covers the first two curriculum bullets — webhooks
(polling vs. push, why the name) and HTTP from the bytes up.*

## Brief

**What is it?** A webhook is one server making an ordinary HTTP request to another server
to announce that something happened. That is the whole idea. There is no webhook protocol,
no webhook port, no webhook header. The word describes a *convention*, not a technology.

The name comes from "hook" in the old programming sense — a place you register your own
code to be called when something occurs — plus "web", because the registration is a URL and
the call is an HTTP request.

**What problem does it solve?** Before webhooks you polled. Your code asks Stripe every ten
seconds: anything new? Almost always the answer is no. Three things are bad about that:

- **Wasteful.** 99% of those calls return nothing, and both sides pay for all of them.
- **Slow.** An event that happens one second after your last poll waits nine seconds to be
  noticed. Your latency floor is your poll interval.
- **It scales backwards.** Ten thousand integrators each polling every ten seconds is a
  thousand requests per second of mostly nothing — a load problem the provider has to solve
  on behalf of everyone else's inefficiency.

Push inverts it: the party who *knows* something happened is the party who speaks. One
request per event, arriving in milliseconds.

**How does it actually work underneath?** One level down: a webhook arriving at your server
is these bytes on a TCP connection.

```
POST /webhook HTTP/1.1
Host: a7f3.hooklens.dev
Content-Type: application/json
Stripe-Signature: t=1756100000,v1=5257a869e7ec...
Content-Length: 42

{"type":"payment.succeeded","amount":4200}
```

A **request line** — method, target, version. Then **headers**, one `Name: Value` per line.
Then a **blank line** (`\r\n\r\n`), which is how the receiver knows the headers ended. Then
exactly `Content-Length` bytes of **body**. The receiver reads until the blank line, parses
the length, reads that many bytes, and stops. The response comes back the same shape, with
a status line (`HTTP/1.1 200 OK`) in place of the request line.

Notice what is *not* in those bytes: anything marking this as a webhook. It is
indistinguishable from a browser submitting a form. The only thing that makes it a webhook
is that you previously handed Stripe this URL and they agreed to POST to it.

**Sharp edges.**

- **The response is part of the protocol.** Return a non-2xx and the provider retries —
  Stripe for up to three days with backoff. Return slowly and they time out and retry
  anyway. So the handler must be fast, and it must tolerate receiving the same event twice,
  because it will.
- **Headers can repeat.** HTTP permits many headers with the same name. Go models this
  correctly as `map[string][]string`. Most people's mental model is `map[string]string`,
  and that model silently loses data.
- **The body is a byte stream, not an object.** It arrives as bytes and you *choose* to
  parse it. That distinction feels pedantic until Phase 4, where a signature is computed
  over the exact bytes, and any parse-then-reserialize — reordering one key, changing one
  space — breaks verification.
- **`Content-Length` can be absent.** Chunked transfer encoding sends the body in
  self-describing pieces with no length up front. Never trust the header for allocation.

**In hooklens.** Our capture handler is the most permissive HTTP handler you will ever
write: any method, any path, any headers, any body. We cannot validate what we do not know
is coming, and the entire product value is recording faithfully what actually arrived.
Everything downstream — the UI, the diff, the signature check — is only as truthful as
those bytes.

Deliberately **not** covered here: how `a7f3.hooklens.dev` becomes an IP address that
reaches our server. The `Host` header is HTTP and belongs in this note; DNS is unit 2.

## Decisions

**One binary serving both halves, split by `Host` header.**
Alternatives were two separate services behind a shared load balancer, or one service with
the capture endpoint on a path prefix instead of a subdomain. Chose one binary because it
is one deploy, one process to reason about, and — critically — it makes self-hosting a
single downloadable file, which is a product feature, not just convenience.
*This is the wrong call if* we ever need more than one replica: a tunnel held open by
instance A cannot be reached from instance B (Phase 3). *The signal to switch* is needing
horizontal scale; the fix is Postgres `LISTEN/NOTIFY` as a bus, or sticky routing by slug.
This is DECISIONS.md 01 and it is a limitation to document, not hide.

**`Resolve` is a pure function of three strings, not a method on a request.**
The alternative was inspecting `*http.Request` directly inside the handler. Chose a pure
function because routing is the one place where a mistake means a customer's webhook lands
in a stranger's inbox — and a pure function is exhaustively testable with no HTTP server,
no socket, and no database. It bought 40 table-driven cases for almost nothing.
See `internal/server/routing.go:46`.

**A path form (`/e/{slug}/...`) alongside the subdomain form, built on day one.**
The alternative was subdomains only, adding a fallback later if needed. Chose to build both
immediately for two reasons: local development has no wildcard DNS (Windows will not
resolve `*.localhost` at all), and PLAN.md names path routing as the documented fallback if
wildcard TLS issuance fights back. Building the fallback now costs about fifteen lines and
retires the phase's biggest risk before we hit it.
*Wrong call if* it became the primary form — the subdomain is nicer to paste into a
provider's config, and some providers reject URLs with paths.

**Slug validation is a whitelist, not a blacklist.**
Chose 3–32 chars of `[a-z0-9-]`, no leading or trailing hyphen, plus a reserved-names list.
The rejected alternative — blocking known-bad characters — is a bug waiting for whichever
character we forgot. This matters more than usual here: the `Host` header is entirely
attacker-controlled and arrives *before* any authentication, and it feeds a database lookup.
See `internal/server/routing.go:139`.

**Reserved names list is generous now rather than correct later.**
`api`, `www`, `admin`, `status`, `docs`, `cdn` and friends can never be inboxes. Growing
this list later is a breaking change for whoever already holds that inbox — so it gets
padded while there are zero users. Cheap now, impossible later.

**`/_hooklens/` reserved on inbox hosts; everything else captured.**
Something must answer health checks on an inbox host, because proving
`https://anything.hooklens.dev` serves a valid certificate is exactly how we verify wildcard
TLS in unit 2. But a provider is entitled to POST to any path, `/healthz` included — so
instead of stealing a plausible path, we reserve a namespace no real provider will use.
*This changed the plan:* PLAN.md's Phase 0 "done when" said
`curl https://anything.hooklens.dev/healthz`; the correct URL is
`/_hooklens/health`.

**The capture handler does not read the body yet.**
It would have been one line. Deliberately left out because reading an unbounded body from
an untrusted client is a real vulnerability, and it is the concept Phase 1 opens with
(`io.LimitReader`). Writing the naive version first would mean shipping it, however
briefly. See `internal/ingest/ingest.go:79`.

**Liveness health check does not touch Postgres.**
Alternative was checking dependencies in `/healthz`. Chose not to: a health check that
fails when a dependency is down invites the platform to kill and restart a process that is
working fine, turning a degraded service into an outage. Readiness/dependency checks get a
separate endpoint in Phase 5. See `internal/server/server.go:82`.

**`ReadHeaderTimeout`, not `ReadTimeout`, on the server.**
`ReadTimeout` caps headers *and* body together, and a legitimate provider on a slow link
may take a while over a large payload. Headers should always arrive promptly — a client
that opens a connection and dribbles headers forever is the Slowloris attack, and
`ReadHeaderTimeout` is the line that closes it. See `cmd/hooklens/main.go`.

**Config from environment variables only, validated at startup.**
No config file, no build tags. The same image runs in dev and prod, which is what lets the
artifact we test be the artifact we ship. Validated eagerly because a process that boots,
reports healthy, and then fails on the first real request is much worse to operate than one
that refuses to start with a clear reason. Defaults chosen so `go run ./cmd/hooklens` works
on a clean machine with nothing exported.

## Walkthrough

### `internal/config/config.go`

Three fields, all strings, all from the environment (`:15`). `Load` (`:33`) reads them with
defaults and then validates — the validation block is the load-bearing part, not the
reading. `env` (`:61`) treats an empty value as unset on purpose: orchestrators routinely
inject `""` for a variable that was never given a value, and an empty `BaseDomain` would
route every request to the app with no obvious cause.

### `internal/server/routing.go` — the heart of the unit

`Resolve` (`:46`) checks three forms in a fixed order, and **the order is load-bearing**:

1. The path form goes first because it must work *everywhere*, including on the base domain
   itself — where form 3 would otherwise claim the request and serve the app.
2. The app matches on the bare domain or its `www` alias.
3. The subdomain form uses `strings.CutSuffix(host, "."+base)`. The subtlety: for
   `a.b.hooklens.dev` this yields `"a.b"`, and `validSlug` rejects it for containing a dot.
   That one line is what stops nested subdomains from resolving, and it is not obvious from
   reading it. Change `CutSuffix` to `HasSuffix` plus a manual index and you will probably
   break it.

The fall-through at the end returns the app, not a 404. Bare IPs, stray CNAMEs someone
pointed at us, and the platform's internal health checker hitting the container directly
all land here. Capturing under an unrecognised host would silently invent inboxes for
anyone who pointed a DNS record at us.

`splitPathForm` (`:82`) has one case worth noting: `/e/a7f3` with no trailing slash returns
path `/`, matching `/e/a7f3/`. Without that branch the inbox root would be unreachable.

`normalizeHost` (`:100`) uses `net.SplitHostPort` and *ignores the error*, keeping the
original on failure. That looks sloppy and is not: `SplitHostPort` errors when there is no
port, which is the common case. The reason not to hand-roll it with `LastIndexByte(':')` is
IPv6 — `[::1]:8080` has four colons, and a naive split corrupts it. The stdlib already
knows about the brackets. The `TrimSuffix(host, ".")` after it handles the fully-qualified
form `hooklens.dev.`, which is legal in a `Host` header and is the same host.

`validSlug` (`:139`) iterates bytes, not runes. That is deliberate and it is what rejects
`café`: every character we permit is ASCII, so any multi-byte rune fails the switch. The
`reservedSlugs` check is last, on the return line.

### `internal/server/server.go`

`New` (`:28`) builds the middleware chain **once**, at construction, not per request
(`:38`). Order matters: `withRecover` wraps `withRequestLog`, so a panic still produces a
logged request line with a 500 rather than escaping the logger entirely.

`route` (`:49`) is the fork. The load-bearing line is `r.Clone(...)` (`:63`) rather than
mutating `r.URL.Path` in place. `r.Clone` deep-copies the URL, so the rewrite cannot be
observed by anything still holding the original — the same reason `http.StripPrefix` clones.
Mutating a request in flight is the class of bug that only appears once something else
reads it concurrently, which is exactly what Phase 3 will add.

`appRoutes` (`:69`) uses Go 1.22 routing patterns. `GET /healthz` puts the method *in the
pattern*, so a POST gets a 405 from the mux itself with no handler code — verified in the
smoke test. `GET /{$}` means **exactly** `/`; without the `{$}`, `/` is a prefix pattern
that matches every otherwise-unmatched path, which is almost never what you want and is a
common source of "why is my 404 page returning 200".

### `internal/server/middleware.go`

`statusRecorder` (`:21`) exists because `net/http` gives no way to ask a `ResponseWriter`
what status it sent — logging middleware has to record it on the way past. The zero value
being `0` rather than `200` is used deliberately: it distinguishes "the handler called
`WriteHeader(200)`" from "the handler never wrote anything". Both branches (`:27`, `:34`)
guard with `if rec.status == 0` so the *first* write wins, and `Write` (`:34`) sets 200
itself because a handler that writes without calling `WriteHeader` gets an implicit 200 —
mirror that or such responses log as status 0.

`withRecover` (`:83`) has one line that looks redundant and is not: re-panicking on
`http.ErrAbortHandler` (`:93`). That sentinel is the documented way for a handler to
abandon a response deliberately; swallowing it would log a normal control-flow event as a
crash. The `http.Error` at the end is a no-op with a "superfluous WriteHeader" warning if
the handler already wrote — which is the correct outcome: the client has *a* response, just
not the one we wanted.

Worth knowing: `net/http` already recovers panics per connection, so a panic will not kill
the process. What it does without this middleware is close the connection with no response
at all, which the client sees as a *network* error rather than a server error.

`newRequestID` (`:115`) uses `crypto/rand`, not because request IDs are secret but because
since Go 1.24 `crypto/rand.Read` cannot fail — it is the one with no error path to handle.

### `internal/ingest/ingest.go`

`slugKey struct{}` (`:26`) is an unexported zero-size type used as the context key. This is
the standard Go idiom and the reason context keys are never plain strings: a `string` key
can collide with another package's key of the same value, silently. An unexported type
cannot be constructed outside this package, so collision is impossible.

`ServeHTTP` (`:48`) checks the reserved prefix first, then captures everything else. That
ordering is the entire access-control model of this handler.

`capture` (`:79`) is a Phase 0 stub and says so in its response body. It logs and returns
200. See the decision above for why it does not read the body.

### `cmd/hooklens/main.go`

`main` does nothing but call `run` and translate an error into an exit code. Calling
`os.Exit` from deeper code would skip every deferred function on the way out; keeping the
exit in one place lets everything else just return an error.

`signal.NotifyContext` gives a context cancelled on SIGINT or SIGTERM. **SIGTERM is the one
that matters** — it is what Docker, Fly and Kubernetes send first, followed by SIGKILL after
a grace period. Handling it is the difference between finishing the captures in flight and
dropping them.

`ListenAndServe` blocks, so it runs in a goroutine reporting over a **buffered** channel.
Buffered matters: with an unbuffered channel, if the `select` below has already taken the
`ctx.Done()` branch, nobody is left to receive and the goroutine blocks forever on send —
a leak.

The `select` has two branches. On `errCh`, the server stopped by itself (port taken, socket
died); `ErrServerClosed` can only appear after a `Shutdown`, which cannot have happened on
that branch, so any error there is real. On `ctx.Done()`, we drain — and the load-bearing
detail is `context.Background()` for the shutdown context, **not** `ctx`. `ctx` is already
cancelled; that is why we are in this branch. Passing it to `Shutdown` would abort the
drain instantly and defeat the entire mechanism.

## Verified

`gofmt` clean · `go vet` clean · `go build ./...` · 40 routing cases pass.
Smoke-tested live against `go run`, all six forms:

| Request | Result |
|---|---|
| `GET localhost:8080/healthz` | `200 {"status":"ok"}` |
| `GET localhost:8080/` | `200` app placeholder |
| `POST localhost:8080/e/a7f3/webhook` | captured, inbox `a7f3`, path rewritten to `/webhook` |
| `POST /webhook` with `Host: b8c2.localhost` | captured, inbox `b8c2` |
| `GET /_hooklens/health` with `Host: b8c2.localhost` | `200`, reports its inbox |
| `GET /` with `Host: api.localhost` | app — reserved name, **not** captured |
| `POST localhost:8080/healthz` | `405` from the mux |

`go test -race` could not run locally: it requires cgo and there is no C compiler on this
machine. CI runs it on Linux.
