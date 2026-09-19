# Quiz log

End-of-phase quizzes, as required by `CLAUDE.md`. Questions are interview-grade and drawn
from that phase's material. Answers are recorded **verbatim** — they are recall, not
transcription, and their value is in being re-readable months later.

One section per phase, appended in order. Each phase lists the learn notes it draws
from, so this doubles as a study index: read the notes, then answer the questions
without looking, then check.

| Phase | Material | Quiz |
|---|---|---|
| 0 — Skeleton and a live URL | [01](learn/01-webhooks-and-http.md) · [02](learn/02-containers.md) · [03](learn/03-migrations.md) · [04](learn/04-ci.md) · [05](learn/05-dns-and-tls.md) | [below](#phase-0--skeleton-and-a-live-url) |
| 1 — The mailbox | [06](learn/06-reading-a-request.md) · [07](learn/07-storing-a-request.md) · [08](learn/08-capability-urls.md) · [09](learn/09-pagination.md) · [10](learn/10-background-workers.md) | pending (end of Phase 1) |

---

# Phase 0 — skeleton and a live URL

*Taken 2026-09-19. Material: units 1, 3, 4, 5 (`docs/learn/01`–`04`). Unit 2, DNS and TLS,
was deferred and is therefore not examined.*

**Result: 8/8 substantively correct.** Four answers went beyond what was taught. Two
contain an overconfident inference, corrected below. One surfaced a genuine design problem
in Phase 1 that had not been noticed.

All four code references cited from memory (`Dockerfile:47`, `routing.go:63`,
`server.go:82`, `server.go:39`) were checked and are exact.

---

## Q1. A provider POSTs to your capture endpoint. Your handler stores the request correctly but returns 500 by mistake. Describe what happens over the next 24 hours, and what that implies about how the handler must be written.

**Answer:**

> The provider doesn't know you stored it. The status code is the only thing it sees, so as
> far as Stripe is concerned that delivery failed, and it does what every provider does:
> retries on a backoff. Stripe keeps going for about three days, GitHub and Shopify have
> their own schedules, but the shape is the same — a few retries in the first minutes, then
> spreading out to hourly-ish. So over 24 hours that one event comes back maybe 10–20 times,
> and my handler cheerfully stores it correctly every single time. I end up with a dozen
> duplicate rows for one real event, and the inbox looks like the provider went haywire when
> the bug is entirely mine.
>
> Two other things happen. Every other event to that endpoint is also failing, so the retry
> backlog stacks up — I'm now taking multiples of my normal traffic. And providers disable
> endpoints that fail persistently; Stripe emails the account owner and eventually turns it
> off. So a wrong status code doesn't just duplicate data, it can get the integration
> switched off.
>
> What that implies about the handler: the status code is a promise, and it's the last thing
> I get to make. Concretely —
>
> - Nothing fallible runs between the successful durable write and the `WriteHeader(200)`.
>   If I store the row and then try to publish to the SSE bus and that errors, I must not let
>   that turn into a 500. The data is safe; say so. Side work happens after the response, or
>   asynchronously, and its failures are logged, not returned.
> - That includes panics. Right now `withRecover` (`internal/server/server.go:39`) turns any
>   panic into a 500 — which is correct as a default, but it means a panic after the insert
>   produces exactly this bug. The write path needs to be ordered so the response is
>   committed before anything that can blow up.
> - And the flip side: even a perfect handler can't avoid duplicates, because the response
>   can be lost on the network after I've written the row. Delivery is at-least-once, always.
>   So dedupe isn't a nice-to-have — I need to key on the provider's event ID where there is
>   one, and for hooklens specifically, show duplicates honestly rather than hide them, since
>   "why did this fire twice" is exactly the question the tool exists to answer.

**Assessment — correct, and the third bullet is the one that matters.**

Retry semantics, backlog amplification, and endpoint disabling are all right. The key
insight — that a perfect handler *still* cannot avoid duplicates, because the 200 can be
lost after the write commits — is the thing most people miss. Delivery is at-least-once as
a property of the network, not as a provider policy, and no amount of correctness in the
handler changes that.

"The status code is a promise, and it's the last thing I get to make" is the right framing.
The ordering rule that follows from it — nothing fallible between the durable write and the
response — is exactly the constraint Phase 1's capture path has to be built around.

Noticing that `withRecover` converts a post-insert panic into precisely this bug is a good
catch about our own code.

*Minor:* 10–20 retries in 24 hours is high for Stripe. Its schedule is roughly exponential
over ~3 days, so the first 24 hours is closer to 8–10. Directionally fine.

---

## Q2. Why is `map[string]string` the wrong type for HTTP headers? Name a header where it loses data.

**Answer:**

> Because HTTP headers aren't a map of name to value — the same field name can legitimately
> appear on multiple lines in one request. `map[string]string` can only hold one, so the
> second silently overwrites the first, and you don't get an error, you just get less data
> than arrived.
>
> The header that kills you is `Set-Cookie`. A response can carry several, one per cookie,
> and unlike most repeated headers you can't fold them into one comma-separated value —
> because `Set-Cookie` already contains commas inside itself, in the
> `Expires=Wed, 09 Jun 2027 ...` date. So there's no lossless way to squeeze multiple
> `Set-Cookie` lines into one string. Go's own `http.Header` is `map[string][]string`
> precisely for this. `WWW-Authenticate` has the same comma problem; `Via`, `Warning`, and
> `Forwarded` repeat but at least fold safely.
>
> There's a second loss that matters more for us than for most apps: a map has no ordering.
> It loses the order of distinct header names as they appeared on the wire, and Go
> canonicalises the casing on top of that (`x-github-event` becomes `X-Github-Event`). For a
> normal server, who cares. For an inspector, the whole point is showing the user exactly
> what the provider sent — if they're debugging a signature mismatch, "the header was spelled
> differently than you think" is a real answer I'd be destroying before they ever see it.
> That's why Phase 1 stores headers as an ordered list, not a map.

**Assessment — correct, and it exposed a hole in the plan.**

`Set-Cookie` is the right example and the reason given is the precise one: RFC 9110 permits
folding repeated fields into one comma-separated value, but `Set-Cookie` carries commas
inside its own `Expires` date, so folding is lossless for `Via` and `Warning` and lossy for
`Set-Cookie`. Most people know "headers can repeat" without knowing why `Set-Cookie` is the
one that breaks.

**The last sentence, however, describes something we cannot actually do.** Verified with a
raw-socket test against `net/http`:

```
sent on the wire:              what the handler received:
  zebra: first-on-the-wire       "Zebra"          -> ["first-on-the-wire"]
  x-github-event: push           "X-Github-Event" -> ["push"]
  X-Custom: a                    "X-Custom"       -> ["a" "b"]
  x-custom: b                    "Alpha"          -> ["last-on-the-wire"]
  alpha: last-on-the-wire
```

`net/http` canonicalises casing before the handler runs, merges the two differently-cased
`X-Custom` lines into one key, and hands over a map — which has no order. Go exposes no
API for the original bytes. So storing "an ordered list of `[name, value]` pairs" preserves
multi-value and gives a *deterministic* order, but it cannot reproduce wire order or
original casing. Phase 1 must either accept and document that, or tee the raw header bytes
off the connection before `net/http` parses them. See the follow-ups below.

Worth being clear on the blast radius: this affects **display fidelity only**. Signature
verification in Phase 4 is unaffected, because signatures are computed over the raw *body*
plus header values looked up by name, and lookup is case-insensitive by design.

---

## Q3. A colleague's container takes *exactly* 10 seconds every time they run `docker stop`. What's the most likely cause, and how would you confirm it?

**Answer:**

> Ten seconds is the giveaway — that's Docker's default grace period, not a coincidence.
> `docker stop` sends SIGTERM, waits 10s, then sends SIGKILL. Landing on exactly 10.0s every
> time means the container is never exiting on its own; it's being killed at the deadline.
>
> Most likely cause: SIGTERM isn't reaching the app, or the app ignores it. And the usual
> reason it isn't reaching the app is shell-form `ENTRYPOINT`/`CMD` — `ENTRYPOINT /app/server`
> instead of `ENTRYPOINT ["/app/server"]`. Shell form wraps the command in `/bin/sh -c`, so
> the shell is PID 1, and `sh` doesn't forward signals to its child. SIGTERM goes to the
> shell, the shell does nothing with it, the app never hears about it. Our Dockerfile uses
> exec form for exactly this reason (`Dockerfile:47`). The other flavour of the same bug is a
> container that does get the signal but has no `signal.Notify` handler for graceful shutdown
> — though a Go binary with no handler exits immediately on SIGTERM, so a 10-second hang
> points at the signal not arriving at all.
>
> How I'd confirm it, in order, cheapest first:
>
> - `docker inspect --format '{{json .Config.Entrypoint}} {{json .Config.Cmd}}' <c>` — if I
>   see `["/bin/sh","-c","..."]`, that's the answer right there.
> - `docker exec <c> ps -o pid,args` — is PID 1 the app or a shell?
> - The decisive test: `docker stop -t 30 <c>`. If it now takes exactly 30 seconds, the
>   timeout is what's ending it, so it's a signal problem. If it still takes 10, something
>   else is going on.
> - And afterwards, `docker inspect --format '{{.State.ExitCode}}' <c>` — 137 is 128+9, i.e.
>   SIGKILL, confirming it was killed rather than exiting cleanly.
>
> The quick workaround is `docker run --init`, which puts a real init as PID 1 to forward
> signals — but that's a patch over a Dockerfile that should just use exec form.

**Assessment — correct diagnosis, excellent method, one inference too strong.**

The reasoning from "exactly 10.0s" to "killed at the deadline" is right, the shell-form
cause is the most likely one, and `docker stop -t 30` as a decisive discriminator is a
genuinely good diagnostic — it separates "timeout is ending it" from "something else takes
10s" in one command. Exit code 137 = 128 + 9 is right.

**The correction:** *"a 10-second hang points at the signal not arriving at all."* Not
quite. A process that receives SIGTERM perfectly and then **blocks during its graceful
shutdown** produces the identical symptom. If an in-flight request never completes and the
drain waits for it, Docker still kills at 10s.

That is not hypothetical — **our own app has this shape.** `cmd/hooklens/main.go:117` gives
`srv.Shutdown` a 20-second budget, and Docker's default grace period is 10. Under
`docker stop` defaults our drain can never use more than half its budget before SIGKILL
arrives. Today nothing blocks long enough to notice; from Phase 3, with long-lived tunnel
WebSockets, it will. Logged as a follow-up.

So `docker stop -t 30` is even more useful than stated: if the container then takes the
full 30s, the signal *is* arriving and the shutdown is blocking. Distinguishing those two
needs the container's own logs — "shutdown signal received, draining" appearing means the
signal arrived and the problem is downstream of it.

---

## Q4. Someone edits `00001_create_endpoints.sql` to add a column, commits, and it works on their machine. What happens on yours, and why?

**Answer:**

> On my machine, nothing happens — and that's the whole problem.
>
> goose keeps a `goose_db_version` table recording which migration IDs have run. My database
> already has version 1 marked as applied, so when I pull and run `hooklens migrate up`,
> goose looks at 00001, sees it's already done, and skips it. It doesn't read the file. It
> doesn't hash the file. It has no idea the contents changed. So my schema never gets the new
> column, goose reports everything is up to date, and then the application code that
> references that column fails at runtime with an error about a column that doesn't exist —
> nowhere near the actual cause.
>
> It "worked on their machine" almost certainly because they blew away their database and
> started fresh (`docker compose down -v`), so migration 1 ran once, with the new column in
> it. The migration is fine on an empty database and a no-op on every existing one, which is
> the worst possible combination: CI is green too, because CI spins up a fresh Postgres. So
> the only environments that break are the ones with history — every developer who didn't
> reset, staging, and production.
>
> Worth noting goose is on the permissive end here. Flyway checksums each applied migration
> and refuses to run if a file changed underneath it, which turns this into a loud error
> instead of silent drift. goose trusts you.
>
> The fix is the rule already written into the top of that file: migrations are append-only.
> The column goes in `00002_add_column.sql`. Once a migration has run anywhere you don't
> control, it's frozen.

**Assessment — correct and complete.**

"Fine on an empty database and a no-op on every existing one, which is the worst possible
combination" is the sentence that shows the concept landed. The follow-through to CI being
green *because* it starts from a fresh Postgres is the part that makes this dangerous in
practice — the automated gate is structurally incapable of catching it, so the only
environments that fail are the ones with history, which is exactly the set you care about.

The Flyway contrast is right and worth keeping: Flyway stores a checksum per applied
migration and fails on mismatch; goose records only the version ID and never re-reads the
file. Neither is wrong, they trade a class of error for a class of friction.

---

## Q5. Why does our Dockerfile copy `go.mod` before the source code? What breaks if you swap those two lines?

**Answer:**

> Docker builds in layers, and each layer is cached against the inputs that produced it. A
> `COPY` layer's cache key includes the contents of the files copied; if those are unchanged
> from a previous build, Docker reuses that layer and everything cached after it, until
> something does change.
>
> So `COPY go.* ./` then `RUN go mod download` (`Dockerfile:12-13`) puts dependency fetching
> on a layer that only depends on `go.mod` and `go.sum`. Editing a `.go` file doesn't touch
> those, so on the next build Docker reuses the downloaded module cache and goes straight to
> compiling. Dependencies only get re-downloaded when I actually change dependencies, which
> is rare.
>
> Swap the two lines — `COPY . .` first — and nothing breaks. The image is byte-for-byte as
> correct. What you lose is the cache: `COPY . .` includes `go.mod`, but it also includes
> every source file, so the layer's cache key changes on every single edit. Which invalidates
> that layer, which invalidates `RUN go mod download` after it, which means every build
> re-downloads the full module graph before compiling. On this project that's an annoying few
> seconds; on something with a heavy dependency tree it's minutes per build, multiplied by
> every CI run.
>
> The pattern generalises — it's the same reason you copy `package.json` before `src/`, or
> `requirements.txt` before your Python. Order your Dockerfile from least-frequently-changed
> to most-frequently-changed. And it's why `.dockerignore` matters alongside it: `docs` and
> `*.md` are excluded, so writing a learn note doesn't invalidate the build context at all.

**Assessment — correct.**

The distinction that carries this answer is "nothing breaks... what you lose is the cache."
Layer ordering is a *performance and cost* property, not a correctness one, and people who
half-know it often claim the build would fail. Naming the general rule — order from
least-frequently-changed to most — is the transferable version.

Connecting `.dockerignore` to the same mechanism is the extra step: excluded files never
enter the build context, so they cannot invalidate a layer even indirectly.

---

## Q6. A migration creates a table, then fails on the next statement. What state is the database left in on Postgres, and on MySQL? Why do they differ?

**Answer:**

> On Postgres: nothing happened. The table isn't there, and the migration isn't recorded as
> applied. Postgres has transactional DDL — `CREATE TABLE` inside a transaction rolls back
> like any other statement — and goose wraps each migration file in a transaction by default.
> So I fix the broken statement and re-run, and it applies cleanly from the top. The database
> is never in a half-migrated state.
>
> On MySQL: the table is still there. MySQL issues an implicit commit before and after every
> DDL statement, so `CREATE TABLE` committed the moment it ran, regardless of any transaction
> I thought I was in. The second statement fails, the migration is not recorded as applied,
> and now I'm stuck: I fix the file, re-run, and it dies immediately on
> `CREATE TABLE ... already exists`. I have to go clean up by hand before I can make progress
> — which, at 3am on a production database, is exactly when you don't want to be hand-editing
> schema.
>
> Why they differ comes down to where the schema lives. Postgres stores its catalog —
> `pg_class`, `pg_attribute`, all of it — in ordinary heap tables under the same MVCC
> machinery as user data. A DDL statement is, underneath, just inserts and updates into those
> tables, so it gets transaction semantics for free. MySQL historically kept its data
> dictionary in files on disk (`.frm`) outside InnoDB entirely, so there was no mechanism to
> roll it back with a transaction. MySQL 8.0 moved the dictionary into InnoDB and made each
> individual DDL statement atomic and crash-safe — so a single `CREATE TABLE` won't leave a
> half-built table any more — but it still can't group multiple DDL statements into one
> rollback-able unit. Per-statement atomicity, not per-migration.
>
> Practical upshot, and the reason it's worth knowing: on Postgres you can write a
> multi-statement migration and trust it's all-or-nothing. On MySQL the discipline is one DDL
> statement per migration file, so a failure leaves an unambiguous state. The exception on our
> side is the handful of Postgres operations that can't run in a transaction —
> `CREATE INDEX CONCURRENTLY` is the one I'll actually hit in Phase 1 — and those need
> `-- +goose NO TRANSACTION`, which puts me right back in MySQL's world for that one file.

**Assessment — the best answer of the eight, and deeper than the brief it came from.**

Everything here is accurate: catalog-in-heap-tables under MVCC as the *mechanism* for
Postgres DDL rollback, `.frm` files outside InnoDB as the historical reason MySQL could
not, and the MySQL 8.0 change being per-statement atomicity rather than per-transaction
rollback. That last distinction is the one people get wrong — "MySQL 8 has atomic DDL now"
is repeated constantly without the qualifier that it cannot group statements.

The two practical conclusions are the payoff: one DDL per file as the MySQL discipline, and
the recognition that `-- +goose NO TRANSACTION` for `CREATE INDEX CONCURRENTLY` puts a
Postgres migration back into exactly the MySQL failure mode for that file. That connection
was not taught and is correct.

Nothing to correct.

---

## Q7. `Resolve("hooklens.dev", "a.b.hooklens.dev", "/")` returns `TargetApp`. Which single line in `routing.go` causes that, and what would happen if it were written differently?

**Answer:**

> The line is `internal/server/routing.go:63`:
>
> ```go
> if sub, ok := strings.CutSuffix(host, "."+base); ok && validSlug(sub) {
> ```
>
> Walk it: path `/` doesn't match the `/e/` form, so form 1 is out. The host isn't
> `hooklens.dev` or `www.hooklens.dev`, so form 3 is out. Then `CutSuffix` strips
> `.hooklens.dev` and hands back `sub = "a.b"` — the whole remaining prefix, dots and all.
> `validSlug` walks it byte by byte and rejects `.` because it isn't in the allowed set of
> `[a-z0-9-]`. So the `&&` short-circuits, we fall past it, and line 67 returns `TargetApp`.
> Nested subdomains don't resolve to an inbox.
>
> The interesting part is what happens if you write it the obvious other way. The natural
> instinct is "take the first label":
>
> ```go
> sub := strings.Split(host, ".")[0]   // "a"
> ```
>
> Now `a.b.hooklens.dev` captures into inbox `a` — a real inbox, belonging to a real person,
> addressed by a hostname they never handed out. Since the `Host` header is entirely
> attacker-controlled and arrives before any authentication, that's someone else's data being
> reachable by a name they can't predict or audit. It also breaks in a way TLS would
> half-hide: a `*.hooklens.dev` wildcard certificate covers exactly one label, so
> `a.b.hooklens.dev` isn't covered by our cert at all — the route would exist but only be
> reachable over a connection the browser screams about, or by anything that skips
> verification. A bug that only manifests for clients ignoring TLS errors is a bug you find in
> an incident, not in tests.
>
> The other way to get this wrong is making `validSlug` a blacklist — "reject anything with a
> slash or a null byte" — instead of a whitelist. Then `a.b` passes, and `a.b` goes straight
> into a database lookup. The whitelist is doing the real work on this line; `CutSuffix` just
> sets it up so there's a single string to validate.

**Assessment — correct, with an addition the brief did not make.**

The trace is exact, including that `validSlug` rejects on the dot rather than on length, and
that `&&` short-circuiting is what carries it to the fall-through.

The `strings.Split(host, ".")[0]` counter-example is the right one — it is what most people
write, it looks obviously equivalent, and it silently routes one person's traffic into
another person's inbox under a hostname the owner never published.

**The wildcard-certificate point is new and correct.** `*.hooklens.dev` matches exactly one
label under RFC 6125, so `a.b.hooklens.dev` is not covered. A route that only works for
clients skipping certificate verification is a route that passes every test written with an
HTTP client and fails silently in a browser — precisely the shape of bug that surfaces as an
incident. This is Phase 0 unit 2 material (DNS and TLS) arrived at without the unit having
been taught, which is worth noting.

---

## Q8. Why does `/healthz` deliberately *not* check whether Postgres is reachable?

**Answer:**

> Because it's a liveness check, and liveness answers exactly one question: should this
> process be killed and restarted? Nothing more.
>
> If I make `/healthz` fail when Postgres is unreachable, here's what happens when the
> database has a bad ten minutes. The platform probes the container, gets a 503, concludes the
> container is broken, and kills it. It starts a new one. That one can't reach Postgres
> either, so it fails its probe and gets killed too. Now I'm in a restart loop, and I've
> converted "the database is down and the app is serving degraded" into "the database is down
> and nothing is running." I've also thrown away every in-flight request and, once Phase 3
> lands, every open tunnel WebSocket — connections that had nothing to do with Postgres and
> were working fine. The restart cannot possibly fix the problem, because the problem isn't in
> this process.
>
> There's a worse version if the health endpoint is also the load balancer's check: every
> instance fails simultaneously, the LB pulls all of them out of rotation, and a dependency
> blip becomes a total outage with no capacity left to recover into.
>
> So `handleHealth` (`internal/server/server.go:82`) returns 200 as long as the process is up
> and the HTTP server is answering. That statement is true and useful on its own.
>
> The nuance is that dependency status is worth exposing — just not on this endpoint and not
> with these consequences. Phase 5 adds a separate readiness/dependency endpoint that does
> check Postgres, and the difference is who consumes it and what they do about it: readiness
> controls whether traffic is routed to this instance, and metrics and alerts tell a human to
> go look. Neither one pulls the trigger on SIGKILL. Same information, wired to a response
> that can actually help.

**Assessment — correct, and one step short of the full picture.**

"The restart cannot possibly fix the problem, because the problem isn't in this process" is
the principle stated as tightly as it can be. The correlated-failure escalation — every
instance failing at once, no capacity left to recover into — is the part that turns this
from a style preference into an outage postmortem.

Naming the Phase 3 consequence unprompted is the right instinct: killing a pod discards
every open tunnel WebSocket, and those connections have no dependency on Postgres
whatsoever.

**The step missing:** readiness has its own version of the same trap. If readiness checks
Postgres and Postgres goes down, *every* instance goes unready simultaneously and the load
balancer has nothing left to route to — the same correlated failure, one layer over. The
real question for readiness is not "is the dependency up" but "can this instance still do
anything useful without it." For hooklens that will need deciding rather than assuming:
capture genuinely requires Postgres, but the tunnel and the health surface do not, so a
blanket readiness failure would take down more than it needs to.

The distinction to carry forward is three-way, not two: **liveness** = restart me;
**readiness** = route to me; **metrics/alerts** = wake a human. Only the first two are
automated, and each has a different correct answer for a dependency outage.

---

## Follow-ups this quiz generated

1. **`Shutdown` budget exceeds Docker's grace period.** `cmd/hooklens/main.go:117` allows 20s
   to drain; `docker stop` defaults to 10s before SIGKILL. The drain can never use more than
   half its budget. Fix: reduce ours below 10s, or set `stop_grace_period` in `compose.yaml`
   and `--stop-timeout` wherever it is deployed. Harmless today, real from Phase 3.
   *(From Q3.)*

2. **Header fidelity is not achievable through `net/http` alone.** Verified: casing is
   canonicalised, differently-cased duplicates are merged, and `r.Header` is an unordered
   map. Phase 1 must choose between (a) accepting it and documenting the limitation,
   (b) teeing raw header bytes off the connection before parsing, via a wrapping
   `net.Listener`/`net.Conn`, or (c) hijacking, which is not worth it. Option (b) is roughly
   60 lines and would make the inspector genuinely byte-faithful, which is arguably the whole
   product claim. Does not affect Phase 4 signatures. *(From Q2.)*

3. **Unit 2 — DNS and TLS — is still unexamined**, because it was never taught. The Q7 answer
   reached part of it independently (wildcard certificates cover one label). Worth the brief
   before Phase 1.
