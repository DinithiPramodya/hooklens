# 31 — Rate limiting: token buckets, and choosing what to count

*Phase 5, unit 1. Covers the curriculum bullet "rate limiting: token bucket, per-IP vs
per-endpoint, what each defends against".*

## Brief

**What is this thing?** A cap on how often something may happen, enforced by the server
and applied per *somebody* — per IP, per inbox, per account.

**What problem does it exist to solve?** Every endpoint built so far is unauthenticated at
the point of arrival. `POST /api/endpoints` creates an inbox with no credential, and the
capture endpoint accepts anything anyone sends. Both are deliberate and both are free to
abuse: a loop creating inboxes fills the database, and a loop posting 10 MB bodies fills
it faster.

Rate limiting is also the **cheapest** defence, because it runs before the expensive work.
A limiter that rejects at 50 microseconds protects a handler that would have spent 50
milliseconds on a database write.

**How does it actually work underneath?** Three approaches, and the differences matter.

A **fixed window** counts requests per calendar minute. Simple and wrong at the edges: a
client sending its whole allowance at 11:59:59 and again at 12:00:00 gets **double** the
limit in one second, which is precisely the burst you were trying to prevent.

A **sliding log** keeps the timestamp of every request and counts those inside the window.
Exact, and it stores one entry per request per client — unbounded memory driven by the
attacker.

A **token bucket** holds up to *N* tokens and refills at *R* per second. A request takes a
token or is refused. It costs **two numbers per client** — the token count and the time it
was last updated — and it needs no timer, because refill is computed lazily on access:
tokens elapsed = (now − last) × rate, clamped to the bucket size.

That structure gives you two independent dials. The **rate** is the sustained throughput
you are willing to serve. The **burst** is how much unused allowance can accumulate, and
it is what makes the limiter tolerable for real traffic, which arrives in clumps.

**Per-IP or per-inbox?** They defend against different things and you generally want both.
**Per-IP** limits one source and is what stops a single machine flooding you — and it is
also the one an attacker escapes most easily, with a botnet or a cloud provider's address
pool. **Per-inbox** limits one *resource* regardless of who is hitting it: it stops one
noisy integration consuming the service, which is a fairness property rather than an
abuse one.

**What are the sharp edges?**

- **`RemoteAddr` is the proxy's address** once anything sits in front. Behind a load
  balancer, per-IP limiting silently becomes global limiting; trusting
  `X-Forwarded-For` blindly instead lets anyone forge their identity.
- **The map of buckets is itself unbounded** if nothing evicts. One entry per IP is a leak
  driven by whoever is attacking you.
- **A limiter on the wrong side of the durability line** turns a rate limit into lost
  webhooks.
- **429 must carry `Retry-After`**, or a well-behaved client cannot behave well.

**In hooklens:** a token bucket per IP on inbox creation, a second per inbox on capture,
lazy refill, and eviction of idle buckets by the existing sweeper.

## Decisions

**A token bucket, not a fixed window or a sliding log.**
A fixed window is wrong at the edges: a client spending its whole allowance at 11:59:59
and again at 12:00:00 gets **double** the limit in one second, which is precisely the
burst being prevented. A sliding log is exact and stores one entry per request per client
— unbounded memory, driven by the attacker. A token bucket costs **two numbers per key**
and needs no timer.

**Refill is lazy.**
No ticker, no goroutine, no per-key timer. Tokens that "would have" accumulated are
computed on access from `(now − last) × rate`, clamped to the burst. That is the entire
reason the structure is cheap, and it means an idle key costs nothing until it is touched.

**Rate and burst are separate dials.**
Rate is sustained throughput; burst is how much unused allowance can accumulate. A limiter
with `burst == rate` refuses two requests arriving in the same millisecond even when the
client is far under its sustained allowance — real traffic is clumpy, and such a limiter
is useless for anything but a synthetic load.

**A new key starts FULL.**
Starting empty would make every client's first request wait, which is indistinguishable
from the service being broken.

**Per-IP for creation, per-INBOX for captures.**
They defend against different things. `POST /api/endpoints` is unauthenticated by design —
there is nobody to authenticate yet — so a loop against it is free, and per-IP is the only
handle available. Captures are different: a provider is a small set of addresses hitting
many inboxes, so per-IP there would throttle legitimate traffic. Per-inbox limits the
*resource* regardless of who is consuming it, which is a fairness property rather than an
abuse one.
*What per-IP does not stop:* a botnet, or anyone with a cloud provider's address pool.
It raises the cost; it does not close the door.

