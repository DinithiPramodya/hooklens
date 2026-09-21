# 33 — Load testing, and what "dropped" actually means

> Phase 5 · unit 33. The plan's target: 1,000 req/s for 60 seconds, zero dropped
> captures. Getting an honest answer to that turns out to be harder than hitting it.

## Brief

**What is this thing?** A load test drives a system at a chosen rate for a chosen
duration and measures what happens to it. It is not a benchmark. A benchmark asks
"how fast is this function"; a load test asks "at this offered load, does the system
keep its promises, and if not, which promise breaks first."

**What problem does it exist to solve?** Every system is fine until it is not, and
the point at which it stops being fine is almost never where you would guess. Before
load testing, the alternatives were to reason about it — which is how you end up
confident about the wrong bottleneck — or to find out in production, which is how you
end up finding out at 3am from a customer. The specific thing a load test catches
that nothing else does is *behaviour that only appears under concurrency*: a
connection pool that is one connection too small, a lock that is uncontended at 10
req/s and a queue at 1,000, a per-request goroutine that leaks 0.1% of the time.

**How does it actually work underneath?** A load generator has to solve one problem
honestly, and most of them get it wrong. If you write the obvious loop — send a
request, wait for the response, send the next — you are not testing at 1,000 req/s.
You are testing at *whatever rate the server allows*, because every time the server
slows down, you slow down with it. This is **coordinated omission**: the requests that
would have been slow are the ones you never sent, so they never appear in your
latency numbers, and the slower the server gets the better your measurements look.
The fix is an *open-loop* generator: requests are scheduled against a clock, not
against the previous response, and a request that goes out late is recorded as late
from its *intended* send time. A ticker fires 1,000 times a second and each tick
hands work to a pool that may already be busy; if the pool is saturated, that is
itself the finding.

The second mechanism is **measurement**: percentiles, not averages, for the reasons
in [32](32-metrics.md), and the load generator records its own numbers rather than
trusting the server's, because a request the server never accepted cannot appear in
the server's own metrics.

**What are the sharp edges?**

- **Coordinated omission**, above. It is the default behaviour of a naive loop and of
  several popular tools.
- **The generator is usually the bottleneck.** 1,000 req/s means 1,000 sockets a
  second unless connections are reused; the default `http.Transport` caps idle
  connections per host at 2, so a load test at any real rate spends its time in TCP
  handshakes and measures its own client. The first load test I wrote for the tunnel
  in [30](30-diagnosing-flaky-tests.md) had exactly this problem in a different form:
  150 `curl` processes produced a peak concurrency of 13 against a limit of 32, so the
  number looked like a pass and proved nothing.
- **Ephemeral port exhaustion.** A machine has ~28,000 usable outbound ports and a
  closed socket sits in `TIME_WAIT` for two minutes; at 1,000 new connections a second
  you run out in under thirty.
- **"Dropped" is not one thing**, which is the whole reason this unit needs a brief.

**In hooklens:** the assertion is "zero dropped captures", and that phrase has at
least five distinct meanings — a connection refused before HTTP starts, a 429 from the
rate limiter, a 5xx from the handler, a 2xx whose row never reached Postgres, and a
2xx whose row is there but whose SSE broadcast was dropped for a slow browser. Only
the fourth is a bug. The first is capacity, the second is the rate limiter *working*,
the third is a bug of a different kind, and the fifth is deliberate backpressure from
[13](13-server-sent-events.md). A load test that collapses those into one number cannot tell me
which one I have, so the generator has to count them separately and the assertion has
to name the one it means. That definition is the deliverable here at least as much as
the throughput figure.

---

## Decisions

**Write the generator rather than use `vegeta`, `k6` or `hey`.**
`vegeta` is the right tool and gets open-loop right; `k6` is the right tool if the
scenario is a script rather than one endpoint. Both were rejected for one reason: the
assertion this phase needs is not "p99 was under X", it is "the number of 2xx the
server returned equals the number of rows Postgres holds". No external generator can
make that assertion, because it does not have a database handle. Shelling out to
`vegeta`, parsing its report, and then separately querying the row count would be two
tools that have to agree about when the run started and stopped — and the seam between
them is exactly where an off-by-a-few-hundred goes unnoticed.
`internal/loadgen` is ~150 lines of scheduling logic, in-process, so the test is one
Go function that sees all three sources of truth at once.
**This is the wrong call if** the scenario grows — multiple endpoints, stateful flows,
think time, a ramp. Reimplementing k6's scripting model would be foolish.
**The signal to switch is** the first time the load test needs a second request type
that depends on the response to the first.

