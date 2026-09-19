# 09 — Cursor vs offset pagination

*Phase 1, unit 4. Covers the curriculum bullet "cursor vs. offset pagination".*

## Brief

**What is it?** Two ways to ask for the next chunk of a large result set.

**Offset**: "skip the first N rows, give me the next M" -- `LIMIT 50 OFFSET 100`.
**Cursor** (also called keyset or seek): "give me M rows positioned after *this specific
row*" -- `WHERE (received_at, id) < ($1, $2) ... LIMIT 50`.

**What problem does it solve?** You cannot return four million rows. Offset is the obvious
formulation -- it maps directly onto how you would say it in English, every ORM has it, and
it is what everyone reaches for first.

**How does it work underneath?** Here is the thing that surprises people: `OFFSET 100000`
does **not** jump to row 100,000. The database has no way to do that. A B-tree index stores
keys in order, but it does not store "how many rows come before this one", so there is
nothing to seek to. Postgres reads 100,000 rows and throws every one of them away, then
returns the 50 you asked for.

Page 1 costs 50 rows. Page 2,000 costs 100,050 rows to return the same 50. The cost of a
page grows linearly with how deep it is, and the work is entirely wasted.

A cursor inverts this. You pass back the *value* of the last row you saw, and
`WHERE received_at < $1` is an index seek -- the B-tree descends to that key in O(log n) and
walks 50 entries forward. Every page costs the same as the first, whether it is page 2 or
page 20,000.

**The second problem, which is worse and less known: drift.** Offset counts rows, and the
count is only meaningful if the data holds still. Ours does not. Our list is newest-first
and append-only, so every new row is inserted at position zero and shoves everything down.

Read page 1 (rows 0-49). Ten webhooks arrive. Ask for `OFFSET 50` and you get what used to
be row 40 -- so rows 40-49 appear on **both** pages. The user sees ten duplicates and has no
idea why. Delete rows instead and you silently skip some. A cursor names a position *in the
data* rather than a count, so rows arriving above it cannot move it.

**Sharp edges.**

- **The cursor key must be unique as well as ordered.** `received_at` alone is not -- two
  requests can land in the same microsecond. Two rows sharing a cursor value means you
  either skip one or loop forever. The fix is a composite cursor with tuple comparison,
  which Postgres supports directly: `WHERE (received_at, id) < ($1, $2)`.
- **The tiebreaker has to be in the index too**, or Postgres can seek on the first column
  and then has to sort the ties.
- **Cursors should be opaque.** Expose `?after_id=01a0b975-...` and clients will start
  building them by hand, and then you can never change the representation. Encoding costs a
  little debuggability and buys freedom.
- **Cursor pagination cannot do "jump to page 57."** It only does next and previous. For a
  log viewer that is fine -- nobody jumps to page 57 of a webhook feed. For a search UI with
  numbered pages, offset is what users expect. Know which one you are building rather than
  cargo-culting.
- **Offset is genuinely fine for small bounded sets.** Ten pages of twenty items costs
  nothing.

**In hooklens.** The request list is the exact worst case for offset: append-only,
newest-first, unbounded per inbox, and read live while new captures are arriving. Cursor on
`(received_at, id)`, base64-encoded, with the tiebreaker added to the index.

## Decisions

**Cursor, not OFFSET, and the reason is drift more than speed.**
Speed is the famous argument and it is real: measured below, a page at OFFSET 4000 reads
4,050 rows to return 50, while the cursor reads exactly 50. But this list is read *live*
while captures arrive, so the decisive problem is correctness. Every new row lands at
position zero and shifts every offset beneath it, so a user paging through sees rows repeat.
Measured: after three inserts between pages, OFFSET returned three rows already shown.
*Offset would be fine* for a small bounded set, or a UI with numbered pages where users
expect to jump to page 57. This is neither.

**Composite cursor `(received_at, id)`, not `received_at` alone.**
`received_at` is not unique — two webhooks can land in the same microsecond. A cursor that
cannot distinguish two rows either skips one or returns it forever. `id` breaks the tie, and
because it is UUIDv7 it is already ordered consistently with time.
*Written as a row-value comparison*, `(received_at, id) < ($1, $2)`, which Postgres compares
lexicographically — `id` is consulted only where timestamps tie. The hand-expanded
equivalent, `received_at <= $1 and (received_at < $1 or id < $2)`, is the same thing and
much easier to get subtly wrong.

**Migration 00004 replaces the index rather than adding one.**
`(endpoint_id, received_at desc, id desc)` supersedes `(endpoint_id, received_at desc)` —
the old one is a prefix of the new one, so keeping both would pay insert cost twice to
serve the same queries. The directions must match the `ORDER BY` exactly: a B-tree walks
backwards happily, so a *wholly* reversed index also works, but a mixed-direction index
cannot serve a mixed-direction sort, and getting that wrong silently reintroduces a Sort.

