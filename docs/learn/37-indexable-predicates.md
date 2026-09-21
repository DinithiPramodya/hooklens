# 37 — Why the retention sweep could not use an index

> Phase 6 · unit 37, and unplanned. Found by a test that failed for a reason that
> looked like my own mess and turned out to be two problems wearing one costume.

## Brief

**What is this thing?** A *sargable* predicate — the ugly old term is "Search
ARGument-able" — is one the database can answer by walking an index instead of
reading every row. The property is narrower than people assume: an index on a column
helps only when the query's condition is expressed in terms of that column compared
against something the planner can compute *before* it starts scanning. Wrap the
column in a function, compare it to a value from another table, and the index becomes
unusable even though it is sitting right there and looks relevant.

**What problem does it exist to solve?** It does not solve a problem so much as
describe a constraint, and the constraint exists because of how a B-tree works. An
index is a sorted structure. To use it, the database must be able to say "start at
this key and walk" — which requires knowing the key. `where received_at < $1` gives
it one. `where received_at < now() - retention_from_some_other_table` does not,
because the boundary is different for every row depending on which endpoint that row
belongs to. There is no single place to start walking, so there is nothing to do but
look at all of them.

**How does it actually work underneath?** Three cases, and the third is the one that
bites.

*A function on the column.* `where lower(email) = 'x'` cannot use an index on
`email`, because the index holds the original values and their sort order is not the
lowercased sort order. The fix is an expression index on `lower(email)`.

*A type mismatch or implicit cast* can force the same thing, silently.

*A predicate that depends on a joined row* — the case here. Postgres must produce the
join before it can evaluate the condition, so it reads the rows first and filters
after. Critically, **`LIMIT` does not save you.** A `LIMIT 1000` on a query whose
filter runs after the join still has to scan until it finds 1000 matches, and if
nothing matches it scans everything. The plan looks cheap and the execution is not.

The standard fix is to make the value the query needs *present on the row being
scanned* — compute it once at write time, store it, index it. That is deliberate
denormalisation: you accept a copy of derived data, and the cost of a copy is that it
can go stale, so something has to maintain it.

**What are the sharp edges?**

- **`EXPLAIN` alone will not tell you.** The estimated cost of the bad plan can look
  reasonable; you need `EXPLAIN (ANALYZE)` and specifically the **`Rows Removed by
  Filter`** line, which is where the wasted work shows up.
- **The worst case is the empty case.** A cleanup query that finds nothing to do
  still pays the full scan — so the steady state, which is most of the time, is the
  expensive one.
- **Stale derived columns.** If retention changes, every already-written row holds an
  expiry computed from the old setting.
- **Adding an index to a large table locks it**, unless you build it concurrently,
  which cannot run inside a transaction — and migration tools wrap everything in one.

**In hooklens:** the retention sweep ([10](10-background-workers.md)) deletes requests
past their inbox's retention window, and the window lives on `endpoints` while the
timestamp lives on `requests`. So the predicate spans two tables, the `(endpoint_id,
received_at desc)` index cannot be used, and every tick is a full scan of `requests`.
Measured on the development database: **276ms and 576,639 rows examined, to return
zero.** Every tick, forever, growing linearly with the table — and this is the sweep
that exists to stop the table growing.

---

## How it was found, which is most of the lesson

`go test ./...` failed in `TestSweepDeletes` — "the expired request was still present
after 3s of sweeping" — in a suite that had been green twenty minutes earlier, with no
change to the sweeper.

The first hypothesis was my own mess: units 33 and 34's load runs had left **176,000
rows** in the development database. That was true, and deleting them made the test
pass, and stopping there would have been a mistake. "My test data broke a test" is a
complete-sounding explanation that explains the trigger and not the mechanism — the
sweeper is *supposed* to cope with a large table; deleting rows to make it pass is
treating the symptom of something.

So: measure before concluding. `EXPLAIN (ANALYZE)` on the sweep query, at the 576,639
rows that remained after cleanup:

