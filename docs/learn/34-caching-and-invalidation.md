# 34 — Caching the inbox lookup, and the part everyone gets wrong

> Phase 5 · unit 34. [33](33-load-testing.md) measured three database round trips per
> capture and named the cheapest one to remove. This is that one — and the reason it
> is not as cheap as it looks.

## Brief

**What is this thing?** A cache is a second, faster copy of an answer you already
computed, kept somewhere closer, and used instead of computing it again. Here: the
inbox row for a slug, held in the process's memory, so a capture does not ask
Postgres "which endpoint is `abc123`?" for the hundredth time this second.

**What problem does it exist to solve?** The same answer being recomputed. Before
caching, the options were to make the computation faster — indexes, a better query —
or to do it less often, which is caching by another name. The index is already there
and the lookup already takes 0.25ms; the problem is not that any one lookup is slow,
it is that there are a thousand of them a second for a row that changed once, when
it was created. That is the shape that caching fits: **high read rate, low write
rate, tolerance for a stale answer.** Where any of those three is false, a cache is
a liability.

**How does it actually work underneath?** The mechanism is trivial — a map from key
to value, guarded by a lock, consulted before the real lookup and populated after
it. Everything difficult is in the second half: *when is the copy wrong, and what
happens then?* Three mechanisms handle that, and they are not interchangeable.

- **TTL (time to live):** each entry carries an expiry; past it, the entry is
  discarded and the real lookup runs again. Bounded staleness, no coordination, and
  it self-heals — a wrong entry is wrong for at most the TTL. It cannot make anything
  fresh *now*.
- **Explicit invalidation:** whoever changes the underlying data deletes the cached
  copy. Immediate, and it only works if every writer knows about the cache. In a
  single process that is achievable; across replicas it is a distributed-systems
  problem wearing a small hat.
- **Bounded size and eviction:** a cache with no size limit is a memory leak that
  grows exactly as fast as your traffic diversity. Somebody scanning subdomains
  creates one entry per guess.

Underneath all three sits the one property worth internalising: **a cache turns a
correctness problem into a timing problem.** The data is not wrong; it is wrong *for
a while*, and the design question is how long "a while" may be for this particular
piece of data.

**What are the sharp edges?**

- **Caching a negative result.** If "no such inbox" is cached, a newly created inbox
  404s until the entry expires — the one case where staleness is user-visible
  immediately. If it is *not* cached, a scanner hammering random subdomains bypasses
  the cache entirely and puts the full lookup rate back on the database. Both choices
  are defensible; picking without noticing the trade is not.
- **The thundering herd / cache stampede.** An entry expires while a thousand
  requests are in flight, and all thousand miss and all thousand hit the database at
  once — a load spike caused by the thing meant to prevent load. The same shape as
  the reconnect storm in [22](22-backoff-and-jitter.md), and the same family of
  fixes.
- **Caching authorisation decisions.** If the cached value is used to decide whether
  a token is valid, then revoking a token does not revoke it — it schedules the
  revocation for up to one TTL from now.
- **Unbounded growth**, above.
- **Testing what is now non-deterministic.** A cached lookup returns a different
  thing depending on what happened in the last N seconds, which is exactly the shape
  of a flaky test. Injecting the clock, as `internal/ratelimit` does in
  [31](31-rate-limiting.md), is not optional.

**In hooklens:** the capture path resolves a slug to an endpoint on every request.
That row changes essentially never after creation, and a capture uses only its id and
retention setting — it is not an authorisation decision, because a capture URL is
public by design ([08](08-capability-urls.md)). So this is close to the ideal case
for a small TTL cache. The two decisions with teeth are how long the TTL is, given
that deleting an inbox should stop captures reasonably promptly, and whether to cache
the 404.

---

## Decisions

**A cache at all, and only after measuring.**
Worth stating plainly, because "add a cache" is the reflex that makes systems hard to
reason about for gains nobody checked. This one exists because
[33](33-load-testing.md) produced a number: the capture path costs three database
round trips, the chain measurement put the ceiling at 919 captures/s with all three
and 1,520 with two, and the one being removed is a lookup of a row that changes once
in its life. The alternative — leave it — was genuinely on the table at 690 req/s.
**Wrong call if** the read rate were low, the write rate high, or staleness
intolerable. Those three are the whole test for whether a cache fits, and the inbox
row passes all three comfortably.