**Open loop, and saturation reported rather than absorbed.**
The alternative is the closed loop everybody writes first: N workers each looping
send-wait-send. It is simpler and it lies. At 1,000 req/s offered against a server
that can do 600, a closed loop quietly offers 600, reports a 100% success rate, and
reports the latency of a server operating comfortably within its capacity. The
failure is invisible precisely when it matters. So the scheduler here keeps a clock
fixed before the run (`internal/loadgen/loadgen.go:228`), and when every worker is
busy it records `Saturated` and moves on instead of blocking
(`internal/loadgen/loadgen.go:249`).
**Wrong call if** the thing being modelled really is a closed loop — a fixed pool of
upstream clients that genuinely wait, like a connection-pooled service mesh. Webhook
providers are not that: Stripe does not wait for your last webhook before sending the
next.
**Signal to switch:** nothing here. This is the right model for this system.

**Latency measured from the scheduled send time, not from `Client.Do`.**
Timing only the HTTP call is the second half of coordinated omission. If a worker
picks a job up 200ms late because the pool was busy, the sender experienced 200ms +
the request; recording only the request reports a fast server to a caller who
experienced a slow one. `loadgen.go:205` measures `time.Since(j.due)`, and `due` was
computed before the run started.
**Wrong call if** you are profiling the server in isolation and want to exclude the
generator. That is a different question and deserves a different measurement, not a
different definition of latency.

**`Saturated` is a generator finding, and is not counted as a drop.**
It would be easy — and flattering to the generator — to fold undispatched requests
into a single "failed" count. It would also be false: those requests never reached
the network, so the server cannot be blamed for them, and the accurate statement is
"this machine could not offer the load asked for". The test tolerates up to 5% and
fails above that with a message saying the run does not demonstrate the target rather
than that the server failed. Distinguishing "the system under test broke" from "the
measurement broke" is most of what makes a load test trustworthy.

**The durability assertion queries Postgres, not `/metrics`.**
`hooklens_captures_total{outcome="stored"}` is incremented by the handler at
`internal/ingest/ingest.go:183`, *after* the insert — so it is a good metric and
still the wrong oracle here. It reports what the handler believed. The failure mode
being tested is precisely the one where the handler's belief and the database
disagree, so asking the handler is circular. `store.CountRequests` was added for
this (`internal/store/requests.go:380`). The report prints all three counts — what
the generator got, what the metric says, what the table holds — because agreement
across three independent accounts is worth much more than any one of them.

**Behind a `loadtest` build tag, not in the default suite and not in CI.**
A 60-second test taxes every `go test ./...`, and a test that slow gets skipped, then
ignored, then deleted. Running it in CI is worse: a shared runner under an unknown
neighbour's load produces a throughput number that means nothing, and a load test
that goes red for reasons unrelated to the code teaches the team to ignore a red
build. So: a tag, a documented command, and a run before release rather than a run
per commit.
**Wrong call if** a performance regression would ship unnoticed between releases.
**The signal to change it** is the first regression that a release-time run catches
which a per-commit run would have caught earlier and cheaper — at which point the
answer is probably a much shorter smoke run (5s, a few hundred req/s) in CI as a
canary, with the full run staying manual.

**Rate limits raised for the run, and the test still asserts no 429 happened.**
Leaving `RateCapture` at its 50/s default would mean 95% of the run is 429s, which
measures unit 31's limiter rather than the capture pipeline. Raising it invites a
subtler bug: if the limiter fired anyway, the 429s would silently shrink the
denominator and a partially-refused run would still "pass". So the limit goes up
*and* `RateLimited() != 0` is a failure — an unexpected 429 means something happened
that this test does not model, and the right response is to stop, not to average over it.