**Fetch `limit + 1` to know whether there is a next page.**
The same probe trick as the body read in unit 06. The alternative is a separate
`COUNT(*)`, a second query that scans every matching row to answer a question we need one
bit of. The extra row is fetched, used as a boolean, and discarded.

**Cursors are opaque, base64 of `RFC3339Nano|uuid`.**
Not security — anyone can decode it in a second. It is a signal that the format is ours to
change. Expose `?after_time=...&after_id=...` and clients build them by hand, at which point
the internal representation is a public API forever.
*RFC3339Nano specifically*: the default `time.Time` text format is lossy at nanosecond
precision, and a cursor that loses precision lands between two rows and skips one.

**A malformed cursor is a 400, not "start from the beginning."**
Silently treating a broken cursor as absent would turn a truncated URL into an infinite
list that never terminates — the caller pages forever, always receiving page one.

**`Page` struct rather than returning `([]StoredRequest, string, error)`.**
Three return values where two are coupled invites transposing them. It also leaves room to
add `total` or `has_previous` without changing every call site.

## Walkthrough

### `internal/store/cursor.go`

`Cursor.String` (`:35`) and `ParseCursor` (`:40`) are deliberately a matched pair in one
file — the encoding and decoding of a format that only this package defines. `RawURLEncoding`
rather than `StdEncoding` so the value is safe in a query string without escaping.

`ParseCursor` returns `ErrBadCursor` for every failure mode. The caller has exactly one
thing to do about any of them, so distinguishing "bad base64" from "bad timestamp" would be
detail nobody acts on.

### `internal/store/requests.go`

`ListRequests` (`:117`) builds the query from two constants rather than one string with a
null guard. `($2::timestamptz is null or (received_at, id) < ($2, $3))` would work, and
gives the planner a condition it evaluates per row instead of a plain index bound.

The probe (`:130`): `probe := limit + 1`, then `if len(out) > limit` (`:171`) trims and
issues a cursor. Note the cursor is built from `page.Requests[limit-1]` — the last row the
caller *actually receives* — not from the probe row. Using the probe row would skip it on
the next page.

`out := make([]StoredRequest, 0, limit)` (`:158`) pre-sizes to `limit`, not `probe`: the
common case returns exactly `limit` rows and the probe row is discarded, so sizing for it
would over-allocate on every call.

### `internal/server/server.go`

The cursor parse in `handleListRequests` distinguishes absent from malformed. Absent means
first page; malformed is a 400. See the decision above for why conflating them creates an
infinite list.

`next_cursor` is omitted from the response entirely when there are no more pages, rather
than sent as `""`. A client can then loop on `while (next_cursor)` without a special case.

## Verified

12 store tests, all passing.

**The drift demonstration** (`TestCursorSurvivesInserts`) — read page 1, insert three rows,
read page 2 both ways:

```
page 1          : [/old-09 /old-08 /old-07 /old-06 /old-05]
page 2 (cursor) : [/old-04 /old-03 /old-02 /old-01 /old-00]
page 2 (offset) : [/old-07 /old-06 /old-05 /old-04 /old-03]
                     ^^^^^^^^^^^^^^^^^^^^^^^^ already shown on page 1
```

OFFSET repeated exactly the three rows the three inserts pushed down. The cursor continued
precisely where page 1 stopped.

**The cost demonstration** (`TestCursorPlanIsNotOffsetPlan`), 5,000 rows, `EXPLAIN ANALYZE`:

```
OFFSET 4000 : Index Only Scan ... (actual rows=4050)   <- read 4050 to return 50
cursor      : Index Only Scan ... (actual rows=50)     <- read exactly 50
```

Both use the same index. The difference is entirely in how many rows are read and thrown
away.

`TestCursorPaginationWalksEverything` pages through 23 rows at 5 per page and asserts 5
pages, 23 rows, correct order, no duplicates, and that the loop terminates.

### Two test bugs worth recording

**`TestIndexIsUsed` was meaningless and passed by luck.** It ran against an
almost-empty table and asserted no `Sort` node. Then it failed:

```
Limit -> Sort (Sort Key: received_at DESC, id DESC) -> Seq Scan on requests
```

The planner was *right*. It is cost-based, and on a table of a few rows, reading all of
them genuinely is cheaper than descending an index. **An index assertion on an empty table
tests nothing.** The fix was to seed 5,000 rows and run `ANALYZE` so the planner has
statistics rather than defaults — after which it picks the index, and the assertion means
something.

**The plan parser silently reported zero.** It looked for lines containing `"Index Scan"`,
but the node is an **Index *Only* Scan** — the query selects only `id`, which is in the
index, so Postgres never touches the heap. `"Index Only Scan"` does not contain the
substring `"Index Scan"`, so the match failed and both counts came back 0 — and the
comparison `0 <= 0` failed rather than passing, which is the only reason it was noticed.