**In `internal/ingest`, not inside `store.EndpointBySlug`.**
Caching inside the store method would be invisible and automatic, which sounds like
an advantage and is the problem: every current and future caller would silently get
a stale answer, including callers for whom that is wrong. `EndpointBySlug` has
exactly one caller today and it is the capture path; when the second one arrives it
should have to decide for itself. Keeping the cache in the consumer also keeps the
store honest — a function named `EndpointBySlug` that sometimes does not look up the
endpoint is a small lie that costs somebody an afternoon eventually.
**Wrong call if** several packages end up needing the same cached read, at which
point a cache *type* shared between them beats three copies — but still not a cache
hidden inside the query.

**Not an authorisation decision, and the boundary is deliberate.**
`AuthenticateEndpoint` and `AuthenticateRequest` are untouched and still hit the
database every time. Resolving a slug grants nothing — a capture URL is public by
design ([08](08-capability-urls.md)), which is exactly why it is safe to cache.
Caching the token check would mean a revoked token stays valid for up to one TTL,
and "we revoke tokens, on a five-second delay, usually" is not a property worth
trading for a database round trip on a path nobody hammers.

**TTL of 5 seconds, chosen against a specific failure.**
Not "5 seconds feels short". The thing that actually goes wrong with a stale entry
is an inbox being deleted while captures keep arriving, and five seconds of that is
invisible to a person and a rounding error against a provider's retry schedule.
Longer buys nothing measurable: at 1,000 req/s a 5s TTL already serves 99.98% of
lookups from memory, so going to 60s improves the hit rate by 0.02% while multiplying
the deleted-inbox window by twelve.
**Wrong call if** something starts reading the cached `Endpoint`'s mutable fields.
`LastSeenAt` is on that struct and a cached copy is stale by construction — nothing
in the capture path reads it, and the moment something does, this decision needs
revisiting rather than the field needing a workaround.
**The signal to switch** is a delete endpoint shipping: at that point `ForgetEndpoint`
should be called from it, and the TTL becomes the backstop rather than the mechanism.

**Negative results are cached, at a shorter TTL.**
The argument for: without it, a scanner walking random subdomains bypasses the cache
completely and puts the full lookup rate on Postgres — the traffic you least want to
serve from the database is the traffic that skips the cache. The argument against: a
cached 404 for a slug that then exists. In practice that cannot happen, because slugs
are 128 bits of randomness and nobody POSTs to one before it is created; one second
is short enough that I do not have to be certain of that.
**Wrong call if** slugs ever become user-chosen. Then "claim `stripe-test`, POST to
it" is a real sequence and a cached 404 is a real bug.

**Refuse admission when full, rather than LRU.**
The interesting decision in the file. LRU is the default answer and it is the wrong
one here, because the realistic way this map fills is somebody enumerating
subdomains: under LRU every garbage key evicts a real inbox, so an attacker turns the
cache off for everyone else while filling it with their own misses — a cache-thrashing
attack that costs them nothing. Refusing admission when full means established
inboxes keep their entries and the scan degrades to exactly the pre-cache behaviour,
which is the correct worst case. It is not a permanent freeze either: entries expire
within seconds and `put` clears expired ones before giving up, so the population
still turns over.
**Wrong call if** the working set legitimately exceeds the cap, where LRU's
adaptiveness is the point. At 10,000 inboxes that is not this system yet, and the
`len()` is observable when it becomes one.

**No singleflight, and the stampede is acknowledged rather than prevented.**
When a hot entry expires with requests in flight, several of them miss and run the
same query. `golang.org/x/sync/singleflight` exists for exactly this and was
rejected: with one busy inbox the herd is a handful of duplicate 0.25ms lookups every
five seconds, and the dependency plus the coordination costs more than it saves.
Recording it because "we thought about the stampede" and "we did not think about the
stampede" produce identical code, and only one of them is engineering.
**The signal to add it** is a lookup that becomes expensive — a join, a remote call —
or a TTL short enough that expiry and peak traffic coincide regularly.

**Transient errors are not cached.**
`resolve` caches the not-found sentinel and the success, and deliberately nothing
else. Caching a database timeout would turn a blip into seconds of guaranteed
failure, and the retry that would have succeeded never reaches the database to find
out. Negative caching means "this does not exist", never "this failed".