**Coalesce `last_seen_at` in process rather than write it per capture.**
This decision only exists because the load test found the bottleneck, and it is
worth stating what the alternatives were. (a) Leave it: 280 req/s is arguably enough
for a portfolio webhook inspector, and the column is genuinely useful. (b) Make the
UPDATE conditional in SQL — `... where id = $1 and last_seen_at < $2 - interval '1
minute'` — which skips the write but still costs a round trip and still takes a row
lock to evaluate the predicate. (c) Coalesce in memory: a `map[endpointID]time.Time`
consulted before the write, one UPDATE per inbox per minute
(`internal/ingest/ingest.go:570`). (d) Drop the column.
Chose (c). It removes the round trip entirely in the common case, needs no schema
change, and the staleness it introduces — up to a minute on a "last active" display —
is invisible. (b) was the close second and is strictly better under multiple
instances; (c) with N replicas writes at most N times per interval, which is fine at
any N worth deploying.
**Wrong call if** `last_seen_at` ever becomes something decisions are made on —
billing, an idle-inbox reaper, "is this inbox alive" in the UI. A minute of lag is
fine for a display and not fine for a reaper.
**The signal to switch** is the first feature that reads `last_seen_at` and cares
about the last sixty seconds, at which point (b) goes on top of (c) rather than
replacing it.

**The map needs eviction, and the sweeper already ticks.**
`EvictTouched` (`internal/ingest/ingest.go:591`) is hung off the existing sweeper tick
in `cmd/hooklens/main.go:131`, exactly like `EvictLimiters` in unit 31. Without it the
map holds one entry per inbox that ever received a capture, for the life of the
process — a slow leak on a service where inboxes are free to create. Forgetting an
entry is always safe: it costs one extra UPDATE on that inbox's next capture, which
is what would have happened at the end of the interval anyway.

**`shouldTouch` marks the inbox before the write, not after.**
So a failed UPDATE is not retried by the next capture. That looks like a bug and is
deliberate: `last_seen_at` is a nicety, and a nicety that retries once per capture
when the database is unhappy is a nicety that amplifies an outage. The next interval
tries again. `TestTouchIsMarkedBeforeTheWrite` pins it so nobody "fixes" it later.

---

## Walkthrough

### `internal/loadgen/loadgen.go` — the generator

**The package comment (`:1`).** Worth reading before the code, because the whole file
is one idea: requests are scheduled against a clock fixed before the run, so the
generator's offered rate does not depend on the server's behaviour.

**`Config` (`:30`).** `Rate` is documented as *offered*, not achieved, and the
distinction is load-bearing enough to be shouted in the comment. `Build` returns a
fresh `*http.Request` per send rather than taking one: a request owns its body
reader, the reader is consumed, and a reused request value sends an empty body the
second time — a bug that would make the run look faster and store smaller rows.

**`Result` (`:62`).** Five separate counts instead of "successes and failures". The
comment says why, and the five map onto the five meanings of "dropped" in the brief.
`Saturated` is the one that does not belong to the server at all.

**`DefaultClient` (`:139`).** The transport tuning is not a micro-optimisation, it is
the difference between measuring the server and measuring the client.
`MaxIdleConnsPerHost` defaults to 2, so without this almost every request opens a
fresh TCP connection and the run measures handshakes plus the generator's ephemeral
port supply. At 1,000/s a machine's ~28,000 ports, held for two minutes each in
`TIME_WAIT`, are gone in under thirty seconds — and the symptom is "the server
started refusing connections", which is a lie about whose fault it was.
`DisableCompression` for the same reason: gzip on both ends is CPU spent measuring
gzip.

**The workers (`:189`).** A fixed pool reading from `queue`. Each worker times from
`j.due` (`:205`), not from just before `Client.Do`. The response body is drained into
`io.Discard` and closed (`:216`) — mandatory, not hygiene: an undrained body means
the connection cannot be returned to the pool, which quietly undoes every line of
`DefaultClient`.

**The scheduler (`:227`–`:255`).** Two lines carry the design.

```go
due := start.Add(time.Duration(i) * interval)
if d := time.Until(due); d > 0 { ...wait... }
```

Sleeping *until* an absolute time rather than *for* an interval. Sleeping for
`interval` accumulates every scheduling delay permanently: on Windows, where timer
granularity is ~15ms, a 60,000-iteration run asking for 1,000/s would silently become
a much longer run at a much lower rate, and the report would still say 1,000/s.
`TestTheClockDoesNotDrift` asserts a one-second run takes about a second.

