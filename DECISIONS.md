# Decisions

Every significant design decision in hooklens, one line each: what was chosen, what it
beat, and the signal that would make it the wrong call. The reasoning in full is in
[`docs/learn/`](docs/learn/README.md); the bracketed number is the note.

> Drafted by Claude at the author's request, from the learn notes. `CLAUDE.md` intended
> this file to be written by hand from memory as a recall exercise; the author chose
> otherwise. For recall practice, use [`docs/QUIZ.md`](docs/QUIZ.md) instead.

---

## Phase 0 — Skeleton

| Decision | Over | Wrong when |
|---|---|---|
| One binary serves capture, API and UI, split by `Host` [01] | Separate services | More than one replica is needed — a tunnel on instance A is unreachable from B |
| Routing is a pure function `Resolve(base, host, path)` [01] | Logic inside the handler | — |
| Path form `/e/{slug}/` alongside subdomains, from day one [01, 05] | Subdomains only | It becomes the primary form; some providers reject paths |
| Slug whitelist `[a-z0-9-]{3,32}`, repeated as a DB `CHECK` [01, 03] | Blacklisting bad characters | — |
| `/_hooklens/` reserved on inbox hosts; every other path captured [01] | A `/healthz` on inbox hosts | — |
| Liveness does not check Postgres [01] | Dependency checks in liveness | — |
| `ReadHeaderTimeout`, not `ReadTimeout` [01] | Capping the whole request | — |
| Compose runs Postgres only [02] | App in Compose too | A Linux-only dependency appears |
| `distroless/static:nonroot` runtime image, 14.7 MB [02] | Alpine (has a shell), scratch (no CA certs, no non-root user) | — |
| Exec-form `ENTRYPOINT` [02] | Shell form, which swallows SIGTERM | — |
| goose as a library: 2 direct / 8 indirect deps [03] | goose CLI: 60 indirect deps | Scaffolding commands become worth that graph |
| Migrations embedded with `go:embed` [03] | Shipping a `migrations/` directory | — |
| `timestamptz` everywhere [03] | `timestamp` | — |
| CI against a real Postgres service container [04] | SQLite or mocks | — |
| Wildcard TLS via DNS-01 [05] | HTTP-01, which cannot prove a wildcard | — |

## Phase 1 — The mailbox

| Decision | Over | Wrong when |
|---|---|---|
| Body read through `LimitReader(maxBody + 1)`, 1 MiB cap [06] | `ReadAll`; trusting `Content-Length` | — |
| Oversized body kept, truncated and flagged [06] | Rejecting it | Truncation becomes common enough that the prefix is useless |
| Remainder of an oversized body not drained [06] | Draining for keep-alive | — |
| Body stored as `bytea` [07] | `jsonb` (reorders, dedupes), `text`/`json` (reject NUL) | SQL queries inside bodies are needed — add a generated column beside it |
| Headers as a `jsonb` array of `{name, value}` [06, 07] | `jsonb` object; header table; `map[string]string` | — |
| `Host` and `Transfer-Encoding` restored by hand [06] | `range r.Header` | — |
| `X-Forwarded-For` ignored unless `HOOKLENS_TRUST_PROXY=1` [06, 31] | Trusting it | — |
| One index, `(endpoint_id, received_at desc, id desc)` [07, 09] | Speculative indexes | Revisited once: see [37] |
| Resolve the inbox before reading the body [07] | Reading first | — |
| 503 on a database failure before storing [07] | 500 or 200 | — |
| Two secrets: public-ish slug, hashed token [08] | One URL that captures and reads | — |
| SHA-256 for tokens [08] | bcrypt/argon2 | Tokens become user-chosen |
| Identical 401 for every auth failure [08] | Distinct "no inbox" / "bad token" | — |
| Lookup by slug, constant-time compare in Go [08] | `WHERE token_hash = $1` | — |
| base32 slug, base64url token [08] | base64 in a hostname | — |
| Cursor pagination on `(received_at, id)` [09] | OFFSET | A small bounded list with numbered pages |
| Retention sweep as an in-process goroutine, batched [10] | Cron container | More than one instance, or a sweep with external side effects |