**The capture limit sits ABOVE the durability line.**
A 429 means nothing was stored, so a provider's retry produces no duplicate. The same
check below the line would reject a capture we already hold — the Phase 0 quiz Q1 bug
wearing a rate limiter.

**Both checks run before the expensive work.**
Creation is limited before the body is read or the database is touched; captures are
limited after the inbox lookup (the key) and before the body read, so a refused request
never costs up to a megabyte. A limiter that runs after the work it was protecting has not
saved anything — rejecting in microseconds is the whole economic argument.

**429 always carries `Retry-After`, rounded UP.**
A 429 with no indication of when to retry leaves a well-behaved client guessing, and
guessing means retrying too soon — so the limiter generates exactly the load it exists to
prevent. Rounding *down* has the same effect: it tells the client to come back before a
token exists.

**`clientIP` is configuration, not a guess.**
The two failure directions are asymmetric and both are bad. Trusting `RemoteAddr` behind a
proxy turns per-IP limiting into **global** limiting, so one client exhausts everyone's
allowance. Trusting `X-Forwarded-For` blindly lets anyone forge their identity with a
header, which is worse than no limit because it looks like protection.
`HOOKLENS_TRUST_PROXY` defaults to **false**, so a misconfiguration under-counts distinct
clients rather than letting them forge. The first `X-Forwarded-For` entry is used, not the
last, because the header is appended to by each hop.
It is also **parsed** rather than taken as text: a header full of junk would otherwise
become an unbounded set of limiter keys, a memory leak reachable by anyone through a
header.

**Eviction runs on the sweeper's existing ticker.**
The bucket map grows one entry per distinct key, which is a leak driven by exactly the
traffic a limiter exists to handle — an attacker rotating addresses would fill memory
*through* the mechanism meant to stop them. A second goroutine would mean two things to
start, two to shut down, and two chances to get the ordering wrong; the sweeper already
handles cancellation and already recovers from panics, and `OnTick` inherits both.
A full bucket is safe to forget, because a new key starts full — so discarding an idle
bucket and recreating it later is unobservable.

**Zero in the config means "use the default".**
Learned by breaking six tests. `config.Config` built by hand — which every test does —
carries `RateCapture: 0`, and `ratelimit.New` clamps a zero rate to 1/sec. Every test
suddenly got a 429 on its second capture. The same contract `ingest.New` already applied
to `maxBody` now applies here, with the constants duplicated in `internal/server` so a
hand-made config behaves like a real one.

## Walkthrough

### `internal/ratelimit/ratelimit.go`

`Allow` (`:66`) — the new-key-starts-full branch, the lazy refill, and the `retryAfter`
calculation.

`Evict` (`:101`) and the argument for why forgetting a full bucket is free.

`now` is a field (`:26`) so tests use a fake clock. A limiter tested by sleeping is slow
*and* flaky — slow because the waits are real, flaky because a loaded CI machine sleeps
longer than asked.

### `internal/server/clientip.go`

The whole file is the `X-Forwarded-For` decision. `forwardedFor` (`:49`) takes the first
entry and parses it.

### `internal/server/server.go`

The zero-means-default block (`:107`), `tooManyRequests` with its round-up, and
`EvictLimiters`.

### `internal/ingest/ingest.go`

The limiter check (`:136`) with its position relative to both the body read and the
durability line stated in the comment.

### `internal/sweeper/sweeper.go`

`OnTick` and the reasoning for reusing the timer.

## Verified

**Eight limiter tests, all instant** — no sleeping anywhere, because `now` is injectable.
They assert the properties rather than the implementation: the first request from a new
key is allowed; a burst of three then a refusal with a usable `retryAfter`; half a second
at 2/sec yields exactly **one** token and not two; an hour idle yields exactly the burst
and not 3600 requests; two keys are independent; eviction removes idle keys and an evicted
key behaves like a new one.

`TestConcurrentAllow` runs **500 goroutines against a burst of 100** and asserts exactly
100 were allowed — a racy implementation hands out more tokens than exist.

**Twelve `clientIP` cases**, covering both failure directions: a forged `X-Forwarded-For`
is ignored when the proxy is untrusted, the *first* entry of a chain is taken when it is
trusted, junk in the header falls back rather than becoming a key, and `::ffff:1.2.3.4`
normalises so it cannot be used to double an allowance.

**Three integration tests**: creation is limited after its burst and carries a numeric
`Retry-After`; a second IP is unaffected by the first being exhausted; and a rate-limited
capture returns 429 **with no `X-Hooklens-Id`** and leaves fewer rows stored than were
sent — which is the durability-line property, checked rather than assumed.