```go
select {
case queue <- job{due: due}:
default:
    saturated.Add(1)
}
```

The non-blocking send. A plain `queue <- job{...}` would block when every worker is
busy, which re-couples the scheduler to the server and reintroduces coordinated
omission at the last possible moment — the generator would slow down to match the
server and then report that nothing was wrong. `TestSaturationIsReportedNotAbsorbed`
puts a 50ms handler behind 4 workers and asserts `Saturated > 0`.

**`Percentile` (`:117`).** Exact, from sorted samples, unlike the bucket
interpolation in `internal/metrics`. A load test holds every sample in memory and can
afford the truth; a running server cannot, which is the whole reason histograms
exist.

### `internal/server/load_test.go` — the assertion

**The build tag and the file comment (`:1`).** The reasoning for keeping it out of
the default suite and out of CI, written where someone will find it when they wonder
why `go test ./...` does not run it.

**The doc comment on the test (`:43`).** The five definitions of "dropped", each with
what it means and whether it is a bug. This is the actual deliverable of the unit;
the numbers are just evidence.

**Setup (`:80`–`:95`).** Rate limits raised above the offered rate, and a note saying
why leaving them at 50/s would mean measuring unit 31 instead.

**The run and the two counts (`:104`–`:124`).** `CountRequests` before and after,
straddling the run, so the assertion is about rows this run added rather than rows
the table happens to hold.

**`report` before the assertions (`:130`).** Deliberately printed unconditionally. A
failing load test whose output is one assertion message tells you it failed and
nothing else, and the run cost a minute. It prints all three accounts — the
generator's, the server's metrics, the database's — side by side, because
disagreement between them is the finding.

**The assertions (`:132`–`:158`), in order of what they rule out.** Transport
failures first (the server never saw those), then 5xx, then an unexpected 429, then
the durability equality, then saturation. The saturation check is last and is phrased
as "this run does not demonstrate the target" rather than "the server failed",
because that is what it means.

### `internal/ingest/ingest.go` — what the load test found

**`touched` and `touchMu` (`:68`).** The field comment carries the measurement,
because a future reader deleting this map deserves to know it costs 1,700 req/s.

**`shouldTouch` (`:570`).** Lock, compare, record, unlock. The mutex is held for a map
lookup, never across the database call — holding it across the UPDATE would serialise
every inbox behind one round trip and reproduce the original problem in a new place.

**The call site (`:218`).** Still below the durability line, still best-effort, still
logging rather than failing. The only change is that it usually does not happen.

**`EvictTouched` (`:591`)** and its wiring at `cmd/hooklens/main.go:131`, on the
sweeper tick that already exists.

### `internal/store/insert_load_test.go` and `chain_load_test.go` — the diagnostics

Not a test of the system; a measurement that answered one question. When the first
run topped out at 280 req/s, the candidates were the HTTP layer, the pgx pool, and
Postgres itself. Driving `InsertRequest` directly at concurrencies 1, 4, 10, 25 and
50 separated them in about two seconds: the database was not the problem.

`chain_load_test.go` is the follow-up, and it is the one that actually found the bug.
It runs the capture's database work step by step — lookup, then lookup+insert, then
+forward, then +touch — so each step's cost is the difference between two lines
rather than a guess. The last line, 299 captures/s against an observed pipeline rate
of 280, is what turned "something is slow" into "it is the `last_seen_at` update, and
here is the number".

Both are kept in the repo behind the same build tag, because the next time something
is slow these are the first things to re-run.

---

## Verified

`go build ./...`, `go vet ./...`, `gofmt -l .` clean. `go test ./... -count=1` green,
including the six new `internal/loadgen` tests and the five new `internal/ingest`
touch tests. `go vet -tags loadtest ./...` clean.

### The generator's own tests

An untested load generator proves nothing about what it measures: one that silently
offers half the rate it claims turns a passing load test into a passing test of
nothing. `internal/loadgen/loadgen_test.go`:

- `TestSaturationIsReportedNotAbsorbed` — 50ms handler, 4 workers, 400/s offered.
  Asserts `Saturated > 0` and `Sent + Saturated == Scheduled`.
