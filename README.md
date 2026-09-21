# hooklens

A webhook inspector with a self-hosted tunnel. It gives you a public URL, captures every
request sent to it byte-for-byte, streams them to a web UI, and forwards them to code
running on your laptop. Replay, diff, and provider-signature verification on top.

> **Status: Phase 4 of 6 complete — the wedge features are in.** Requests are captured, stored
> byte-for-byte, authenticated with per-inbox tokens, paginated by cursor, and expired on a
> retention schedule. The frontend is compiled into the binary and shows captures arriving
> live over SSE, with a detail pane that renders JSON as a collapsible tree, headers with
> duplicates intact, raw text, or a hex dump for binary. The tunnel protocol now has an
> authenticated WebSocket handshake, and `hooklens forward --to localhost:3000` delivers
> captures to a local app, relays its response back to the sender, and reconnects with
> exponential backoff and jitter on the same URL. In-flight forwards are bounded per
> tunnel, oversized bodies are refused rather than silently truncated, and the UI shows
> whether each capture actually reached your app. Captures can be replayed (to the tunnel
> or a URL), edited and replayed, diffed structurally, and checked against Stripe, GitHub,
> Shopify and Slack signatures -- showing the exact bytes that were signed. Hardening and
> release are Phase 5.
> See [PLAN.md](PLAN.md) for the full build plan.

## Requirements

- Go (version is pinned in `go.mod`)
- Docker Desktop — on Windows this needs WSL2, so `wsl --install` and a reboot first
- Node.js 24+ — the frontend is compiled into the binary, so `go build` needs it built first

## Run it

```sh
cd web && npm install && npm run build && cd ..   # frontend, embedded at compile time
docker compose up -d --wait     # Postgres
go run ./cmd/hooklens migrate up
go run ./cmd/hooklens            # serves on :8080
```

Then, in another terminal:

```sh
# create an inbox -- the token is shown ONCE and stored only as a hash
curl -sX POST localhost:8080/api/endpoints -d "{\"name\":\"demo\"}"

curl localhost:8080/healthz

# capture into inbox "a7f3", two equivalent ways
curl -X POST localhost:8080/e/$SLUG/webhook -d "{\"hello\":\"world\"}"
curl -X POST localhost:8080/webhook -H "Host: $SLUG.localhost" -d "{\"hello\":\"world\"}"

# reading needs the token; capturing never does
curl -H "Authorization: Bearer $TOKEN" localhost:8080/api/endpoints/$SLUG/requests
```

Both forms reach the same inbox. The subdomain form is what a provider will use in
production; the `/e/{slug}/` form works anywhere and needs no wildcard DNS, which is why
local development uses it.

## Commands

| | |
|---|---|
| `go run ./cmd/hooklens` | Serve. Also `serve` explicitly. |
| `go run ./cmd/hooklens migrate up` | Apply pending migrations |
| `go run ./cmd/hooklens migrate down` | Reverse one migration |
| `go run ./cmd/hooklens migrate status` | Show applied and pending |
| `go run ./cmd/hooklens forward --to localhost:3000` | Tunnel captures to a local app |

Migrations are embedded in the binary, so `hooklens migrate up` works on a machine holding
nothing but the executable.

### Forwarding to a local app

```
$ hooklens forward --to localhost:3000

  forwarding  http://632xap2u4zmm64zx3oqnxyhlzq.localhost/  ->  localhost:3000
  inbox       632xap2u4zmm64zx3oqnxyhlzq
  inspect     http://localhost:8080/
```

Anything sent to that URL is captured, then delivered to `localhost:3000`, and the local
app's response — status, headers and body — goes back to the sender. The inbox is saved
under the platform's user config directory (mode 0600, since it holds a token), so the
**URL is the same every run**; pass `--new` to mint a fresh one, and `--server` or
`HOOKLENS_SERVER` to point at a hosted instance.

If the local app is not running, the sender still gets a 2xx and the capture is stored
with `forward_error: unreachable` — a delivery failure must not make a provider retry or
disable the endpoint.

## Configuration

All from the environment, with defaults that work on a clean machine.

| Variable | Default | |
|---|---|---|
| `HOOKLENS_ENV` | `dev` | `dev` or `prod`; selects text or JSON logs |
| `HOOKLENS_ADDR` | `:8080` | Listen address |
| `HOOKLENS_BASE_DOMAIN` | `localhost` | Domain the app is served from; one label in front of it is an inbox |
| `DATABASE_URL` | matches `compose.yaml` | Postgres connection string |

## Checks

```sh
gofmt -l .
go vet ./...
go test ./... -count=1
cd web && npm test && cd ..   # frontend: node --test, no runner dependency
```

### The load test

Behind a build tag, so it never taxes `go test ./...` and never runs in CI -- a
shared runner under an unknown neighbour's load produces a throughput number that
means nothing.

```sh
go test -tags loadtest -run TestLoad -timeout 5m ./internal/server/ -v
```

Defaults to 1,000 req/s for 60 seconds; override with `HOOKLENS_LOAD_RATE`,
`HOOKLENS_LOAD_SECONDS`, `HOOKLENS_LOAD_WORKERS`. It asserts zero dropped captures,
which here means the server's 2xx count, the `hooklens_captures_total` metric and the
row count in Postgres are all equal. See
[docs/learn/33-load-testing.md](docs/learn/33-load-testing.md) for why that phrase
needed five definitions, and for the measured throughput on this hardware.

Two database diagnostics live alongside it, for when the answer is "something is
slow" and the next step is a measurement rather than a guess:

```sh
go test -tags loadtest -run 'TestInsertThroughput|TestCaptureChainThroughput' \
  ./internal/store/ -v
```

### The race detector

`go test -race` needs cgo and therefore a C compiler, which Windows does not have by
default. It runs in CI on Linux, and locally through WSL:

```sh
wsl -d Ubuntu -u root -- bash -lc \
  "cd /mnt/d/Projects/webhook-inspector && \
   DATABASE_URL=postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable \
   GOFLAGS=-buildvcs=false go test ./... -race -count=1"
```

About 12 seconds with a warm build cache. Worth having before touching anything
concurrent -- every race found on this project so far was invisible to a non-race run.

One-time setup inside Ubuntu (`wsl --install -d Ubuntu`):

```sh
apt-get install -y gcc libc6-dev curl      # libc6-dev is easy to miss; gcc alone
                                           # fails with "stdlib.h: No such file"
curl -sSL https://go.dev/dl/goX.Y.Z.linux-amd64.tar.gz -o /tmp/go.tgz
tar -C /usr/local -xzf /tmp/go.tgz && ln -sf /usr/local/go/bin/go /usr/local/bin/go
```

Running it in a Docker container instead was tried and abandoned: compiling with
`-race` over a Windows bind mount crashed Docker Desktop twice.

## Layout

```
cmd/hooklens/      main, subcommand dispatch, migrate runner
internal/config/   environment configuration
internal/server/   host-based routing, middleware, app routes
internal/ingest/   the capture endpoint
internal/broker/   in-process pub-sub, captures to open streams
internal/webui/    the built frontend, embedded via go:embed
web/               frontend source (Vite + React + TypeScript)
migrations/        SQL migrations, embedded via go:embed
docs/learn/        how each piece works and why it was built this way
```

## Docs

[`docs/learn/`](docs/learn/) explains every piece: the concept, the alternatives that were
rejected, and a walkthrough of the code. Written as it was built, in build order.

- [01 — Webhooks, and the HTTP underneath](docs/learn/01-webhooks-and-http.md)
- [02 — Containers, images, and Compose](docs/learn/02-containers.md)
- [03 — Schema migrations](docs/learn/03-migrations.md)
- [04 — Continuous integration](docs/learn/04-ci.md)
- [05 — DNS, and the certificates that ride on it](docs/learn/05-dns-and-tls.md)
- [06 — Reading a request body, and why the raw bytes matter](docs/learn/06-reading-a-request.md)
- [07 — Storing a request: column types, identifiers and one index](docs/learn/07-storing-a-request.md)
- [08 — Capability URLs: entropy, and why tokens are hashed](docs/learn/08-capability-urls.md)
- [09 — Cursor vs offset pagination](docs/learn/09-pagination.md)
- [10 — Background workers: goroutines, tickers, and the retention sweep](docs/learn/10-background-workers.md)
- [11 — SPA vs server-rendered, and what `go:embed` does](docs/learn/11-spa-and-go-embed.md)
- [12 — What CORS actually is, and how single-origin sidesteps it](docs/learn/12-cors.md)
- [13 — Server-Sent Events](docs/learn/13-server-sent-events.md)
- [14 — In-process pub/sub: fan-out, slow consumers, backpressure](docs/learn/14-pubsub.md)
- [15 — Server state, and TanStack Query's cache model](docs/learn/15-server-state.md)
- [16 — Recursive rendering: the JSON tree and the detail pane](docs/learn/16-recursive-rendering.md)
- [17 — NAT and firewalls: why the internet cannot reach your laptop](docs/learn/17-nat-and-firewalls.md)
- [18 — WebSocket: an HTTP request that stops being HTTP](docs/learn/18-websockets.md)
- [19 — Multiplexing and correlation: one connection, many requests](docs/learn/19-multiplexing-and-correlation.md)
- [20 — Forwarding, and what the provider is told](docs/learn/20-forwarding.md)
- [21 — The CLI: turning a frame back into an HTTP request](docs/learn/21-the-cli.md)
- [22 — Exponential backoff, jitter, and the thundering herd](docs/learn/22-backoff-and-jitter.md)
- [23 — Semaphores, bounded concurrency, and backpressure](docs/learn/23-bounded-concurrency.md)
- [24 — Size limits at every boundary](docs/learn/24-size-limits.md)
- [25 — Showing delivery: three states, not two](docs/learn/25-showing-delivery.md)
- [26 — Hashes, MACs, HMAC, and comparing without leaking](docs/learn/26-hmac.md)
- [27 — Replay: the same mechanism as the attack](docs/learn/27-replay-and-ssrf.md)
- [28 — Structural diff: comparing trees, not text](docs/learn/28-structural-diff.md)
- [29 — Mutations, and holding a secret in a browser](docs/learn/29-mutations-and-secrets.md)
- [30 — Three flaky tests, and what each one actually was](docs/learn/30-diagnosing-flaky-tests.md)
- [31 — Rate limiting: token buckets, and choosing what to count](docs/learn/31-rate-limiting.md)
- [32 — Counters, gauges, histograms, and why p99](docs/learn/32-metrics.md)
- [33 — Load testing, and what "dropped" actually means](docs/learn/33-load-testing.md)
- [34 — Caching the inbox lookup, and the part everyone gets wrong](docs/learn/34-caching-and-invalidation.md)

End-of-phase quizzes and their assessments are in [`docs/QUIZ.md`](docs/QUIZ.md).