## Phase 2 — The inspector UI

| Decision | Over | Wrong when |
|---|---|---|
| Vite + React SPA embedded in the binary [11] | Next.js | A public surface needs SEO |
| `/api/` catch-all returning JSON 404 [11] | SPA fallback answering HTML | — |
| `immutable` hashed assets, `no-cache` `index.html` [11] | One cache policy | — |
| No CORS headers, enforced by a test [12] | CORS middleware | An API is served to a page we do not host |
| Vite dev proxy [12] | `VITE_API_BASE_URL` plus dev-only CORS | — |
| SSE for the live list [13] | Polling; WebSocket | — |
| `fetch` + `ReadableStream` [13] | `EventSource`, which cannot send headers | Resume-from-last-event is needed |
| Per-stream write deadline cleared [13] | Removing the global `WriteTimeout` | — |
| Broker: buffer 32, non-blocking send, drops counted and shown [14] | Blocking; disconnecting | Drops become common — switch to disconnect-and-resync |
| TanStack Query for server state, `useState` for the inbox [15] | Hand-rolled cache | — |
| `setQueryData` on live events, with a duplicate guard [15] | `invalidateQueries` | Events become lossy or partial |
| JSON tree depth cap of 64; collapsed nodes not rendered [16] | Unbounded recursion; CSS hiding | Find-in-page over collapsed text is required |
| `<Detail key={selected}>` [16] | Reusing the component | — |

## Phase 3 — The tunnel

| Decision | Over | Wrong when |
|---|---|---|
| WebSocket for the tunnel [18] | SSE, long-polling | — |
| `coder/websocket` [18] | `gorilla/websocket` (no context) | — |
| One JSON object per WebSocket message [18] | NDJSON inside frames | — |
| Auth in the first frame, 10 s handshake deadline, 4 KiB read limit [18] | `Authorization` header | — |
| Mutex-guarded `map[id]chan` [19] | A goroutine owning the map | The registry gains expiry, quotas or fairness |
| Response channels buffered with one slot [19] | Unbuffered | — |
| `defer forget(id)` right after registration [19] | Cleanup on the success path | — |
| One `done` channel woken on disconnect [19] | Closing each response channel | — |
| Newest connection wins; being replaced is permanent [19, 25] | Refusing the second connection | — |
| Synchronous forwarding [20] | Async delivery | — |
| Delivery failure → 200 + `X-Hooklens-Forward`; app 5xx relayed [20] | 502 on delivery failure | — |
| Forward on a detached context, 30 s [20] | The request's context | — |
| CLI rewrites `Host`, no redirects, no auto-gzip [21] | Transparent proxying defaults | — |
| Full jitter, 500 ms → 30 s; reset after 30 s connected [22] | No jitter; reset on connect | A predictable worst-case retry time is required |
| Permanent errors not retried, decided by close code [22] | Retrying everything | — |
| Semaphore of 32 per tunnel; excess waits [23] | Reject; drop; process-wide limit | `overloaded` appears regularly |
| CLI acquires inside the request goroutine, never the read loop [23] | Acquiring in the read loop | — |
| Truncated captures not forwarded [24] | Forwarding what was kept | hooklens becomes a delivery relay |
| Frame limit derived from body limit (`× 4/3 + 256 KB`) [24] | One shared limit | — |

## Phase 4 — The wedge features