**Injectable clock.**
`endpointCache.now` is a field, like `ratelimit`'s in [31](31-rate-limiting.md). A
cached value is a function of the clock, so a test of one either injects time or
sleeps — and a test that sleeps is a test that is slow now and flaky later. None of
the seven cache tests sleep.

---

## Walkthrough

### `internal/ingest/cache.go`

**The tuning constants (`:15`).** Constants, not configuration. These are not numbers
an operator has any basis for choosing, and every knob in a config file is a knob
somebody sets wrong at 3am. Each one carries the failure it was chosen against rather
than a number that "felt right" — that is the difference between a tuned system and a
system with magic numbers in it.

**`endpointCache` (`:61`).** A map, a mutex, and an injectable clock. The doc comment
does the two jobs a cache's doc comment has to do: say why this data is safe to cache
(high read rate, near-zero write rate, tolerable staleness) and say explicitly what
is *not* cached (the token checks) so nobody extends it there by analogy.

**`cacheEntry.ep` being nillable (`:71`).** A nil endpoint is a cached "no such
inbox", which is a real answer rather than an absence. The comment says why the
negative entry earns its place: it is precisely the traffic you least want on the
database — a scanner's misses — that would otherwise skip the cache entirely.

**`get` (`:94`) and its three outcomes.** `(nil, false)` is a miss, `(ep, true)` is a
hit, `(nil, true)` is a hit on a cached miss. One more outcome than a cache usually
has, and they cannot be collapsed: the caller must distinguish "I do not know" from
"I know it does not exist".

The expiry check is `!c.now().Before(e.expires)` rather than `c.now().After(...)`, so
an entry expiring exactly on the boundary is treated as expired. `TestCacheHitAndExpiry`
pins both sides of that boundary to the nanosecond, because an off-by-one here is
invisible in production and changes how long a deleted inbox keeps answering.

Expired entries are deleted **on read** (`:105`). That is the cheapest eviction
available: it happens exactly where the entry is proven stale, costs a map delete on
a path that was already taking the lock, and needs no timer.

**`put` (`:114`) and the admission policy.** The load-bearing block of the file:

```go
if len(c.entries) >= maxEndpointEntries {
    if c.evictExpiredLocked() == 0 {
        return
    }
}
```

When full, clear what has expired; if that frees nothing, *decline to store this
entry* rather than evicting a live one. This is the opposite of LRU and it is
deliberate — see the decision above. What breaks if you change it to LRU: a scan of
20,000 random subdomains evicts every real inbox, and the cache stops helping the
people it exists for while doing nothing to slow the scanner.
`TestFullCacheRefusesAdmissionRatherThanEvicting` fills the cache from a simulated
scan and asserts the established entry is still there; `TestExpiredEntriesMakeRoom`
asserts the refusal is not permanent.

**`forget` (`:149`).** Explicit invalidation, three lines. There is no delete-inbox
feature yet, and the hook exists now because it is much easier to add while the cache
is being written than to remember during the feature that needs it.

**`EvictExpired` (`:156`) and `evictExpiredLocked` (`:162`).** The split is the usual
Go convention and it matters here: `put` already holds the mutex when it needs to
evict, so the two entry points cannot both take it. Calling the exported one from
`put` would deadlock on a non-reentrant `sync.Mutex` — the classic way this bug ships,
because it only triggers when the cache is full.

**`resolve` (`:192`).** The integration point.

```go
if ep, hit := h.endpoints.get(slug); hit { ... }
ep, err := h.store.EndpointBySlug(ctx, slug)
```

Note what is absent: no lock is held across the database call. Holding one would
serialise every capture behind a single query — reproducing, in a new place, exactly
the contention unit 33 removed. The price is a small stampede when a hot entry
expires with requests in flight, which is acknowledged in the comment and measured at
"a handful of duplicate 0.25ms lookups every five seconds".

The `switch` (`:202`) is the part worth reading twice. `ErrNotFound` is cached; any
other error is **not**. Caching a database timeout would turn a blip into seconds of
guaranteed failure, and the retry that would have succeeded never reaches the
database to find out. Negative caching means "this does not exist", never "this
failed" — and the two look identical at the call site if you are not careful, which
is why they get separate cases rather than one `if err != nil`.

