# hooklens

A webhook inspector with a self-hosted tunnel. It gives you a public URL, captures every
request sent to it byte-for-byte, streams them to a web UI, and forwards them to code
running on your laptop. Replay, diff, and provider-signature verification on top.

> **Status: Phase 1 of 6 complete — the mailbox works.** Requests are captured, stored
> byte-for-byte, authenticated, paginated and expired. The UI arrives in Phase 2. Nothing is

> visual yet. See [PLAN.md](PLAN.md) for the full build plan.

## Requirements

- Go (version is pinned in `go.mod`)
- Docker Desktop — on Windows this needs WSL2, so `wsl --install` and a reboot first

## Run it

```sh
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

Migrations are embedded in the binary, so `hooklens migrate up` works on a machine holding
nothing but the executable.

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
```

`go test -race` needs cgo and therefore a C compiler, which Windows does not have by
default — CI runs the race detector on Linux.

## Layout

```
cmd/hooklens/      main, subcommand dispatch, migrate runner
internal/config/   environment configuration
internal/server/   host-based routing, middleware, app routes
internal/ingest/   the capture endpoint
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

End-of-phase quizzes and their assessments are in [`docs/QUIZ.md`](docs/QUIZ.md).
