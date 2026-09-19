# 10 — Background workers: goroutines, tickers, and the retention sweep

*Phase 1, unit 5. Covers the curriculum bullet "background workers: goroutines, tickers,
retention sweeps".*

## Brief

**What is it?** Code running inside your server process on its own schedule, with no
request attached and nobody waiting for the result.

**What problem does it solve?** Some work must happen but has no request to hang off --
expiring old data, retrying a queue, refreshing a cache. Three ways to do it, and the trade
is real.

*Do it during a request* ("clean up old rows whenever someone reads the list"). One unlucky
user pays for everyone's cleanup, and if nobody visits for a week, nothing runs for a week.

*A separate cron job or container.* Correct at scale, and what you eventually want. But it
is a second artifact to build, deploy and version -- and it will drift from the app,
because nothing forces them to move together.

*An in-process goroutine on a ticker.* Ships with the binary, shares its configuration and
database pool, cannot drift. The cost is that it stops being correct the moment you run
more than one instance.

**How does it work underneath?** A **goroutine** is a function the Go runtime schedules
onto OS threads itself. It starts with about 8 KB of stack that grows on demand, against an
OS thread's fixed 1-8 MB. That ratio is the whole reason ten thousand goroutines is
unremarkable in Go and ten thousand threads is not -- and it is why Phase 3 can hold a
tunnel open per user without thinking about it.

A **ticker** is not a thread. `time.NewTicker(d)` registers an entry in a runtime-managed
heap of timers; the runtime sleeps until the earliest one is due and then sends on that
ticker's channel. A thousand tickers cost a thousand heap entries, not a thousand threads.

The worker itself is then a loop over a `select`: wake on a tick, do the work, or return
when the context is cancelled.

**Sharp edges.**

- **A ticker must be stopped**, or its runtime entry stays alive after the worker is gone.
  `defer ticker.Stop()`.
- **Ticks are dropped, not queued.** The channel has a buffer of one. If a sweep takes
  longer than the interval you do not get a backlog -- you get one pending tick and the rest
  are silently discarded. Usually what you want, but it means "every 10 seconds" quietly
  becomes "every 35 seconds" under load, with nothing in the logs saying so.
- **A panic in a goroutine takes down the entire process.** This bites people who have
  internalised that HTTP handlers are safe: `net/http` recovers per connection, so a panic
  in a handler kills one request. There is nothing above a bare goroutine. One nil
  dereference in the sweeper and the server is gone, mid-request, for everyone.
- **It must respect shutdown, and something must wait for it.** Otherwise SIGTERM cuts a
  sweep mid-transaction, or the process exits while the worker is still using a closing
  pool.
- **Two instances do the work twice.** For idempotent deletes that is merely wasteful; for
  anything with an external effect it is a bug. Coordination needs a lock, and that is the
  point where this design stops being the right one.
- **Deleting many rows in one statement** holds locks and bloats the write-ahead log. Batch
  it.

**In hooklens.** A retention sweeper that deletes captures past each inbox's retention
window. Ticker-driven, batched, panic-guarded, cancelled on shutdown, and awaited by `main`
before the process exits.

## Decisions

**In-process goroutine, not a cron job or a second container.**
It ships with the binary, shares its config and connection pool, and cannot drift from the
code it maintains. A separate cron container is the textbook answer and is what you
eventually want — but it is a second artifact to build, deploy and version, with its own
credentials, and nothing forces the two to move together.
*Wrong the moment there are two instances:* both sweep, both delete the same rows. For
idempotent deletes that is only wasteful, so the ceiling is higher than it looks — but it
is a ceiling. *The signal to switch* is either horizontal scale or a background job with an
external side effect, where doing it twice is a bug rather than a waste.

**Retention is per-endpoint, not one global constant.**
A throwaway debugging inbox wants hours; one wired into a staging integration wants weeks.
Migration 00005 adds `retention_hours` with a `NOT NULL DEFAULT 168` and a `CHECK` between
1 hour and 1 year. Note the contrast with 00003: there the three-step nullable/backfill/
enforce dance was necessary because no correct value existed for existing rows. Here a
default *is* correct for every row, so one statement does it.
Pinned by `TestDeleteExpiredRequests`, which proves a 48-hour-old row is expired in a
1-hour inbox and perfectly fine in a 168-hour one.

**No index on `requests.received_at`.**
The sweep filters on it across all endpoints, so an index would help — that query, running
every few minutes. Every single capture would pay to maintain it, forever. One index on an
append-heavy table, the same argument as unit 07.
*The real answer at scale is time-based partitioning*: `DROP PARTITION` is instant and
takes no locks, where `DELETE` writes a tombstone per row and leaves the cleanup to vacuum.
Out of scope, and written into the migration so the ceiling of this design is recorded
rather than discovered.