- `TestTheClockDoesNotDrift` — a 1s run takes about 1s and sends exactly 200.
- `TestLatencyIncludesQueueWait` — p99 exceeds the handler's own sleep.
- `TestOutcomesAreCountedSeparately` — 200/429/500 land in three different counters
  and sum to `Sent`.
- `TestTransportErrorsAreNotServerErrors` — a closed port produces `Transport`, not 5xx.
- `TestPercentileIsExact`.

### Run 1 — 1,000 req/s, 60s, before the fix

```
scheduled 60000   sent 16782   saturated 43218 (72%)
status 200 16782  transport 0
p50 1.81s   p95 2.29s   p99 3.40s
accepted 16782 · rows in postgres 16782 · metric stored 16782
```

**Zero dropped captures — every one of the three counts agrees exactly.** The
durability property held. What failed was the saturation check: 280 req/s achieved
against 1,000 offered, so the run does not demonstrate the target.

This is the load test working. A closed-loop generator would have offered 280/s,
reported 100% success and a comfortable latency, and I would have believed the
system did 1,000.

### Diagnosis — measurement, not guesswork

Three candidates for the ceiling: the HTTP layer, the pgx pool, Postgres itself.
`internal/store/insert_load_test.go` drives `InsertRequest` directly:

```
concurrency  1:   326/s     concurrency 10:  1659/s
concurrency  4:   984/s     concurrency 25:  1989/s
                            concurrency 50:  2011/s
```

So the database can absorb ~2,000 inserts/s and flattens just past the pool's
`MaxConns = 10`. It was not the insert. `internal/store/chain_load_test.go` then
measured what a capture actually costs, step by step, at concurrency 64:

```
lookup only               3979 captures/s
insert only               1820 captures/s
lookup+insert             1520 captures/s
lookup+insert+forward      919 captures/s
all four (with touch)      299 captures/s
```

**299/s.** The observed pipeline rate was 280/s. The last line — adding
`TouchEndpoint` — costs more than the other three operations combined, because every
capture for an inbox updates *the same endpoints row*, Postgres takes a row lock per
update, and they serialise. Inserts go to different rows and do not. The bottleneck
was a `last_seen_at` column nobody reads more than once a minute.

### Run 2 — same test, after coalescing the touch

```
scheduled 60000   sent 41450   saturated 18550 (31%)
status 200 41450  transport 0
p50 723ms   p95 854ms   p99 2.41s
accepted 41450 · rows in postgres 41450 · metric stored 41450
```

280 → **690 req/s**, a 2.5x improvement from deleting a write, and still zero dropped
captures. p50 fell from 1.81s to 723ms.

### What is still in the way, and what this run does and does not show

The chain measurement says the remaining ceiling is three database round trips per
capture — resolve the inbox, insert, record the forward outcome — which on this
machine is ~919 captures/s before any HTTP work. 690 observed against a 919 database
ceiling is about the right shape: the gap is the HTTP layer and the ~75% efficiency
you would expect on top.

So, stated plainly: **the 1,000 req/s target is not met on this machine, and the
saturation assertion fails.** Two honest caveats about that number, in both
directions:

- It is a *deployment* property as much as a code property. This is Postgres in
  Docker Desktop on Windows, where a commit is far more expensive than on a Linux
  host; the same code against a native Postgres would very likely clear 1,000
  without further work. I have not measured that, so I am not claiming it.
- It is also a *code* property, and there is one clear thing left to do: the inbox
  lookup is a database round trip on every capture for data that barely changes.
  That is unit 34.

What the run does establish, at every rate tested, is the property the phase is
actually about: **the number of 2xx the server returned, the number the metric
counted, and the number of rows in Postgres were identical, every time.** No capture
was ever acknowledged and lost.

### A note on the failing assertion

`TestLoadCapturesNothingDropped` currently fails on this hardware at its default
rate, on the saturation check alone. That is deliberate and I would rather leave it
red than adjust the default until it passes: the target came from the plan, the
number is real, and a load test whose threshold is set to whatever the machine
happens to do is a test of nothing. Its durability assertions — the ones the name
refers to — pass. To run it at a rate this machine serves:

```
HOOKLENS_LOAD_RATE=600 go test -tags loadtest -run TestLoad -timeout 5m ./internal/server/
```