```
Limit  (actual time=276.140..276.143 rows=0.00)
  Rows Removed by Join Filter: 576639
  ->  Seq Scan on requests r  (actual time=0.542..158.562 rows=576639)
Execution Time: 276.361 ms
```

**276ms, examining 576,639 rows, to return zero.** Not a test-data problem. A
sequential scan of the entire table, every sweep tick, forever, growing with the table
— in the component whose job is to keep the table from growing.

Two distinct problems wearing one costume, and the pollution was the one that made the
other visible. Both got fixed.

---

## Decisions

**Store the expiry on the row, rather than any of the alternatives.**
Four options.
(a) **Leave it** — it only hurts at scale, and the sweep is a background job. Rejected
because "only at scale" describes a webhook inspector after a month of use, and the
cost falls in the empty case, which is most ticks.
(b) **A per-endpoint loop:** for each inbox, delete where `received_at <` its own
window. Every one of those *is* index-assisted by the existing `(endpoint_id,
received_at desc)` index, so it needs no schema change at all. Rejected: 2,346
endpoints on this machine means 2,346 queries per tick, trading one bad query for
thousands of good ones.
(c) **An expression index.** Cannot be done — the expression depends on another
table's column, and an index is built from one row at a time.
(d) **Store `expires_at` on the request, indexed.** Chosen.
**This is the wrong call if** retention becomes something that changes often or is
computed from several inputs, at which point maintaining the copy costs more than the
scan. **The signal to switch** is `SetRetention` showing up in a latency profile.

**NOT NULL, and backfilled in the migration.**
The alternative was a nullable column with the sweep treating NULL as "use the old
join path". That keeps the migration trivial and leaves two code paths forever, one of
which is the slow one nobody tests. A NULL here would also mean "never expires",
silently — a row that opts out of retention is precisely the bug this column exists to
prevent.

**A plain btree, not a partial index.**
A partial index (`where expires_at < now()`) is the intuitive choice and is illegal:
the predicate must be immutable and `now()` is not. It would also not help. Almost
every row is in the future, and the index's value is not that it filters — it is that
it lets the scan **walk in expiry order and stop after `LIMIT`**, so the cost is
proportional to what gets deleted rather than what is stored.

**Built inside the migration's transaction, not CONCURRENTLY.**
`CREATE INDEX CONCURRENTLY` avoids the write lock and cannot run inside a transaction,
which would mean opting this migration out of one and giving up the all-or-nothing
property from [03](03-migrations.md) — a failed migration would leave an invalid index
behind. For a single-instance self-hosted tool, a sub-second lock is the better trade.
**The signal to change it** is a deployment where `requests` is big enough that the
lock is an outage; then it becomes its own migration, without a transaction, applied
deliberately.

**`SetRetention` becomes two statements in a transaction.**
This is the bill for the denormalisation, and it comes due immediately. Without the
second statement, shortening an inbox's retention would apply only to future captures
while the settings page claimed otherwise — and *lengthening* it would be worse: rows
already past their old expiry would be swept away despite the user having just asked
to keep them. The transaction is there because a failure between the two leaves the
column disagreeing with the setting it is derived from, which is the one state nothing
else in the system checks for. Both directions have a test.

**`DeleteEndpoint` reports a delete that matched nothing.**
Added for test cleanup, and it returns `ErrNotFound` when `RowsAffected() == 0`. That
is not defensive habit: the first version returned nil regardless, the cleanup silently
did nothing, and the tests passed while the rows piled up — exactly the failure the
method was written to prevent, reproduced inside the fix for it. A no-op delete means
the caller is wrong about something.

**The load tests clean up after themselves, with `t.Cleanup` rather than `defer`.**
Worth its own line because the first attempt failed: `defer st.Close()` runs **before**
any `t.Cleanup`, so the cleanup found a closed pool. Registering `t.Cleanup(st.Close)`
first and the row deletion second gets the right order, because cleanups run LIFO.

---

## Walkthrough

