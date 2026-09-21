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
| 1 — The mailbox | [06](learn/06-reading-a-request.md) · [07](learn/07-storing-a-request.md) · [08](learn/08-capability-urls.md) · [09](learn/09-pagination.md) · [10](learn/10-background-workers.md) | [below](#phase-1--the-mailbox) |
| 2 — The inspector UI | [11](learn/11-spa-and-go-embed.md) · [12](learn/12-cors.md) · [13](learn/13-server-sent-events.md) · [14](learn/14-pubsub.md) · [15](learn/15-server-state.md) · [16](learn/16-recursive-rendering.md) | [below](#phase-2--the-inspector-ui) |

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

---

# Phase 1 — the mailbox

*Set 2026-09-19. Material: notes [06](learn/06-reading-a-request.md),
[07](learn/07-storing-a-request.md), [08](learn/08-capability-urls.md),
[09](learn/09-pagination.md), [10](learn/10-background-workers.md).*

> **These are model answers, not recall.** Phase 0's answers above were written from memory
> and then assessed; these were written by Claude on request, to serve as study material.
> The two are not equivalent evidence — reading a good answer feels like knowing it. Cover
> the answers, work through the questions cold, and compare.

---

## Q1. Your capture handler does `body, _ := io.ReadAll(r.Body)`. Describe the attack, what it costs the attacker, and why `Content-Length` doesn't save you.

`io.ReadAll` loops calling `Read` and growing a slice until it sees `io.EOF`. EOF arrives
when the *sender* decides to stop. So the sender controls how much memory you allocate, and
there is no upper bound on it.

The attack is to never stop. Open a connection, start a body, and dribble a few kilobytes
per second forever. Your process grows until the OOM killer takes it — and takes every
in-flight request with it. A few hundred concurrent connections doing this is enough.

What it costs the attacker: one machine, a few kilobytes per second per connection, and
`curl`. No botnet, no amplification, no cleverness. That asymmetry — negligible cost to
attack, fatal cost to defend — is what makes it a denial-of-service *primitive* rather than
merely a bug.

`Content-Length` does not save you, three times over:

1. **It is a claim, not a measurement.** The sender writes it. Nothing verifies it matches
   the bytes that follow.
2. **Chunked transfer encoding omits it entirely.** The body arrives in self-describing
   pieces with no length declared up front, so there is nothing to check.
3. **Trusting it makes things worse.** The instinct after learning about it is to
   pre-allocate — and then an attacker declares four gigabytes and you allocate four
   gigabytes before a single byte of body arrives.

The fix is not to validate the sender's claim but to stop trusting the stream: wrap it, so
the ceiling is a number *you* chose. `io.LimitReader(r.Body, maxBody+1)`.

A related variant is worth knowing because we defend it elsewhere: dribbling *headers*
rather than a body is the Slowloris attack, and `ReadHeaderTimeout` in
`cmd/hooklens/main.go` is what closes it.

---

## Q2. We read the body with `io.LimitReader(r.Body, maxBody+1)`. What is the `+1` for, and what specifically breaks if you remove it?

`LimitReader` reports `io.EOF` once it has handed over its limit. That EOF is
indistinguishable from the sender finishing — same value, no flag, nothing to inspect. So
if you ask for exactly `maxBody` and receive exactly `maxBody` bytes, you cannot tell
whether the body was exactly that size or whether it was larger and you silently cut it.

Asking for one byte more makes the two cases distinguishable. Receiving `maxBody+1` bytes
is proof there was more to come; receiving fewer is proof the sender finished.

Remove it and `Truncated` can never be `true`. Nothing errors, nothing logs, and no test
fails unless one asserts exactly at the boundary — which is why `TestFromHTTPBody` has
cases at `limit-1`, exactly `limit`, and `limit+1`.

The consequence is the one an inspector can least afford: the UI would render a cut-off
body as though it were the whole thing. A user debugging a webhook would be looking at
truncated JSON with no indication it was truncated, and would conclude the provider sent
malformed data. The tool would be lying, confidently, about the one thing it exists to
report accurately.

---

## Q3. A colleague stores captured webhook bodies in a `jsonb` column — it's queryable, and the payloads are JSON anyway. Name two distinct things that breaks, one of which is silent.

**The silent one: `jsonb` does not store what you gave it.** It parses on write into a
binary tree with keys held in sorted order. So object keys come back alphabetised,
whitespace is gone, and duplicate keys silently lose all but the last.
`{"b":1,   "a":2}` becomes `{"a": 2, "b": 1}`.

Semantically equivalent JSON, and catastrophic here. Phase 4 verifies provider signatures
by computing an HMAC over the exact bytes received. Reorder one key and every signature
check fails — while the payload on screen looks perfectly correct, which is what makes the
bug so expensive to find. You would be debugging your HMAC implementation for a day.

**The loud one: it cannot hold the data at all.** A body containing a NUL byte is rejected
outright by Postgres in `jsonb`, `json` *and* `text` — the insert errors and the capture is
lost. Protobuf, gzip, images, and anything binary all hit this. And a body that is not
valid JSON is not storable in a JSON column by definition, which rules out form-encoded
bodies, XML, plain text, and — pointedly — *malformed JSON*, which is very often exactly
the thing the user opened an inspector to look at.

Hence `bytea`, with the UI parsing on read. The fallback is not lossy and the storage is
not opinionated about what a webhook is allowed to contain.

---

## Q4. Headers go into `jsonb` as an *array* of `{name, value}` rather than an object keyed by name. Why is that not just a style preference?

Because `jsonb`'s lossiness applies only to **object keys**. Array element order and
duplicate elements are preserved exactly. The array is not avoiding `jsonb` — it is using
the half of `jsonb` that does not destroy data.

An object keyed by header name would lose two things, both real:

**Duplicates.** HTTP explicitly permits the same field name on multiple lines, and
`jsonb` collapses duplicate keys with last-wins. `Set-Cookie` is the case that cannot even
be worked around by folding into one comma-separated value, because it contains commas
inside its own `Expires` date.

**Order.** An object sorts its keys, so whatever ordering we chose is gone.

There is a third consideration specific to us: a header name is not a well-behaved key. It
is attacker-controlled, arbitrary bytes in practice, and a JSON object with duplicate or
strange keys is a worse container than an array of pairs with no such requirements.

Pinned by `TestInsertRequestRoundTrip`, which stores two `X-Custom` headers and asserts both
come back, in the order they were sent.

---

## Q5. Why is the index `(endpoint_id, received_at desc)` and not `(received_at desc, endpoint_id)`? What does Postgres do differently in each case?

A composite index is one B-tree over the *pair*, sorted by the first column and then by the
second within each value of the first. The column order determines what it can seek to.

Our query is always `where endpoint_id = $1 order by received_at desc limit N`.

**With `(endpoint_id, received_at desc)`:** Postgres descends the tree to where that
`endpoint_id` begins — an O(log n) seek — and then walks forward. Because `received_at` is
the second column, the rows it walks are *already in the order requested*. It reads exactly
N entries, produces no sort, and discards nothing. Confirmed by `EXPLAIN`: an Index Only
Scan with no `Sort` node.

**Reversed, `(received_at desc, endpoint_id)`:** the tree is ordered by time across all
endpoints, so one inbox's rows are scattered along its entire length. Postgres can still
walk in time order for free, but it must now read and discard every row belonging to every
other inbox until it accumulates N matches. With a hundred inboxes that is roughly a
hundred times the reads, and it degrades as you add tenants.

The generalisable rule: **equality columns first, then the range or ordering column.** An
equality predicate on the leading column is what turns the index into a seek; leading with
the ordering column gives you free ordering and no filtering.

---

## Q6. We use UUIDv7 for row ids and a separate 128-bit base32 value for the inbox slug. Why can't the UUIDv7 be the slug?

Because unique and unguessable are different properties, and a slug needs the second one.

A UUIDv7 is 48 bits of Unix-millisecond timestamp, 4 version bits, 12 random bits, 2
variant bits and 62 more random bits — **74 random bits, and 48 knowable ones**. An attacker
who knows roughly when an inbox was created has already pinned the largest field in it. The
value is guaranteed unique, and that guarantee says nothing whatever about how hard it is
to guess.

The slug is the only thing standing between a subdomain scanner and an inbox, so it gets a
full 128 bits straight from `crypto/rand`, with no structure to erode them.

The neat part is that the property making v7 unsuitable as a secret is precisely the
property making it excellent as a primary key. The leading timestamp means new rows sort at
the end of the B-tree, so inserts concentrate on the rightmost page, which stays in cache —
no page splits scattered across the index, no write amplification. Time-ordering is a
liability in a secret and an asset in a key.

(UUIDv4 would be acceptable as a secret at 122 random bits. We do not use it because base32
of 16 raw bytes is denser, DNS-safe, and fits the existing slug whitelist.)

---

## Q7. We hash tokens with a single SHA-256, and I argued bcrypt would be *worse*. Make that argument.

Slow hashes exist to solve exactly one problem: the secret comes from a small space, so an
attacker holding the hash can enumerate that space offline. A human-chosen password carries
perhaps 20–30 bits of entropy, which is a few million candidates — trivial at millions of
hashes per second. bcrypt's work factor makes each attempt cost ~100ms, turning hours into
centuries. The slowness *is* the defence.

Our token is 256 bits from a CSPRNG. There is no space to enumerate. At a trillion guesses
per second you would still need vastly longer than the age of the universe. The attack the
work factor defends against cannot happen, so the work factor defends nothing.

And it is not free. It costs ~100ms of CPU **on every authenticated request** — the user
pays it, on the happy path, forever. Worse, it hands over a denial-of-service vector: an
attacker sends a stream of bogus tokens and you burn a CPU core hashing each one before
rejecting it. You have made yourself slower to verify than the attacker is to guess.

There is a secondary reason, and it is the sharper one: **bcrypt silently truncates its
input at 72 bytes.** A 43-character token survives, but the general shape — reaching for
the "more secure" primitive and having it quietly discard entropy — is how a security
upgrade becomes a downgrade.

The rule worth carrying: *slow hash for low-entropy human secrets, fast hash for
high-entropy generated ones.* Both answers are right, in their own case.

---

## Q8. `GET /api/endpoints/{slug}/requests` returns the same 401 body for a wrong token and for an inbox that doesn't exist. What does distinguishing them give an attacker?

An oracle — a question the system answers truthfully for free.

With identical responses, reaching someone's data requires guessing a 128-bit slug *and* a
256-bit token, and the first is already infeasible.

With distinguishable responses — 404 for a missing inbox, 401 for a bad token — the
existence question is answerable without holding any credential at all. The attacker
enumerates slugs against the cheap question, discards the overwhelming majority, and is
left with a list of real inboxes. Two hard problems have become one hard problem plus a
lookup.

Two things make that worse than it first sounds. Existence is itself information: knowing
that a particular inbox exists can matter independently of reading it. And an enumerable
existence check is a far better target for automation than a token check, because it is
cheap, stateless and unauthenticated — exactly the shape a scanner wants.

Our implementation makes the two indistinguishable at the source rather than at the edge:
`AuthenticateEndpoint` returns `ErrUnauthorized` for `pgx.ErrNoRows` as well as for a hash
mismatch, and the HTTP layer has a single `unauthorized()` function with three call sites.
The moment one of them says something slightly different, the oracle exists again — which
is why it is one function and not three inline responses.

---

## Q9. A user reads page 1 of their inbox, gets distracted for a minute while three webhooks arrive, then clicks "next page". Trace what they see with `OFFSET 50` and with a cursor.

The list is newest-first, so every arriving row is inserted at **position zero** and pushes
everything below it down. That is the mechanism; the rest follows.

Call the existing rows `old-99` (newest) down to `old-00`. Page 1 is `OFFSET 0 LIMIT 50`:
`old-99` … `old-50`.

Three webhooks arrive. They are the newest, so they take positions 0, 1 and 2, and every
old row shifts down by three. What was at index 50 is now at index 53.

**With `OFFSET 50`:** the query returns indices 50–99 of the *new* ordering, which is
`old-52` down to `old-03`. The user already saw `old-52`, `old-51` and `old-50` on page 1.
They see three duplicates and no gap. It looks as though the provider double-delivered —
which, for a tool whose entire purpose is answering "why did this fire twice", is a
uniquely bad failure.

Run the same scenario with three *deletions* instead of insertions and the shift goes the
other way: three rows are skipped, silently, and nothing on screen indicates anything is
missing. That is the worse direction, because there is no symptom at all.

**With a cursor:** the cursor holds the `(received_at, id)` of `old-50` — the last row the
user actually saw. Page 2 asks for `where (received_at, id) < (that)`, which returns
`old-49` onward no matter what has been inserted above it. The cursor names a position *in
the data*; new rows cannot move it, because it never referred to a count.

Measured in `TestCursorSurvivesInserts`, which does exactly this and logs both results
side by side.

---

## Q10. `TestIndexIsUsed` originally ran against an almost-empty table and asserted the plan contained no `Sort`. Why was that test meaningless, even while it was passing?

Because the Postgres planner is **cost-based**, not rule-based. It does not use an index
because one exists; it estimates the cost of each plan and picks the cheapest. On a table of
a few rows the whole thing fits in one or two pages, so reading all of it sequentially and
sorting the handful of results genuinely *is* cheaper than descending a B-tree. Choosing the
sequential scan was the optimiser being correct.

So the test asserted an outcome the planner was never obliged to produce. While it passed,
it passed for reasons unconnected to whether our index is well designed — the row count and
the statistics at that moment happened to tip the estimate. Change either and it flips,
which is exactly what happened.

The general form of the mistake: **a test whose outcome depends on conditions the test does
not control is testing those conditions, not the code.** And the specific form is worth
remembering as a rule — *an index assertion against an empty table tests nothing at all*,
because the one plan you are trying to rule out is the correct plan for that data.

The fix was to make the test control what it depends on: seed 5,000 rows so the index is
genuinely the cheaper plan, and run `ANALYZE` so the planner works from real statistics
rather than its default guesses. Now the assertion means something specific — *given a
realistic table, this index serves this query without a sort.*

---

## Q11. The retention sweeper runs in a goroutine with `defer recover()` inside `sweepOnce`. Why is that necessary here when our HTTP handlers get away without it?

Because `net/http` is doing it for the handlers, and nothing is doing it for a goroutine.

The server runs each connection in its own goroutine with a `recover` wrapped around the
handler call. A panic in a handler is caught there: that one request dies, a stack trace is
logged, the connection closes, and every other request continues. This is why experienced
Go programmers develop the intuition that panics are survivable — in the context most of
them work in, they are.

A bare `go f()` has no supervisor. There is no frame above it, nothing wrapping it, nobody
watching. A panic unwinds `f`'s stack, finds no `recover`, and the runtime terminates **the
entire process**. Every in-flight capture, every open connection, gone.

Which produces a genuinely absurd failure mode: a nil dereference in *housekeeping code
nobody is waiting on* takes down the webhook capture endpoint for every user. The sweeper is
the least important thing in the process and, without a recover, the most dangerous.

The `recover` converts that from an outage into a skipped sweep plus a logged stack trace,
and the next tick tries again.

`TestSweepOncePanicIsContained` forces the case with a nil `*Store`. It is worth noting how
that test fails if the recover is removed: it does not report a failure, it **crashes the
test binary** — which is the same thing that would happen in production, demonstrated.

---

## Q12. In `run()`, why is the sweeper given a *derived* cancellable context rather than the signal context directly? What fails if you use the signal context?

`serve()` returns for two quite different reasons, and only one of them involves the signal
context.

**Normal shutdown:** SIGTERM arrives, the signal context is cancelled, `srv.Shutdown` drains,
`serve` returns. The sweeper's context is dead too, so it exits and `wg.Wait()` returns
promptly. Everything works.

**Startup failure:** `ListenAndServe` fails immediately — the port is in use — and `serve`
returns that error within milliseconds. **Nobody sent a signal.** The signal context is
perfectly healthy, so the sweeper is still ticking away happily, and `wg.Wait()` blocks
forever.

The observable result is the worst kind of bug: a trivially diagnosable one-line error
("address already in use") becomes a process that prints nothing and hangs, and you have to
interrupt it to discover it was never going to tell you. A clear failure has been converted
into a mystery.

The derived context fixes it by making both paths identical. Whatever caused `serve` to
return, `cancel()` runs, the sweeper's `select` takes its `ctx.Done()` branch, `wg.Wait()`
returns, and the real error propagates.

The ordering of the last three statements is also load-bearing: `cancel()`, then
`wg.Wait()`, then `return`. The return is what triggers the deferred `st.Close()`, and
closing the pool out from under an in-flight `DELETE` produces "conn closed" errors on every
otherwise-clean shutdown — noise that looks like a bug and is not.

---

# Phase 2 — the inspector UI

*Set 2026-09-21. Material: notes [11](learn/11-spa-and-go-embed.md),
[12](learn/12-cors.md), [13](learn/13-server-sent-events.md), [14](learn/14-pubsub.md),
[15](learn/15-server-state.md), [16](learn/16-recursive-rendering.md).*

> **These are model answers, not recall.** As with Phase 1, these were written by Claude on
> request to serve as study material. Reading a good answer feels like knowing it. Cover
> the answers, work through the questions cold, and compare.

---

## Q1. The server serves `index.html` for any unmatched path so client-side routes work on refresh. Why does `/api/` get an explicit catch-all registered *above* that, and what is the failure if you omit it?

Because the SPA fallback is a catch-all, and a catch-all cannot tell "a route the client
knows about" from "an API path I typo'd". Without `mux.HandleFunc("/api/", ...)`, a request
to `/api/endponts/abc` falls through to the fallback and gets **200 OK with an HTML
document**.

The failure that produces is disproportionate to the typo. The client does
`resp.ok → resp.json()`, `resp.ok` is true, and `json()` throws a parse error on `<!DOCTYPE`.
So a wrong URL surfaces as "Unexpected token '<'" — an error that names JSON parsing and
points nowhere near the actual mistake. Whole afternoons have gone to that message.

The fix is one route that returns `404 {"error": ...}` for anything under `/api/` that
nothing else matched, registered so that Go's pattern precedence prefers the more specific
`/api/` prefix over bare `/`. The general principle: **an API namespace should fail as an
API**, never fall back into a different content type.

## Q2. `go:embed` puts the built frontend in the binary. Name a real cost of that, and describe the cache-header policy the embedded assets need and why it is different for `index.html` than for the JS bundle.

The cost is that **the frontend is now part of the Go build graph**. The binary is bigger
(a few hundred KB here, which is nothing), but the real price is ordering: `go build` will
happily embed a *stale* `dist/`, because it cannot know the TypeScript changed. You must
build the frontend first, every time, and CI must enforce that rather than trusting
discipline. The `.gitkeep` incident is the same class of problem — the embed directory has
to exist at build time even when empty, or the `//go:embed` directive fails to compile.

The cache policy runs in opposite directions on purpose, because Vite fingerprints asset
filenames:

- **`/assets/index-B51nQ6hb.js`** — the hash is in the name, so the content at that URL can
  never change. `Cache-Control: public, max-age=31536000, immutable`. A new build produces
  a new name, so there is no invalidation problem to solve.
- **`index.html`** — the one file with a stable URL, and the file that *names* the hashed
  assets. It must be revalidated: `no-cache`. Cache it and the browser keeps asking for the
  old bundle names forever, which after a deploy that removed them is a blank page.

The pairing is the whole trick: one uncacheable document pointing at infinitely cacheable
resources.

## Q3. Our UI makes cross-origin requests to the API in development but not in production. Explain what CORS actually is, who enforces it, and why serving the SPA from the Go binary removes the category of problem rather than solving it.

CORS is **not a server-side access control**. It is a browser policy: the browser refuses
to let *JavaScript on page A* read a response from origin B unless B says it may, via
`Access-Control-Allow-Origin`. The request often still reaches the server and still has its
side effect — the browser just withholds the response from the script. That is why CORS is
not a security mechanism protecting the server; the thing it protects is the *user's*
ambient credentials on other sites.

Two consequences people get wrong:

- A `curl` to the same endpoint works fine, because curl is not a browser and enforces
  nothing. "It works in Postman" tells you nothing about a CORS failure.
- Some requests trigger a **preflight** `OPTIONS` first — anything beyond a simple method
  with simple headers. An `Authorization` header alone is enough. So adding auth to a
  request that previously worked can produce a failure that is really a *second*, unhandled
  request.

In production the SPA is served by the same Go binary on the same origin, so there is no
cross-origin request and the policy never engages — nothing to configure, nothing to get
subtly wrong, no `Access-Control-Allow-Origin: *` accidentally shipped. In development Vite
proxies `/api` to the Go server, so the browser still sees one origin. The category is
removed by architecture rather than by configuration, which is the stronger form of fixing
it.

## Q4. Describe the SSE wire format precisely enough to write a parser. Then name two things a hand-rolled parser gets wrong if it processes the stream chunk by chunk.

The format is text, one event per blank-line-terminated block, each line
`field: value` (one optional space after the colon is format, not data):

```
event: capture
data: {"id":"..."}
id: 42

```

`data` may repeat, and repeated `data` lines are **joined with `\n`**, not concatenated.
A missing `event` field means the event name is `message`. A line beginning with `:` is a
comment — that is how heartbeats are sent, and it must produce no event. The blank line is
the terminator: without it, nothing dispatches.

The two chunk-boundary bugs:

1. **A chunk boundary can fall mid-line.** `fetch`'s `ReadableStream` hands you arbitrary
   byte runs, not lines. If you split each chunk on `\n` and process it independently, a
   payload split as `data: {"id":"ab` / `c"}\n\n` yields two corrupt fragments. The parser
   must keep a buffer across chunks and only consume up to the last complete line.
2. **A multi-byte UTF-8 character can be split across chunks.** Decoding each chunk with a
   fresh `TextDecoder` produces a replacement character in the middle of a payload. The
   decoder must be reused with `{stream: true}` so it can hold the partial sequence.

Both are invisible in testing, because small payloads arrive in one chunk. They appear in
production when a payload gets large — that is, on exactly the interesting requests.

## Q5. Why must an SSE handler flush, and why did adding a `statusRecorder` middleware silently break flushing app-wide? What is the general lesson about wrapping `http.ResponseWriter`?

Go buffers response writes. For a normal response that is right — one syscall instead of
many. For a stream it is fatal: the bytes sit in the buffer and the client sees nothing,
so a stream that is working perfectly looks completely dead. After each event you must
flush, which pushes the buffer to the socket.

The middleware broke it because `http.ResponseController` (and the older
`http.Flusher` assertion) finds the flusher by **type-asserting the writer it was handed**.
Our `statusRecorder` wrapped the real writer to capture the status code for logging, and it
only implemented `http.ResponseWriter`. The assertion therefore failed, and
`ResponseController.Flush()` returned "feature not supported" — for every handler in the
app, not just the stream, because the middleware was global.

The fix is a one-method escape hatch:

```go
func (rec *statusRecorder) Unwrap() http.ResponseWriter { return rec.ResponseWriter }
```

`ResponseController` follows `Unwrap()` down the chain until it finds something that can
flush.

The general lesson: **wrapping a `ResponseWriter` silently removes every optional interface
the original implemented** — flushing, hijacking, `io.ReaderFrom`. The compiler cannot help,
because the wrapper still satisfies `http.ResponseWriter`; the capability disappears at
runtime, in a distant handler, with an error message that does not mention middleware.
Any wrapper must implement `Unwrap`.

## Q6. A browser tab is subscribed to an inbox and stops reading — backgrounded, throttled, whatever. Enumerate the four things the publisher can do, and defend the one we chose. What specifically makes our choice safe to perform while holding the broker's mutex?

The four:

1. **Block** until the subscriber reads.
2. **Drop** the message.
3. **Grow** the buffer without bound.
4. **Disconnect** the slow subscriber.

Blocking is disqualified by the shape of this system: publishing happens on the *ingest*
path, with a provider waiting on the HTTP response. A backgrounded tab would stall webhook
delivery — the one thing this service must never do. Unbounded growth converts a slow
reader into an OOM, and it is the same failure with a delay. Disconnecting is genuinely
defensible and arguably better UX, because the client reconnects and refetches and so
*resynchronises* rather than carrying a permanent gap — it was rejected only because the
client had no resync path yet.

So: a buffered channel of 32, a non-blocking send, drop on full, **and a counter that is
reported to the UI**. That last clause is what makes dropping acceptable. A viewer shown a
silently incomplete list is worse off than one shown a gap, because they cannot tell
"nothing arrived" from "I missed it" and will draw a wrong conclusion about their own
integration.

What makes it safe under the lock is precisely that `select` with a `default` **cannot
block**:

```go
select {
case s.ch <- m:
default:
    s.dropped++
}
```

A blocking send under the mutex deadlocks the instant one buffer fills — the publisher
waits for a reader, and the reader needs the mutex the publisher is holding. Delete that
`default` and you have written that deadlock, which is why there is a comment above it. The
usual workaround (copy the subscriber set, release the lock, then send) is unnecessary
here, and saves an allocation per publish.

## Q7. `close(ch)` lives in `Unsubscribe`, under the same mutex as `Publish`. Why is that placement the load-bearing invariant of the package, and what goes wrong if a subscriber closes its own channel?

Sending on a closed channel **panics**, and it is not recoverable in a way that helps — it
takes down whatever goroutine is publishing, which here is an ingest request.

Putting `close` inside `Unsubscribe`, guarded by the same mutex `Publish` takes, means a
send and a close can never overlap: both need the lock, so they are serialised. Once
`Unsubscribe` has removed the subscriber from the map and closed the channel, no subsequent
`Publish` can see it to send to it.

If the subscriber closed its own channel from its reading goroutine — the intuitive place,
since it "owns" the reading — there would be no lock held, and a concurrent `Publish`
holding a reference to that channel would panic on the next capture. It would also be
racy in the worst way: harmless under light traffic, and a crash under load.

Two details follow from this. `Unsubscribe` checks membership before deleting, so calling
it twice is a no-op rather than a double `close` (also a panic). And `Subscriber.C()`
returns a **receive-only** channel, so a caller cannot close it even by mistake — the
compiler enforces what the design requires.

## Q8. The stream delivers a new capture. Why do we call `setQueryData` instead of `invalidateQueries`, and why is that not the same reckless thing as an optimistic update?

`invalidateQueries` marks the cached list stale and refetches it. That means: a webhook
arrives, the server tells us about it over the stream, and we respond by **asking the
server what just happened** — a full round trip per webhook, and a visible delay before the
row appears. Under a burst it is a round trip per webhook, amplifying the traffic we were
trying to observe.

`setQueryData` writes the new row into the cache directly, so the row appears in the frame
the event arrives in.

The reason this is not the usual danger of hand-writing a cache: an **optimistic update is
a prediction** — "I think the server will accept this mutation" — and predictions can be
wrong, which is why they need rollback. This is the opposite direction. The event is the
server reporting something it has already durably done; the row exists in Postgres before
the event is published. There is nothing to be wrong about and nothing to roll back.

The one thing it does need is a duplicate guard on `id`, because a stream reconnect can
replay and the initial fetch can land after an event for a row it already contains. Without
it the same capture renders twice — which, in a tool whose purpose is answering "why did
this fire twice", is the most damaging bug available.

## Q9. `<Detail key={selected}>`: what does the `key` do here, why is `useEffect(() => reset(), [id])` the wrong fix, and why does this matter more in this app than in most?

React matches a component to its previous instance by position and `key`. With the `key`
changing on selection, React **unmounts the old `Detail` and mounts a new one**, so all of
its state — the active tab, and the collapsed flag held independently by every node in the
tree — is discarded by construction.

Without the key, React reuses the instance and only changes the props. State survives, so
the tab you were on and every branch you expanded carry over onto an unrelated capture.

The effect-based reset is worse for three reasons. It resets *after* a render, so there is
a frame showing the stale state. It only resets the state you remembered to list, and here
the state is spread across hundreds of `Branch` components with no central registry — there
is literally nowhere to reset from. And it is a rule a future edit can silently violate by
adding a fourth piece of state.

Why it matters more here: in most apps, leftover UI state is a cosmetic glitch. In an
inspector, expanded branches drawn over a different request's data is **the tool lying
about what arrived** — and the entire value proposition is that it does not do that.

Verified rather than assumed: expanding everything and switching to the raw tab, selecting
another capture, then returning gives `body:3 expanded` — the fresh default — not
`raw:` with the expansions intact.

## Q10. Your JSON tree renders a component that calls itself. Give the specific reason an unbounded depth cap is a security concern and not merely a performance one, and explain why `try/catch` is not an acceptable alternative.

React's recursion happens **during rendering**, synchronously — rendering a node calls the
function that renders its children. So the **depth of the data is the depth of the call
stack**, and the data is a webhook body supplied by whoever has the capability URL.

A payload nested tens of thousands deep therefore overflows the JavaScript stack. That is
not a slow render; it is `RangeError: Maximum call stack size exceeded` during a commit,
which React cannot recover from cleanly — the tab dies. And it is cheap to send: a few
hundred kilobytes of `{"a":` produces a nesting depth no legitimate payload approaches. A
remote party turning a small POST into a guaranteed client crash is an availability bug,
which is why this is a security property and not a preference.

`try/catch` does not work. Stack overflow is not reliably catchable — the handler itself
needs stack to run — and by the time it throws, React is part-way through a commit, so
the tree is in an inconsistent state even if you caught it. The guard has to run *before*
the recursion goes too deep, not after it fails.

Hence a `MAX_DEPTH` of 64: far past any real payload (Stripe's deepest object is about 6),
far short of the limit. Past it, the tree renders a message instead of the subtree. This
hides nothing, because the raw tab always shows the complete bytes — verified with 300
levels of nesting, which capped at 65 rendered levels with the page responsive, while the
raw view still showed all 1808 bytes.

The same reasoning, one level down, applies to breadth: collapsed children are **not
rendered at all** rather than hidden with CSS, because `display: none` costs exactly as
much rendering as showing them and so fixes the scrolling while leaving the cost intact.
