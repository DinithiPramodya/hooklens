# Learn notes

How each piece of hooklens works, why it was built that way, and what was rejected.
Written as it was built, so the numbering is **build order** — which is not the same as
phase order (note 05 was deferred and written after 04's phase had otherwise finished).

Each note has three parts: a **Brief** written before any code, the **Decisions** made
while building with their rejected alternatives, and a **Walkthrough** of the resulting
code with `file:line` references.

Pair these with [`../QUIZ.md`](../QUIZ.md), which has one section per phase: read the
notes below, answer that phase's questions without looking, then check.

---

## Phase 0 — Skeleton and a live URL

| | Note | Covers |
|---|---|---|
| 01 | [Webhooks, and the HTTP underneath](01-webhooks-and-http.md) | Polling vs. push · HTTP from the bytes up · host-based routing |
| 05 | [DNS, and the certificates that ride on it](05-dns-and-tls.md) | Resolution and delegation · A/CNAME/TXT/wildcard · what a certificate proves · why wildcards need DNS-01 |
| 02 | [Containers, images, and Compose](02-containers.md) | Namespaces and cgroups · images vs. containers · layer caching · PID 1 and SIGTERM |
| 03 | [Schema migrations](03-migrations.md) | Schema as code · ordering and append-only · up/down · transactional DDL |
| 04 | [Continuous integration](04-ci.md) | What a runner is · service containers · why lint and test gate a merge |

*Note 05 is listed second because that is where it belongs conceptually — it was written
last, after the domain question was settled.*

**Quiz:** [Phase 0](../QUIZ.md#phase-0--skeleton-and-a-live-url) — 8 questions, answered and assessed.

---

## Phase 1 — The mailbox

| | Note | Covers |
|---|---|---|
| 06 | [Reading a request body](06-reading-a-request.md) | `io.Reader` · bounded reads and why unbounded is a DoS · why raw bytes matter · what `net/http` does to headers |
| 07 | [Storing a request](07-storing-a-request.md) | `jsonb` vs `json` vs `text` vs `bytea` · UUIDv7 and index locality · what a composite index actually does |
| 08 | [Capability URLs](08-capability-urls.md) | Entropy · why unguessable is enough · why tokens are hashed, and why *not* with bcrypt |
| 09 | [Cursor vs offset pagination](09-pagination.md) | Why `OFFSET 10000` reads 10,000 rows · drift on a live list · composite cursors |
| 10 | [Background workers](10-background-workers.md) | Goroutine cost vs threads · tickers and dropped ticks · why a panic here kills the process · shutdown ordering |

**Quiz:** [Phase 1](../QUIZ.md#phase-1--the-mailbox) — 12 questions with model answers.

---

## Phase 2 — The inspector UI

| | Note | Covers |
|---|---|---|
| 11 | [SPA vs server-rendered, and `go:embed`](11-spa-and-go-embed.md) | Why nothing here needs SSR · the SPA fallback · what embedding does to the binary · cache headers in opposite directions |
| 12 | [What CORS actually is](12-cors.md) | Why the *browser* enforces it · preflights and what triggers them · why single-origin removes the category |
| 13 | [Server-Sent Events](13-server-sent-events.md) | The wire format · why you must flush · reconnection and why we hand-rolled it · `Unwrap` and wrapped ResponseWriters |
| 14 | [In-process pub/sub](14-pubsub.md) | Fan-out and why one channel will not do · the four answers to a slow consumer · why the send is under the lock |
| 15 | [Server state, and TanStack Query](15-server-state.md) | Client vs server state · the query key is the cache identity · why setQueryData beats invalidate here |
| 16 | [Recursive rendering](16-recursive-rendering.md) | Why a loop cannot render a tree · the depth cap and why a stack overflow is not catchable · collapsed children are absent, not hidden · `key` as a remount |

**Quiz:** [Phase 2](../QUIZ.md) — 10 questions.

---

## Phase 3 — The tunnel

*The deep one: the concepts here are the ones the whole project is built to demonstrate.*

| | Note | Covers |
|---|---|---|
| 17 | [NAT and firewalls](17-nat-and-firewalls.md) | Why the internet cannot reach your laptop · translation rows are created by outbound packets · connection tracking and why rows expire · what the dev machine actually shows |
| 18 | [WebSocket](18-websockets.md) | An HTTP request that stops being HTTP · the `Upgrade` handshake and frames · why not SSE here · a cancelled read context closes the connection |
| 19 | [Multiplexing and correlation](19-multiplexing-and-correlation.md) | Why HTTP gives correlation for free and this does not · the `map[id]chan` pattern · why a mutex, not a manager goroutine · every exit path deletes its entry · waking waiters the instant a tunnel drops |
| 20 | [Forwarding](20-forwarding.md) | Why forwarding is synchronous and what that costs · delivery failure vs application failure · why a dead local app must not become a 502 · recording the outcome |
| 21 | [The CLI](21-the-cli.md) | A reverse proxy with an unusual transport · why the `Host` header must be rewritten · not following redirects, not decompressing · tokens on disk, permissions and atomic writes |
| 22 | [Backoff and jitter](22-backoff-and-jitter.md) | Why a fixed retry delay causes the outage it is recovering from · exponential fixes rate, jitter fixes synchronisation · resetting on connect is a bug · what must never be retried |
| 23 | [Bounded concurrency](23-bounded-concurrency.md) | A semaphore is a buffered channel · wait vs reject vs drop · a bounded worker count with an unbounded queue is not bounded · never acquire on the path that must stay responsive |
| 24 | [Size limits](24-size-limits.md) | Truncation is a corrupt payload that looks complete · a frame limit is not a body limit · why base64 arithmetic closed the tunnel · limits that cannot be raised are bugs |
| 25 | [Showing delivery](25-showing-delivery.md) | Three states, not two · why "not attempted" must not look like failure · an app 500 is not a delivery failure · the livelock two CLIs caused |

**Quiz:** [Phase 3](../QUIZ.md) — 18 questions.

---


---

## Phase 4 — The wedge features

*Replay, edit-and-replay, structural diff and signature verification for four providers.*

| | Note | Covers |
|---|---|---|
| 26 | [Hashes, MACs and HMAC](26-hmac.md) | Why a plain hash proves nothing · length extension, and why hashing secret-then-body is broken · MAC vs signature · timing attacks and constant-time comparison |
| 27 | [Replay and SSRF](27-replay-and-ssrf.md) | The feature and the attack are the same bytes · why the guard must run at the dial, not before · DNS rebinding · what `IsGlobalUnicast` does not mean |
| 28 | [Structural diff](28-structural-diff.md) | Why a text diff of JSON is noise · sorting the key union is correctness · matching array elements by id vs index · UseNumber and the 2^53 trap |
| 29 | [Mutations and secrets](29-mutations-and-secrets.md) | Why a query and a mutation are different things · a replay that runs twice cannot be undone · where a signing secret may live in a browser |
| 30 | [Diagnosing flaky tests](30-diagnosing-flaky-tests.md) | What the SHAPE of an intermittent failure rules out · a precondition that skipped itself · an httptest teardown deadlock · an RST discarding the frame that explained it |

**Quiz:** [Phase 4](../QUIZ.md) — 20 questions.

---


---

## Phase 5 — Hardening and release

*Two of these units were written because a measurement demanded them. 33 set out to
verify a throughput target and instead found that a `last_seen_at` column was costing
the pipeline 2.5x; 34 exists because 33's diagnosis named the next thing to remove.
The phase went 280 → 860 req/s without adding anything — both fixes deleted a
database round trip.*

| | Note | Covers |
|---|---|---|
| 31 | [Rate limiting](31-rate-limiting.md) | Fixed window vs sliding log vs token bucket · lazy refill · per-IP vs per-inbox and what each defends against · why `X-Forwarded-For` is a configuration question |
| 32 | [Metrics](32-metrics.md) | Counters vs gauges vs histograms · the Prometheus text format, written by hand · cumulative buckets and the `+Inf` slot · why p99 and not the mean · cardinality, and why a path label kills a scraper |
| 33 | [Load testing](33-load-testing.md) | Coordinated omission and open-loop generators · the five meanings of "dropped" · measuring the bottleneck instead of guessing · why a thousand updates a second to one row serialise |
| 34 | [Caching and invalidation](34-caching-and-invalidation.md) | TTL vs explicit invalidation vs eviction · caching negatives · the stampede · why refusing admission beats LRU under a scan · never caching an authorisation decision |
| 35 | [Release and distribution](35-release-and-distribution.md) | Cross-compilation without a toolchain per target · why `CGO_ENABLED` defaults differently on different machines · reproducible builds · what a Homebrew tap actually is · checksums as the security boundary |

**Quiz:** [Phase 5](../QUIZ.md) — 16 questions.

---

## Phase 6 — Launch

*In progress.*

| | Note | Covers |
|---|---|---|
| 36 | [The README as a product page](36-the-readme-as-a-product-page.md) | Ordering by the reader's questions, not the system's structure · time-to-first-success · why the demo outranks every paragraph · a status line that does not rot |
| 37 | [Why the retention sweep could not use an index](37-indexable-predicates.md) | Sargable predicates · why a filter that depends on a joined row defeats both the index and the `LIMIT` · denormalising to make a predicate indexable, and who then maintains the copy · 276ms → 0.75ms |
| 38 | [What a licence actually does](38-licensing.md) | Copyright is automatic and default-deny · a licence as a conditional grant, not a transfer · why breaching a condition is infringement and not breach of contract · the warranty disclaimer as the author-facing clause · why relicensing gets more expensive with every contributor |
| 39 | [Where it runs](39-where-it-runs.md) | Deploy config as reproducible infrastructure · a platform vs. a rented box, layer by layer · why the wildcard forces DNS-01 either way · scale-to-zero as a correctness bug for capture · the single-replica constraint the tunnel imposes |

---

## Still to come

**The rest of Phase 6**, all of it the author's: the demo GIF, `DECISIONS.md`
(written by hand, from memory, per `CLAUDE.md`), and the launch posts.

Dogfooding on a hosted instance is **deliberately not happening** — hosting costs
money and this is a portfolio project. [39](39-where-it-runs.md) records what that
gives up and the zero-cost way to revisit it.

One thing still blocks a packaged release: **the tap and bucket do not exist.**
`homebrew-tap` and `scoop-bucket` are separate GitHub repositories, each needing a
PAT in this repository's secrets. The licence, the other former blocker, is settled
— MIT, see [38](38-licensing.md).