**Batched deletes, capped at 20 batches per tick.**
One unbounded `DELETE` over a backlog holds locks for its entire duration and writes a WAL
record per row before anything commits. Batching turns that into many short transactions
that can be interrupted between them. The per-tick cap stops a first run against months of
accumulated data from monopolising a connection — leftovers are simply collected next tick.
`DELETE ... LIMIT` is not valid Postgres, hence the `where id in (select ... limit $1)`
shape.

**`sweepOnce` never returns an error.**
There is no caller who can act on it. The next tick retries regardless, so an error return
would travel one frame up to be logged by something else. Logged and swallowed at the point
it happens.

**Context is checked between batches, not only between ticks.**
A sweep working through a backlog would otherwise ignore SIGTERM for as long as it took,
and the HTTP drain would time out around it.

**An unparseable `HOOKLENS_SWEEP_INTERVAL` falls back rather than failing startup.**
This contradicts the eager-validation rule in `config.Load`, deliberately. `HOOKLENS_ENV`
and `BASE_DOMAIN` are correctness settings where a wrong value means wrong behaviour; the
sweep interval is an operational knob where a typo should not trade a slightly-wrong
interval for an outage.

## Walkthrough

### `internal/sweeper/sweeper.go`

`Run` (`:58`) is the whole worker: a ticker, a `select`, two cases. `defer ticker.Stop()`
(`:62`) is not optional — without it the runtime keeps this ticker's timer entry alive after
`Run` has returned.

The `select` has `ctx.Done()` **first**. Go picks uniformly at random among ready cases, so
ordering is not semantically load-bearing — but it does mean a cancelled context is
honoured even on a tick, and `TestRunAlreadyCancelled` pins that a dead context returns
immediately instead of waiting a full interval.

`sweepOnce` (`:85`) opens with the `recover` (`:87`), and that placement is the point. A
panic in a bare goroutine takes down the **entire process** — `net/http` recovers per
connection so a panic in a handler kills one request, and people generalise from that
incorrectly. There is nothing above a goroutine. One nil dereference here and the server is
gone mid-request for everyone. `TestSweepOncePanicIsContained` constructs a `Sweeper` with a
nil store to force exactly that: without the recover, that test does not fail, it *crashes
the test binary*.

`debug.Stack()` is captured inside the recover rather than logged from a helper — once the
deferred function returns, the stack is gone.

The batch loop (`:104`) checks `ctx.Err()` before each batch, and treats
`context.Canceled` from the store as expected rather than as a fault (`:113`): during
shutdown that is the correct outcome, not an error worth an ERROR-level line.

`if n < int64(s.batch) { break }` (`:122`) — a short batch means the backlog is drained. The
alternative, a `COUNT(*)` to check first, is a second query answering a question the delete
already answers.

### `internal/store/requests.go` — `DeleteExpiredRequests`

`where id in (select ... limit $1)` rather than `delete ... limit`, which Postgres does not
support. `make_interval(hours => e.retention_hours)` takes the value as a parameter rather
than concatenating a column into an interval literal.

### `cmd/hooklens/main.go` — the shutdown ordering

Three lines that exist because of one deadlock.

`runCtx, cancel := context.WithCancel(ctx)` — a derived context, not the signal context
directly. `serve()` returns for two reasons: the signal context was cancelled (normal), or
`ListenAndServe` failed, for instance because the port is taken. **In the second case the
signal context is still live**, so `wg.Wait()` on it would block forever. A startup error
turning into a silent hang is a considerably worse failure than the error.

Then `cancel()` **before** `wg.Wait()`, and `wg.Wait()` before returning — because returning
runs the deferred `st.Close()`, and closing the pool out from under an in-flight `DELETE`
produces "conn closed" errors on every otherwise-clean shutdown.

## Verified

6 new tests (2 store, 4 sweeper); 20 across the project, all passing.

- `TestDeleteExpiredRequests` — per-inbox windows: a 2h-old row is swept from a 1h inbox, a
  30m-old row is kept, a 48h-old row is kept in a 168h inbox, a 200h-old row is swept. The
  same age being expired in one inbox and fine in another is what makes retention
  per-endpoint rather than global.
- `TestDeleteExpiredRequestsBatching` — a batch of 10 against 25 expired rows deletes
  exactly 10, and draining reports the short final batch.
- `TestRunStopsOnCancel` — `Run` returns within 2s of cancellation. If it did not, the
  `WaitGroup` in `run()` would block forever and SIGTERM would hang until SIGKILL.
- `TestRunAlreadyCancelled` — a dead context returns immediately, with the interval set to
  an hour so a `select` that only woke on ticks would fail.
- `TestSweepDeletes` — leave it running, and an expired row disappears with nobody asking.
- `TestSweepOncePanicIsContained` — a nil store forces a nil dereference inside `sweepOnce`.
  Without the recover this crashes the test binary rather than failing.

Containerised shutdown with the sweeper running: **663ms**, against Docker's 10s grace
period. The added `WaitGroup` did not introduce a hang.
