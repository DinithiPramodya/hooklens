# hooklens

**See exactly what a webhook sent you — then send it to your laptop.**

A public URL that captures every request byte-for-byte, streams them to a web UI as
they arrive, and forwards them to code running on `localhost`. Replay them, diff two
deliveries, and check the provider's signature. One binary, self-hosted, no account.

<!--
  DEMO GIF GOES HERE, and it belongs above everything else.
  A screen recording is the only element on this page that proves the thing works
  rather than claiming it. ~15 seconds: run `hooklens forward`, curl the URL it
  prints, the capture appears in the UI, expand the JSON tree.
  Record it, drop it in docs/demo.gif, and replace this comment with:
      ![hooklens capturing a webhook and forwarding it to localhost](docs/demo.gif)
-->

> **Not yet released.** The code is complete through hardening; packaging is written
> but no version has been tagged, and the repository carries no licence yet — see
> [what is still open](#what-is-still-open). Build from source today.

---

## What it does

- **A URL that keeps everything.** Method, path, query, every header with duplicates
  and ordering intact, and the body as raw bytes — never parsed and re-serialised, so
  a signature still verifies against what we stored.
- **Live, without refreshing.** Captures appear over SSE; the detail pane renders JSON
  as a collapsible tree, or a hex dump when the body is not text.
- **A tunnel to `localhost`.** `hooklens forward --to localhost:3000` delivers each
  capture to your app and relays its real response — status, headers and body — back
  to the sender. Same URL every run; reconnects by itself.
- **Replay and edit-and-replay**, to the tunnel or to any URL you name.
- **Structural diff** between two deliveries — nested JSON and headers, not a text
  diff full of key-ordering noise.
- **Signature verification** for Stripe, GitHub, Shopify and Slack, showing the exact
  bytes that were signed. That string is where your bug usually is.
- **Yours.** One static binary plus Postgres. No third party sees your payloads.

## Quickstart

Three commands and a `curl`, assuming Docker and Go.

```sh
git clone https://github.com/DinithiPramodya/hooklens && cd hooklens
cd web && npm ci && npm run build && cd ..   # the UI is compiled INTO the binary
docker compose up -d --wait                  # Postgres
go run ./cmd/hooklens migrate up && go run ./cmd/hooklens
```

Open <http://localhost:8080>, click to create an inbox, and send it something:

```sh
curl -X POST localhost:8080/e/$SLUG/webhook -d '{"hello":"world"}'
```

It appears in the list before you switch windows back.

To point a real provider at your machine, run the tunnel:

```sh
$ hooklens forward --to localhost:3000

  forwarding  http://632xap2u4zmm64zx3oqnxyhlzq.localhost/  ->  localhost:3000
  inbox       632xap2u4zmm64zx3oqnxyhlzq
  inspect     http://localhost:8080/
```

Anything sent to that URL is captured, then delivered to `localhost:3000`. The inbox
is saved under your user config directory (mode 0600 — it holds a token), so the
**URL is the same every run**. `--new` mints a fresh one; `--server` or
`HOOKLENS_SERVER` points at a hosted instance.

If your app is not running, the sender still gets a 2xx and the capture is stored with
`forward_error: unreachable`. A delivery failure on our side must never make a provider
retry or disable your endpoint.

### Once there is a release

```sh
brew install DinithiPramodya/tap/hooklens              # macOS, Linux
scoop bucket add hooklens https://github.com/DinithiPramodya/scoop-bucket
scoop install hooklens                                 # Windows
```

Neither works yet — the tap and bucket are not published. The build that will produce
them is in [`.goreleaser.yaml`](.goreleaser.yaml), explained in
[note 35](docs/learn/35-release-and-distribution.md).

## How it works

The part that is not obvious is the tunnel, and the reason is NAT: nothing on the
internet can open a connection *to* your laptop, but your laptop can open one
*outward* and keep it. So the CLI dials the server, and every capture travels back
down that connection it already holds.

```
                 ┌──────────────────────────────────────────┐
  Stripe ──POST──▶ 1. ingest        capture the raw bytes    │
  GitHub         │      │                                    │
  your curl      │      ├──▶ Postgres      durable, then:     │
                 │      │                                     │
                 │      ├──▶ broker ──SSE──▶ browser  (live list)
                 │      │                                     │
                 │      └──▶ hub                              │
                 │            │                               │
                 └────────────┼───────────────────────────────┘
                              │  one WebSocket, opened by the CLI
                              │  and held open — this is the arrow
                              ▼  that NAT will not let you reverse
                    hooklens forward --to localhost:3000
                              │
                              ▼
                     your app on :3000
                              │
                              └── its real response travels back up
                                  the same connection to the sender
```

Everything above the dashed boundary is one Go binary — API, UI and tunnel hub — with
the compiled frontend embedded by `go:embed`. There is no second service and no CORS,
because there is only one origin.

Two properties are worth knowing before you trust it with anything:

- **Durability comes first.** Once a capture is stored, nothing downstream can turn
  the response into a non-2xx. A provider treats any non-2xx as "not delivered" and
  retries, so a late failure would give you a duplicate of an event already held.
- **Nothing is silently truncated.** A body over the limit is refused with a clear
  status rather than stored half-complete — a truncated payload looks valid and fails
  signature verification for reasons nobody can see.

## Self-hosting

hooklens needs a Postgres and a place to run one static binary. It does not need
Redis, a queue, an object store or a second process.

```sh
docker compose up -d --wait          # or point DATABASE_URL at your own Postgres
hooklens migrate up                  # migrations are embedded in the binary
hooklens                             # serves API + UI on :8080
```

`hooklens migrate up` works on a machine holding nothing but the executable — the SQL
is compiled in.

For a real deployment you also want:

- **A wildcard DNS record and certificate** for `*.yourdomain`, so each inbox gets its
  own subdomain the way a provider expects. A wildcard certificate requires a DNS-01
  ACME challenge, not HTTP-01 — [note 05](docs/learn/05-dns-and-tls.md) explains why.
  Locally none of this matters: the `/e/{slug}/` path form works anywhere.
- **`HOOKLENS_BASE_DOMAIN`** set to that domain.
- **`/metrics`** scraped, and **not** exposed publicly — it publishes traffic volumes
  and error rates. Bind it to a private interface or terminate it at your ingress.
- **Rate limits** reviewed: the defaults are 10 inbox creations per IP per minute and
  50 captures per inbox per second.

### Configuration

All from the environment, with defaults that work on a clean machine.

| Variable | Default | |
|---|---|---|
| `HOOKLENS_ENV` | `dev` | `dev` or `prod`; selects text or JSON logs |
| `HOOKLENS_ADDR` | `:8080` | Listen address |
| `HOOKLENS_BASE_DOMAIN` | `localhost` | Domain the app is served from; one label in front of it is an inbox |
| `DATABASE_URL` | matches `compose.yaml` | Postgres connection string |
| `HOOKLENS_MAX_BODY` | `1MiB` | Largest body kept; bigger is refused, not truncated |
| `HOOKLENS_RATE_CREATE` | `10` | Inbox creations per IP per minute |
| `HOOKLENS_RATE_CAPTURE` | `50` | Captures per inbox per second |

### Commands

| | |
|---|---|
| `hooklens` | Serve. Also `serve` explicitly. |
| `hooklens version` | Print the version |
| `hooklens migrate up` | Apply pending migrations |
| `hooklens migrate down` | Reverse one migration |
| `hooklens migrate status` | Show applied and pending |
| `hooklens forward --to localhost:3000` | Tunnel captures to a local app |

### The two URL forms

```sh
curl -X POST localhost:8080/e/$SLUG/webhook  -d '{"hello":"world"}'
curl -X POST localhost:8080/webhook -H "Host: $SLUG.localhost" -d '{"hello":"world"}'
```

Both reach the same inbox. The subdomain form is what a provider uses in production;
the path form works anywhere and needs no wildcard DNS, which is why local development
uses it. Capturing never needs a token — the URL *is* the capability. Reading does:

```sh
curl -H "Authorization: Bearer $TOKEN" localhost:8080/api/endpoints/$SLUG/requests
```

The token is shown once, at creation, and stored only as a hash.

## What is still open

- **No licence.** The repository declares none, which legally means all rights
  reserved. This has to be settled before anybody can use a release.
- **No tagged release**, and therefore no Homebrew tap or Scoop bucket. The
  configuration exists and has not been executed.
- **The load test's throughput target is not met on the development machine** —
  ~860 req/s against a 1,000 req/s goal, with zero dropped captures at every rate
  tested. The remaining cost is two durable writes per capture;
  [note 33](docs/learn/33-load-testing.md) has the measurements.

## Development

### Requirements

- Go (version pinned in `go.mod`)
- Docker Desktop — on Windows this needs WSL2, so `wsl --install` and a reboot first
- Node.js 24+ — the frontend is compiled into the binary, so `go build` needs it built
  first

### Checks

```sh
gofmt -l .
go vet ./...
go test ./... -count=1
cd web && npm test && cd ..   # frontend: node --test, no runner dependency
```

### The load test

Behind a build tag, so it never taxes `go test ./...` and never runs in CI — a shared
runner under an unknown neighbour's load produces a throughput number that means
nothing.

```sh
go test -tags loadtest -run TestLoad -timeout 5m ./internal/server/ -v
```

Defaults to 1,000 req/s for 60 seconds; override with `HOOKLENS_LOAD_RATE`,
`HOOKLENS_LOAD_SECONDS`, `HOOKLENS_LOAD_WORKERS`. It asserts zero dropped captures,
which here means the server's 2xx count, the `hooklens_captures_total` metric and the
row count in Postgres are all equal. See
[note 33](docs/learn/33-load-testing.md) for why that phrase needed five definitions.

Two database diagnostics live alongside it, for when the answer is "something is slow"
and the next step is a measurement rather than a guess:

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
concurrent — every race found on this project so far was invisible to a non-race run.

One-time setup inside Ubuntu (`wsl --install -d Ubuntu`):

```sh
apt-get install -y gcc libc6-dev curl      # libc6-dev is easy to miss; gcc alone
                                           # fails with "stdlib.h: No such file"
curl -sSL https://go.dev/dl/goX.Y.Z.linux-amd64.tar.gz -o /tmp/go.tgz
tar -C /usr/local -xzf /tmp/go.tgz && ln -sf /usr/local/go/bin/go /usr/local/bin/go
```

Running it in a Docker container instead was tried and abandoned: compiling with
`-race` over a Windows bind mount crashed Docker Desktop twice.

### Layout

```
cmd/hooklens/        main, subcommand dispatch, migrate runner
internal/config/     environment configuration
internal/server/     host-based routing, middleware, app routes
internal/ingest/     the capture endpoint, and the inbox cache
internal/capture/    turning an *http.Request into a stored record
internal/broker/     in-process pub-sub, captures to open streams
internal/secret/     token generation and hashing
internal/sweeper/    the retention sweep, on a ticker
internal/tunnel/     the WebSocket hub, protocol and CLI client
internal/signature/  provider signature verification
internal/replay/     replay, and the SSRF guard on the dialer
internal/diff/       structural diff of bodies and headers
internal/ratelimit/  token buckets
internal/metrics/    Prometheus exposition, hand-rolled
internal/loadgen/    open-loop load generator
internal/store/      Postgres: the pool and the queries
internal/webui/      the built frontend, embedded via go:embed
web/                 frontend source (Vite + React + TypeScript)
migrations/          SQL migrations, embedded via go:embed
docs/learn/          how each piece works and why it was built this way
```

## Docs

[`docs/learn/`](docs/learn/) explains every piece: the concept, the alternatives that
were rejected, and a walkthrough of the code. Written as it was built, in build order —
[the index](docs/learn/README.md) groups them by phase. End-of-phase quizzes are in
[`docs/QUIZ.md`](docs/QUIZ.md).

If you read only three: [17 — NAT and firewalls](docs/learn/17-nat-and-firewalls.md)
for why a tunnel has to exist at all, [19 — multiplexing and
correlation](docs/learn/19-multiplexing-and-correlation.md) for how one connection
carries many requests without leaking a goroutine, and
[33 — load testing](docs/learn/33-load-testing.md) for a measurement that found a
2.5x bottleneck in a column nobody reads.

<details>
<summary>All 37 notes, in build order</summary>

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
- [35 — Cross-compilation, static binaries, and how a `brew install` works](docs/learn/35-release-and-distribution.md)
- [36 — The README as a product page](docs/learn/36-the-readme-as-a-product-page.md)
- [37 — Why the retention sweep could not use an index](docs/learn/37-indexable-predicates.md)

</details>

The build plan is [PLAN.md](PLAN.md); the working agreement that produced the notes is
[CLAUDE.md](CLAUDE.md).
