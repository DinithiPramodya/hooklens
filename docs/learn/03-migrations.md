# 03 — Schema migrations

*Phase 0, unit 4. Covers the curriculum bullet "schema migrations: why schema-as-code,
ordering, up/down".*

## Brief

**What is it?** A migration is a versioned, ordered script that changes a database's schema,
kept in source control next to the code that depends on it.

Your database has structure — tables, columns, indexes, constraints — and that structure
has to change as the application changes. A migration is one such change, recorded as a
file, applied exactly once, in a known order. The database itself records which migrations
it has already run, in a small bookkeeping table. The tool compares that record against the
files on disk and applies only what is missing.

**What problem does it solve?** Before migrations, you connected to the database and typed
DDL. That works with one developer and one machine, and fails three ways beyond that.

*There is no shared truth.* Your development database has a column staging does not, and
nobody remembers who added it. The schema exists only as the accumulated residue of commands
nobody wrote down.

*There is no repeatability.* A new developer, or a new environment, has no way to reproduce
the schema except by copying a dump — which starts drifting the moment it is restored.

*Schema and code are not coupled.* A deploy containing `SELECT email FROM users` requires
`email` to exist. If the schema is versioned separately from the code, that ordering is a
hope rather than a guarantee.

Migrations fix all three by making the schema a *sequence of committed files*. The schema at
any commit becomes a pure function of the migrations up to that commit. Check out last
month's code and you can rebuild exactly the schema it expected.

**How does it work underneath?** Two pieces, simpler than they look. First, a version table
in the database — for goose, `goose_db_version` — holding the IDs of applied migrations.
Second, ordered files on disk with an unambiguous sort order, usually a numeric or timestamp
prefix.

Applying migrations means: read the version table, find files whose IDs are not in it, run
each in order, and record each ID as it succeeds.

The part worth knowing one level down: on Postgres, each migration runs **inside a
transaction**, because Postgres has transactional DDL. A migration that fails halfway rolls
back completely and leaves nothing behind. MySQL cannot do this — a failed migration there
strands you in a half-applied state you have to untangle by hand. It is one of the genuinely
significant differences between the two.

**Up and down.** An "up" applies a change; a "down" reverses it. Down migrations are less
useful than they first appear. In production you almost always roll *forward* with a new
migration rather than reversing, because a down that drops a column destroys the data in it
— the schema reverses, the data does not. Their real value is in development, where you are
iterating on a design and want to re-run something.

**Sharp edges.**

- **Migrations are append-only once shared.** Editing a migration that has already run
  somewhere means that database will never receive the edit — its ID is already recorded.
  The fix is always a new migration, never a corrected old one.
- **Ordering must be total and stable.** Two people creating `00007_` on separate branches
  collide, and the merge looks clean while the schema is wrong.
- **Some DDL takes locks.** `CREATE INDEX` blocks writes for the duration;
  `CREATE INDEX CONCURRENTLY` does not, but cannot run inside a transaction — so migration
  tools need an explicit escape hatch for it. On a table with real traffic this is the
  difference between a deploy and an outage.

**In hooklens.** goose, plain SQL files, numbered, in `migrations/`. This unit creates the
`endpoints` table — the inbox — and deliberately creates only the columns whose concepts are
already covered. Phase 1 *adds* to it with a second migration rather than editing this one,
which is the append-only rule demonstrated rather than described.

## Decisions

**goose as a library, not the goose CLI — and the measurement that forced it.**
The obvious move was `go get -tool github.com/pressly/goose/v3/cmd/goose`, the modern Go
1.24+ way to pin a tool per project. It worked, and it added **60 indirect dependencies**:
ClickHouse, Vertica, YDB, MSSQL, MySQL, SQLite and libsql drivers, because the goose *CLI*
links a driver for every database it supports. Our application imports nothing outside the
standard library and will only ever talk to Postgres.

Switching to the goose *library* plus the one driver we need: **2 direct dependencies, 8
indirect.** Those numbers are the whole argument. Tool dependencies never reach the
application binary, but they do sit in `go.mod` and `go.sum`, they are downloaded by
`go mod download` in the Dockerfile on every cache miss, and they appear in every
vulnerability scan for the rest of the project's life.
*Wrong call if* we needed goose features only the CLI exposes (`create`, `fix`), which we
do not. *The signal to switch back* would be wanting scaffolding commands enough to accept
that graph.

**Migrations embedded in the binary with `go:embed`, not read from disk.**
Alternative was shipping a `migrations/` directory alongside the binary. Chose embedding
because it makes `hooklens migrate up` work on a server holding nothing but the executable:
no tool to install, no directory to copy, and — the real reason — **no way for the binary
and its migrations to be different versions.** That mismatch is a genuinely nasty production
failure and embedding removes the category.
This makes `migrations/` a Go package (`migrations/embed.go`), because `//go:embed` can only
see files in or below its own package directory. Full treatment of `go:embed` is Phase 2;
this is the light touch.

**A `migrate` subcommand on the same binary, not a second binary.**
One artifact to build, ship, and version. The dispatch is hand-rolled in `run()` rather than
reaching for a CLI library: there is exactly one subcommand, and the *default* — no
arguments at all — has to remain `serve` so the container `ENTRYPOINT` needs no arguments.