| Decision | Over | Wrong when |
|---|---|---|
| HMAC, compared with `hmac.Equal` [26] | `sha256(secret + body)`; `==` | — |
| Results always carry the canonical string, expected and provided tags [26] | A boolean | — |
| Verify against arrival time; window checked after the signature [26] | Wall clock; window first | — |
| Any matching Stripe `v1` accepted [26] | First only | — |
| Replay to arbitrary URLs, guarded in `Dialer.Control` [27] | Tunnel-only; pre-checking the hostname | Multi-tenant at scale — prefer an allowlist |
| `Unmap()` plus explicit extra ranges (CGNAT) [27] | Trusting `IsGlobalUnicast` | — |
| Redirects refused, `Proxy: nil` [27] | Following redirects; environment proxy | — |
| `X-Hooklens-Replay: 1` on every replay [27] | Unmarked replays | — |
| Structural diff with `UseNumber()` and sorted keys [28] | Text diff; float64 decoding | — |
| Arrays matched by unique id, else by index [28] | Index only | — |
| Volatile headers hidden by default, and reported as hidden [28] | Silent filtering | The signature is what is being investigated — use `?volatile=1` |
| Signing secret in component state only [29] | `localStorage` | Users re-enter it constantly — add an explicit opt-in |
| Verify and replay are mutations; diff is a query [29] | Treating all as queries | — |

## Phase 5 — Hardening and release

| Decision | Over | Wrong when |
|---|---|---|
| Token bucket, lazy refill [31] | Fixed window; sliding log | — |
| Per-IP for creation (10/min), per-inbox for capture (50/s) [31] | One global limit | — |
| Capture limit checked before the body read and the insert [31] | After storing | — |
| Hand-rolled Prometheus exposition, no globals [32] | `client_golang` (34 modules) | Exemplars, native histograms or protobuf are needed |
| Status class labels, never path [32] | Per-status; per-path | Per-route insight is needed — use a bounded route label |
| Known series zero-initialised [32] | Emitting on first use | — |
| Open-loop load generator, saturation counted separately [33] | Closed loop | The modelled clients genuinely wait |
| Durability asserted against Postgres row count [33] | Against the handler's own metric | — |
| Load test behind a build tag, not in CI [33] | Every `go test` / CI run | A regression slips between releases — add a short CI smoke run |
| `last_seen_at` written at most once per inbox per minute [33] | Every capture (row-lock contention) | Something acts on `last_seen_at` within the minute |
| Inbox lookup cached 5 s, negatives 1 s, errors never [34] | No cache; caching auth | Slugs become user-chosen |
| Full cache refuses admission [34] | LRU (thrashable by a scan) | The legitimate working set exceeds the cap |
| No singleflight [34] | Coalescing duplicate misses | The lookup becomes expensive |
| GoReleaser, `CGO_ENABLED=0`, `-trimpath`, 5 targets [35] | Hand-rolled build matrix; `windows/arm64` | — |
| `hooklens version` dispatched before config loads [35] | After | — |
| Lint errors on `Close` discarded explicitly [35] | `exclude-functions` in lint config | — |

## Phase 6 — Launch

| Decision | Over | Wrong when |
|---|---|---|
| README ordered by the reader's questions; unmet goals stated [36] | Status-first developer README | — |
| ASCII architecture diagram [36] | Mermaid | Diagram maintenance blocks documentation |
| `expires_at` stored and indexed; `SetRetention` rewrites it in a transaction [37] | Join-dependent sweep predicate (276 ms full scan) | Retention changes often |
| Index built inside the migration transaction [37] | `CREATE INDEX CONCURRENTLY` | The table is large enough that the lock is an outage |
| MIT licence, shipped inside every archive [38] | Apache-2.0; AGPL-3.0 | Hosted resale becomes a concern |
| Not hosted [39] | Fly.io; a VPS | A reason to spend money, or a free always-on tier that does not scale to zero |
| Forward outcome sent as a second live `delivery` event, after it is stored [25] | Delaying the capture event until after the forward | — |
| Compare hint's line always reserved; only its text toggles [36] | Rendering the line on selection (rows jumped under the cursor) | — |
| Browser stream stops on 401/403 and shows "inbox not found" [40] | Retrying every failure forever | The server uses 401 for something transient |
| CLI inspect link carries the inbox in the URL fragment [40] | Query string (logs, Referer); one-time exchange code | hooklens is hosted and links get pasted into chat |
| A link asks before replacing a different stored inbox [40] | Silently switching (the old token is unrecoverable) | The browser remembers several inboxes |
| Quickstart written one command per line, PowerShell blocks only for HTTP requests [36] | A parallel Windows section | — |