### `migrations/00009_expires_at.sql`

Column, backfill, `SET NOT NULL`, index — in that order, and the order is the whole
migration. `NOT NULL` cannot precede the backfill, and the index is cheapest built once
the data is final.

One thing worth knowing about goose that cost a failed run: **it reads any comment line
containing its marker as a directive.** The comment explaining why the index is *not*
built with the no-transaction annotation originally spelled that annotation out, and
goose tried to parse the sentence:

```
failed to parse annotation line "-- `-- +goose NO TRANSACTION` and giving up ...":
not supported: invalid annotation
```

The comment now describes the annotation without writing it, and says why.

### `internal/store/requests.go`

**`InsertRequest` (`:49`).** `expires_at` is computed in the same statement, from a
subquery on `endpoints`. A subquery rather than a value passed from Go, because the
caller holds a cached `*Endpoint` that does not carry `retention_hours` — and threading
that field through the cache would give it a second thing to go stale. An indexed
primary-key lookup inside a statement already running costs far less than another round
trip.

Note `$11::timestamptz`, appearing twice. Without the cast, `$11` is used once as a
column value and once in `$11 + make_interval(...)`, and Postgres refuses: `inconsistent
types deduced for parameter $11`. It fails at execution, not at build, so every insert
test went red at once.

**`DeleteExpiredRequests` (`:298`).** The subquery-then-delete shape is unchanged —
`DELETE ... LIMIT` is still not valid Postgres, and an unbounded delete would still hold
locks across an entire backlog. What changed is the predicate: `where expires_at < now()
order by expires_at limit $1`, which is an index scan that stops.

**`SetRetention` (`:338`).** Two statements in an explicit transaction, with
`defer tx.Rollback(ctx)` — a no-op after a successful commit, and the thing that saves
you on every error path you did not write by hand.

### `internal/store/endpoints.go`

**`DeleteEndpoint`.** One statement; `requests.endpoint_id` has `on delete cascade`
(migration 00002), so there is no window in which an inbox is gone and its captures are
not. Returns `ErrNotFound` on zero rows affected.

### `internal/store/expiry_test.go`

**`TestSweepUsesTheExpiryIndex`** asserts on the **plan**, not on a duration — a timing
assertion on a machine under unknown load is a flake, and "it was fast" is not the
property anyway. It seeds 5,000 rows and runs `ANALYZE` first, because on a small table
a sequential scan genuinely *is* cheaper and the planner would be right to choose it,
which would make the test assert the opposite of what it means.

**`TestSetRetentionReprojectsExistingExpiries`** and
**`TestSetRetentionLengtheningRescues`** cover the derived-copy hazard in both
directions. The second is the one that matters more: it is the direction that loses
data rather than merely keeping it too long.

---

## Verified

- `gofmt -l .`, `go vet ./...`, `go vet -tags loadtest ./...` clean.
  `go test ./... -count=1` green.
- `migrate up`, `migrate down`, `migrate up` — the full cycle CI runs. The up took
  21.7s on 576k rows (the backfill), the down 27ms.
- **The measurement, before and after**, same query shape, same database:

  | | plan | rows examined | time |
  |---|---|---|---|
  | before | Seq Scan on requests | 576,639 | 276ms |
  | after | Index Scan using `requests_expires_at_idx` | 0 | **0.75ms** |

  ~370x, and more importantly the cost is now proportional to rows *deleted* rather
  than rows *stored*, so it no longer degrades as the table grows.
- `TestSweepDeletes`, the test that started this, passes — including at the table size
  that broke it.
- Three new tests in `internal/store/expiry_test.go`, all passing.
- Cleanup verified by running the diagnostics and checking the row count before and
  after: the run's own rows are gone, and the pre-existing leftovers from before
  cleanup existed were removed by hand.

### Not verified

The `CREATE INDEX` lock duration on a table much larger than 576k rows. The decision
to stay transactional rests on "sub-second lock at this size", which is measured here
and is an extrapolation anywhere else.