**`database/sql` here, `pgxpool` later.**
goose expects a `*sql.DB`, so this path uses the pgx stdlib adapter. The application proper
will use `pgxpool` in Phase 1, which suits a long-running server better. A migration runner
opens one connection, runs a handful of statements and exits, so the generic pool costs
nothing here.

**Explicit `db.PingContext` after `sql.Open`.**
`sql.Open` does not connect — it validates the DSN and prepares a lazy pool. Without the
ping, a wrong host or password does not surface until the first migration statement, buried
under a less obvious error. Two lines turn a confusing failure into a clear one.

**`gooseLogger.Fatalf` panics instead of calling `os.Exit`.**
goose calls `Fatalf` on failure. Exiting there would skip every deferred function on the way
out, including `db.Close`. Panicking unwinds them, and `run()` has no `recover`, so the
process still dies with a non-zero status. Same outcome, no leaked connection.

**`migrate down` reverses one migration; there is no `reset`.**
A command that drops every table should be typed out in full, not sit one word away from the
one you use daily.

**The first migration creates only columns whose concepts are already covered.**
`owner_token`, the configurable response, and retention are Phase 1 — and they arrive as
migration `00002` that *adds* to this table, never as an edit to `00001`. The append-only
rule demonstrated rather than described.

**`timestamptz`, never `timestamp`.**
`timestamp` discards the offset and stores whatever wall-clock number it was handed, so two
servers in two regions disagree about when a row was written. `timestamptz` normalises to
UTC on the way in. There is essentially no case in this application for the former.

**A `CHECK` constraint on `slug` duplicating `server.validSlug`.**
Deliberate duplication. The application validates before SQL is ever reached; the constraint
is a second line, not the only one. The database should not depend on the application being
correct — especially for a value arriving in the `Host` header, before any authentication.
Verified: `AB7F`, `a_b3`, `-abc` and `ab` are all rejected by Postgres.

## Walkthrough

### `migrations/embed.go`

Nine lines, one of which matters: `//go:embed *.sql` above `var FS embed.FS`. The directive
is a comment with no space after the slashes, which is load-bearing — `// go:embed` is just
a comment, fails silently, and leaves an empty filesystem. The package exists only so embed
has a directory to look in.

### `migrations/00001_create_endpoints.sql`

`-- +goose Up` and `-- +goose Down` are goose's section markers, not SQL. Everything between
Up and Down is the forward migration; everything after Down reverses it.

The check pattern requires the first and last characters to be alphanumeric with 1–30 in
between, which encodes "no leading or trailing hyphen, 3–32 total" in a single regex.
Postgres `~` is POSIX regex match.

### `cmd/hooklens/migrate.go`

The import block contains the one line people delete by accident: the pgx stdlib import
under a blank identifier. It is imported purely for its `init()`, which registers the driver
name `pgx` with `database/sql`. Remove it and `sql.Open` fails at runtime with "unknown
driver" — a compile-clean, test-clean failure that only appears when you actually run it.

`goose.SetBaseFS(migrations.FS)` redirects goose from the real filesystem to the embedded
one. The directory argument then becomes `.` — the root of that embedded FS — rather than
`migrations`.

`defer db.Close()` and the `PingContext` immediately after `sql.Open`: see decisions above.

The `switch` returns each goose call directly rather than collecting an error, because there
is nothing to do afterwards; `run()` logs it and sets the exit code.

### `cmd/hooklens/main.go:34` — `run()`

Restructured this unit. `run()` now loads config, builds the logger, sets up the signal
context, and *then* dispatches. Two things about that ordering are deliberate. The signal
context is created **before** dispatch, so `migrate` is interruptible too — Ctrl-C during a
long migration cancels the context and Postgres rolls the transaction back. And the `switch`
has no `default: serve` case; an unrecognised argument is an error, while *no* arguments
falls through to `serve(ctx, cfg, log)`. That keeps `ENTRYPOINT ["/hooklens"]` working with
no arguments while still rejecting typos rather than silently serving.

`serve()` at `cmd/hooklens/main.go:67` is the old `run()` body, unchanged.

### `internal/config/config.go:24`

`DatabaseURL` added, defaulting to the exact DSN `compose.yaml` produces, so a fresh clone
works after one `docker compose up -d` with nothing exported.

## Verified

- `migrate status` → pending; `migrate up` → applied in 59ms; `status` → timestamped
- `\d endpoints` matches the migration exactly: `uuidv7()` default, unique index on slug,
  check constraint present
- `goose_db_version` holds rows 0 and 1
- CHECK rejects `AB7F`, `a_b3`, `-abc`, `ab`; UNIQUE rejects a duplicate slug
- `migrate down` → table dropped; `migrate up` → recreated
- **Transactional DDL proven:** a temporary `00002` that created a table and then ran
  `select 1/0` left *no* table behind (`to_regclass(...) is null` returned true) and the
  recorded version stayed at 1. Postgres discarded the entire migration. On MySQL that
  table would still exist and the next `up` would fail on "table already exists".
- `gofmt`, `go vet`, `go test` all clean; image rebuilds; `docker run hooklens:dev migrate`
  reaches the subcommand
- Dependency graph: 60 indirect → **8 indirect, 2 direct**