**`ForgetEndpoint` (`:227`) and `EvictEndpointCache` (`:234`).** The exported surface,
which is two lines because everything interesting is unexported.

### Wiring

- `internal/ingest/ingest.go:86` — the field; `:103` — constructed in `New`, so a
  `Handler` is never in a state where `resolve` would nil-panic.
- `internal/ingest/ingest.go:150` — the call site. The comment above it changed from
  "costs one indexed lookup" to "costs one indexed lookup, or, most of the time, a map
  read", because a comment that describes the old cost is worse than no comment.
- `internal/server/server.go:483` and `cmd/hooklens/main.go:134` — eviction on the
  sweeper's existing tick, the third thing hung off it after the rate limiters and the
  last-seen bookkeeping. Still one goroutine.

---

## Verified

`go build ./...`, `go vet ./...`, `go vet -tags loadtest ./...`, `gofmt -l .` clean.
`go test ./... -count=1` green.

### The cache's own tests

`internal/ingest/cache_test.go`, seven tests, none of which sleep — the clock is
injected, as in [31](31-rate-limiting.md):

- `TestCacheHitAndExpiry` — hit, then the TTL boundary pinned to the nanosecond on
  both sides.
- `TestNegativeCaching` — a cached "no such inbox" is a hit returning nil, and it
  expires at `negativeTTL`, not `endpointTTL`.
- `TestForgetIsImmediate`.
- `TestEvictExpiredReachesQuietInboxes` — the sweep reaches entries that expiry-on-read
  never will, because nothing asks for them again.
- `TestFullCacheRefusesAdmissionRatherThanEvicting` — 20,000 scan keys against a
  10,000 cap; the established inbox is still cached afterwards.
- `TestExpiredEntriesMakeRoom` — the refusal is not permanent.
- `TestCacheIsConcurrencySafe` — 50 goroutines × 200 iterations across all four
  operations.

### Load test, same command, third run

```
scheduled 60000   sent 51577   saturated 8423 (14%)
status 200 51577  transport 0
p50 489ms   p95 649ms   p99 2.23s
accepted 51577 · rows in postgres 51577 · metric stored 51577
```

**690 → 860 req/s**, and zero dropped captures again — the three counts identical for
the fourth run in a row. p50 fell from 723ms to 489ms.

The three runs together:

| | achieved | saturated | p50 | dropped |
|---|---|---|---|---|
| before unit 33's fix | 280/s | 72% | 1.81s | 0 |
| after coalescing the touch | 690/s | 31% | 723ms | 0 |
| after caching the lookup | 860/s | 14% | 489ms | 0 |

3.1x in two changes, both of which deleted a database round trip rather than adding
anything.

### Ruling out the generator

Before claiming the remaining limit is the server, the obvious alternative had to go:
the generator runs in the same process on the same CPU, and 256 busy workers could
themselves be the ceiling. Re-running with `HOOKLENS_LOAD_WORKERS=512`:

```
sent 53720   saturated 6280 (10.5%)   p50 998ms   p95 1.28s
```

895 req/s — **4% more throughput for twice the workers, and p50 roughly doubled.**
That is the signature of a saturated downstream, not a starved generator: the extra
workers did not get more work done, they queued. If the generator had been the limit,
throughput would have scaled and latency would have fallen. So the remaining
bottleneck is genuinely below the HTTP layer.

### Where the remaining ~900/s comes from, and what is left

Two database writes per capture survive: the insert, and `RecordForward` writing the
delivery outcome. Unit 33's chain measurement put insert+forward at ~1,195
captures/s on this machine, and 860–895 observed against that is the ~75% efficiency
the HTTP layer costs — consistent, not coincidental.

Removing the third round trip means not writing the forward outcome synchronously:
batching it, or deriving "no tunnel" from the absence of a row rather than storing it
60,000 times. Both are real changes to what the UI can promise about delivery state,
so neither is a load-test tweak, and neither is being done on the way past.

**The 1,000 req/s target is still not met on this machine and the saturation
assertion still fails.** I would rather leave it red than tune the threshold to match
the hardware. What every run does establish, unchanged across all four: the server's
2xx count, the `hooklens_captures_total` metric and the row count in Postgres were
identical every single time. Not one acknowledged capture was ever lost.
