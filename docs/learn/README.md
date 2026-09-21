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

## Still to come

- **Phase 3 — the tunnel:** NAT, WebSocket upgrade, multiplexing and correlation, goroutine leaks, backpressure
- **Phase 4 — the wedge features:** hash functions, HMAC, timing attacks, replay windows, structural diff
- **Phase 5 — hardening:** rate limiting, metrics, load testing, testcontainers, distribution
