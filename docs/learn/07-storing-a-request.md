# 07 — Storing a request: column types, identifiers and one index

*Phase 1, unit 2. Covers the curriculum bullets "Postgres: `jsonb` vs `json` vs `text` vs
`bytea`, when each; indexes, and what the `(endpoint_id, received_at desc)` index actually
does" and "UUIDv7 and why time-sortable IDs matter".*

## Brief — choosing a column type

**What is it?** A Postgres column type is two decisions at once: what the value *means* —
can you query into it, index it, constrain it — and how it is *stored*, which determines
what survives a round trip.

Four candidates matter for us.

**`text`** is a string of *characters*, validated against the database encoding. It cannot
hold arbitrary bytes: a NUL byte is rejected outright, and so is invalid UTF-8. This is a
hard error from Postgres, not silent corruption.

**`bytea`** is arbitrary bytes. No encoding, no validation, no interpretation. What goes in
comes out.

**`json`** is text that has been validated as JSON and then stored *verbatim* — whitespace,
key order and duplicate keys all preserved. Every operation re-parses it.

**`jsonb`** is parsed on write into a binary tree. Whitespace is gone, object keys are
sorted, duplicate keys collapse with last-wins. In exchange it is fast to reach into and can
be indexed with GIN.

**What problem do the JSON types solve?** Before them you had two options for variable
structure: one column per field, which is rigid and needs a migration for every change; or a
serialised blob, which the database cannot see into at all. JSON columns let one column hold
variable structure the database can still query. `jsonb` specifically solved "I want
document flexibility *and* index performance."

**How does it work underneath?** `jsonb` decomposes the document into a binary layout with
keys stored in sorted order. Reaching a field is then a binary search rather than a re-parse
of the whole document. That sorting is exactly why it is fast — and exactly why it is lossy.
The speed and the information loss are the same design decision.

**Sharp edges.**

- **`jsonb` is lossy in ways that look like corruption.** `{"b":1,"a":2}` comes back
  `{"a": 2, "b": 1}`. Duplicate keys silently lose all but the last. Invisible and fine for
  most applications; fatal for anything that must reproduce what it received.
- **`text` cannot hold a NUL byte.** A webhook body containing one — protobuf, gzip, a PNG —
  cannot go in a `text`, `json` or `jsonb` column *at all*. The insert fails.
- **`jsonb` arrays are not lossy.** Only *object keys* are sorted and deduplicated. Array
  element order and duplicates survive exactly. That distinction is the escape hatch used
  below.
- **A GIN index over `jsonb` can be larger than the data it indexes.**

**In hooklens.** The body goes in `bytea` and there is no second option: Phase 4 verifies
signatures over exact bytes, and bodies are not reliably UTF-8 or even JSON. Headers go in
`jsonb` **as an array**, not an object — which sidesteps key-sorting entirely, because we
need the order and the duplicates an object would destroy. Method, path and query go in
`text`, with one catch handled in the decisions below.

## Brief — time-sortable identifiers, and what a composite index does

**What is it?** A UUID is a 128-bit identifier you can generate without asking anyone.
Version 4 is entirely random. Version 7 is a 48-bit millisecond timestamp followed by
randomness.

**What problem does it solve?** Why not an auto-incrementing integer? Three reasons. It
leaks volume — `id=1523` tells any user roughly how many rows you have. It requires the
database to allocate before you can know the value, so nothing upstream can reference the
row it is about to create. And two systems generating them independently will collide, which
makes merging or sharding painful.

**Why v7 rather than v4?** B-tree index locality. A B-tree keeps keys in sorted order across
pages on disk. Random v4 keys land at random positions, so every insert dirties a different
page, the effective working set becomes the whole index rather than its tail, and you get
page splits and write amplification throughout. A v7's leading timestamp means new rows sort
at the *end*, so inserts concentrate on the rightmost page, which stays cached. Identical
uniqueness guarantees, substantially better write behaviour on a table that only ever
appends — which is exactly what a capture log is.

It also means `order by id` is `order by time`, which unit 09's cursor pagination uses
directly.

**How does a composite index work underneath?** `(endpoint_id, received_at desc)` builds one
B-tree over the *pair*, ordered by the first column and then by the second within it. Our
query is always "the newest N requests for this one inbox." With that column order, Postgres
seeks straight to where that `endpoint_id` begins and walks forward — rows arrive already in
the order we asked for, so there is no sort step and no reading rows we then discard.

Reverse the columns to `(received_at desc, endpoint_id)` and the index becomes useless for
that query: one inbox's rows are scattered across the entire tree, so you scan everything and
filter. The rule generalises: **equality columns first, then the range or ordering column.**

**Sharp edges.**

- **A v7 UUID leaks its creation time** — the timestamp is in the first 48 bits. Fine here;
  not fine where creation time is sensitive.
- **`desc` in an index declaration matters less than people think.** A B-tree can be walked
  backwards, so `(endpoint_id, received_at)` would serve `order by received_at desc`
  perfectly well. Declaring direction only matters when different columns need different
  directions.
- **An index is not free.** Every insert maintains it, and this table is append-heavy. One
  well-chosen index beats four speculative ones.

## Decisions

**`bytea` for the body. There is no second option.**
`text` rejects a NUL byte outright, and webhook bodies are routinely protobuf, gzip or
images. `jsonb` sorts object keys and collapses duplicates, so a body would come back
*different from what arrived* — and Phase 4 computes HMACs over exactly those bytes. `json`
would preserve the text faithfully but still rejects NUL and refuses anything that is not
valid JSON, which a capture endpoint cannot promise.
*Wrong call if* we wanted to query inside bodies in SQL. We do not: the UI parses on read,
and a `jsonb` projection could always be added later as a generated column beside the
`bytea`, never instead of it.

**Headers as a jsonb ARRAY of `{name, value}` objects, not a jsonb object keyed by name.**
This is the decision that makes `jsonb` usable here at all. `jsonb` sorts *object keys* and
collapses duplicates — which would destroy both the ordering and the repeated headers HTTP
explicitly permits. `jsonb` **arrays** preserve element order and duplicates exactly. So
wrapping the pairs in an array sidesteps the lossiness completely while keeping the column
queryable and indexable.
*Rejected alternatives:* two parallel `text[]` columns (nothing keeps them in step), or a
separate `request_headers` table (correct, and a join on the hottest read path for data
that is never queried independently).
Pinned by `TestInsertRequestRoundTrip`, which stores a duplicate header name and asserts
both survive in order.

**Store the percent-ENCODED path.**
`r.URL.Path` is decoded, so a request for `/a%00b` puts a NUL byte in a Go string — and
Postgres rejects NUL in `text`. `EscapedPath()` is both storable and closer to the literal
bytes on the wire, which is what an inspector should be showing anyway. This also surfaced
a latent bug in the router: `route` rewrites `URL.Path` for the `/e/{slug}/` form without
clearing `URL.RawPath`, leaving a stale encoding behind. `net/url` does detect the mismatch
and re-encode, so nothing was actually broken — but relying on that is relying on an
implementation detail of the very function whose job is this ambiguity.
`internal/server/server.go:77` now clears it explicitly.

**`declared_size` is NULL under chunked encoding, not `-1`.**
`capture.Request` carries `-1` because that is what `r.ContentLength` gives. The database
stores NULL, because "unknown" and "negative one" are different statements and a NULL
cannot be silently summed, averaged or compared into a wrong answer. Same reasoning for
`source_ip`: unparseable becomes NULL rather than an empty string, which `inet` has no
representation for anyway.

**`inet` for the IP, not `text`.**
Postgres validates it on the way in, normalises the representation, and supports subnet
containment operators — which Phase 5's per-IP rate limiting will want. `text` would accept
`"not an ip"` and force every future query to parse.

**One index, not four.**
`(endpoint_id, received_at desc)` and nothing else. Every read this table has is "newest N
for this inbox". Speculative indexes on `method`, `path` or `received_at` alone would each
cost write throughput on an append-heavy table to serve queries nobody makes. Adding one
later is a migration; removing one is a conversation about whether something depends on it.

**IDs as `string` in Go, with `::text` casts in SQL.**
The alternative was adding `github.com/google/uuid` plus pgx's registration shim so `uuid`
columns scan into `uuid.UUID`. We never parse, compare or generate these values in Go — they
come from `uuidv7()` in Postgres and go back as parameters. A dependency and a type
conversion to gain nothing.
*The signal to change:* the first time application code needs to read the timestamp out of
a v7, or generate an ID before the insert.

**Resolve the inbox BEFORE reading the body.**
An unknown slug otherwise means reading up to a megabyte from a stranger for a destination
that does not exist — free storage and bandwidth exhaustion for anyone scanning subdomains.
Rejecting first costs one indexed lookup. `internal/ingest/ingest.go:89`.

**503 on a database failure, not 500 — and not 200.**
A provider reads any non-2xx as "not delivered" and retries. When we genuinely have not
stored anything, that retry is the behaviour we *want*, and 503 says "come back later"
rather than "something is broken forever". This is the mirror image of Phase 0 quiz Q1: the
bug there was returning an error for something we *had* stored. Getting this right in both
directions is the whole point of the `---- durable from here on ----` line at
`internal/ingest/ingest.go:127`.

**`TouchEndpoint` is best-effort and deliberately after the insert.**
`last_seen_at` is a nicety. It sits below the durability line and its error is logged, never
returned, because a failure there must not turn a stored request into a retry.

**Tests skip when no database is reachable, rather than failing.**
Honest trade-off with a real cost: a green `go test ./...` on a machine with Docker stopped
does **not** mean the store layer passed — which is exactly what happened on the first run
of this unit. It is acceptable only because CI always has a Postgres service container, so
CI never skips. testcontainers would remove the footgun and is Phase 5 material; pulling it
forward would mean teaching it before its brief.

## Walkthrough

### `migrations/00002_create_requests.sql`

`on delete cascade` on the foreign key: deleting an inbox removes its captures. The
alternative, `on delete restrict`, would make deletion fail until every request is gone
manually — wrong for data whose whole lifecycle is tied to its parent.

The index declaration is the file's only performance decision, and the column order is the
whole of it. `(endpoint_id, received_at desc)`: equality column first, ordering column
second. Reverse them and one inbox's rows scatter across the entire B-tree.

### `internal/store/store.go`

`Open` (`:24`) sets four pool parameters and each has a reason. `MaxConns = 10` sits well
under Postgres's default `max_connections` of 100 — every connection is a backend *process*
server-side, so more is not better. `MinConns = 2` keeps connections warm so the first
capture after an idle period does not pay for a handshake plus auth. `MaxConnLifetime` and
`HealthCheckPeriod` guard against connections that have died silently behind a NAT or load
balancer idle timeout, which otherwise appears as one inexplicable failed request.

`pool.Ping` (`:55`) is the same argument as the migrate runner's: `NewWithConfig` is lazy
and does not connect, so without this a wrong password fails on the first captured webhook
rather than at startup.

`isCode` (`:73`) uses `errors.As`, not a type assertion. pgx wraps its errors, so a direct
assertion on the top-level value misses anything annotated with `%w` on the way up. This is
what keeps `pgconn` from leaking past this package: callers get `ErrSlugTaken`.

### `internal/store/requests.go`

`InsertRequest` (`:41`) builds two pointers before the query — `declared` and `ip` — solely
so "unknown" reaches Postgres as NULL. Passing the values directly would store `-1` and an
empty address.

`ListRequests` (`:91`): `defer rows.Close()` is not optional. Exhausting the iterator closes
it for you, but any early return inside the loop would not, and a leaked `Rows` holds its
connection out of the pool forever — a leak that presents as the app hanging after exactly
`MaxConns` failures.

`rows.Err()` (`:120`) looks redundant after the loop and is not. A failure *during*
iteration makes `Next()` return false, which is indistinguishable from "no more rows"
without this check. Skip it and a partial result set silently looks like a complete one.

`scanner` (`:141`) is a two-line interface satisfied by both `pgx.Row` and `pgx.Rows`, so
`GetRequest` and `ListRequests` share one scan function instead of two that drift apart the
first time a column is added.

### `internal/ingest/ingest.go`

The `---- the request is durable from here on ----` comment at `:127` is the structural
version of Phase 0 quiz Q1. Above it, every failure returns non-2xx and the provider
retrying is correct because nothing was stored. Below it, nothing may return non-2xx,
because a retry would duplicate an event we already hold. `TouchEndpoint` and the log line
are deliberately the only things down there.

### `internal/server/server.go`

`handleCreateEndpoint` (`:104`) validates with `validSlug` — the same function
`Resolve` uses. Two separate notions of "valid slug" would drift, and the failure mode is
an inbox that can be created and can never receive anything.

`handleGetRequest` (`:179`) base64-encodes the body, because `bytea` holds arbitrary bytes
and a JSON string must be valid UTF-8. The UI decodes and decides how to render.

`json.NewDecoder(io.LimitReader(r.Body, 4<<10))` (`:111`) — the unit 06 lesson applied
somewhere it is easy to forget. `json.Decode` on an unbounded reader has exactly the same
problem as `io.ReadAll`.

## Verified

Store tests, all against a real Postgres 18 (5 tests):

- Endpoint create / duplicate-slug / lookup / not-found, with the unique violation
  surfacing as `ErrSlugTaken` rather than a raw pgx error
- **Round trip byte-for-byte**, including a NUL byte and `0xff` in the body, a duplicate
  header name, and `jsonb` array ordering
- NULL handling for chunked (`declared_size`) and unparseable IP (`source_ip`)
- Ordering newest-first, scoped to one inbox, with a second inbox's newer row proving
  isolation

`TestIndexIsUsed` asks Postgres what it plans to do rather than asserting on output:

```
Limit  (cost=0.15..8.17 rows=1 width=24)
  ->  Index Scan using requests_endpoint_received_idx on requests
        Index Cond: (endpoint_id = '01a0b974-...'::uuid)
```

`Index Scan`, `Index Cond` on `endpoint_id`, and **no `Sort` node** — the index supplies the
ordering, exactly as the brief claimed. A test asserting rows come back in the right order
would pass just as happily with a sequential scan plus a sort, which is the thing the index
exists to avoid.

### End to end against the running binary

Created an inbox, sent three requests, read them back:

| Path | `body_size` | `body_truncated` | headers |
|---|---|---|---|
| `/big` (2 MB sent) | 1048576 | **true** | 6 |
| `/bin` | 4 | false | 5 |
| `/webhook` | 16 | false | 7 |

Newest first, as the index orders them. POSTing to an inbox that does not exist returns
**404 before the body is read**.

Byte fidelity, decoded back out of `bytea`:

```
{"b":1,   "a":2}   ->  7b22 6222 3a31 2c20 2020 2261 223a 327d
```

The three spaces survived and the keys are still `b` then `a`. Stored as `jsonb` this would
have come back `{"a": 2, "b": 1}` — reordered and reformatted, and every Phase 4 signature
check against it would fail.

```
NUL + 0xff + gzip magic  ->  00ff 1f8b
```

A `text` column would have rejected that insert outright.

And the headers, read back from the `jsonb` array:

```
"Host":"localhost:8080"     <- the header net/http strips from the map
"X-Custom":"one"            <- duplicate name...
"X-Custom":"two"            <- ...both present, in order
```

Stored as a `jsonb` object those two would have collapsed to one.

### One test failure worth keeping

`TestListRequestsOrdering` failed on its first run: `violates check constraint
"endpoints_slug_check"`. The test built its second slug as `uniqueSlug(t)+"x"`, which is 33
characters against a 3–32 limit. A bug in the test, not the code — and the `CHECK`
constraint added in migration 00001 catching it, which is the second line of defence doing
exactly the job it was argued for in [03](03-migrations.md).
