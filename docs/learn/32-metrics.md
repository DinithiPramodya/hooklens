# 32 — Counters, gauges, histograms, and why p99

*Phase 5, unit 2. Covers the curriculum bullet "metrics: counters vs gauges vs
histograms, why p99 and not mean".*

## Brief

**What is this thing?** Numbers a running process publishes about itself, in a format
something else scrapes on a timer. Prometheus is the de facto standard, and its exposition
format is plain text over HTTP.

**What problem does it exist to solve?** Logs answer "what happened to *this* request".
They are terrible at "how many requests failed in the last hour" — that requires reading
every line, and lines are expensive to keep. Metrics are the aggregate view: a fixed,
tiny amount of state per *kind* of thing, regardless of traffic.

The distinction that matters is **cardinality**. A log line costs memory per event; a
counter costs eight bytes forever. That is why metrics can be always-on and logs are
sampled — and also why putting a request id in a metric label is a catastrophe, since it
turns the fixed cost back into a per-event one.

**How does it actually work underneath?** Three types, and the difference is *what the
number means over time*.

A **counter** only goes up. Total requests, total failures. You never read a counter's
value directly — you read its **rate of change**, and the fact that it only increases is
what makes that meaningful across a restart: a counter that resets to zero is obviously a
restart, where a number that goes both ways is ambiguous.

A **gauge** goes up and down: connected tunnels, in-flight requests, queue depth. It is a
snapshot, and the current value *is* the information.

A **histogram** counts observations into **cumulative buckets**. For request durations,
you get `le="0.01"` = 7, `le="0.1"` = 42, `le="1"` = 50 — each bucket holding everything
at or below its boundary. The shape lets a quantile be estimated afterwards, and
cumulative-ness is what makes the buckets addable across instances.

**Why p99 rather than mean.** A mean is dominated by the common case and hides exactly
what you need to see. Ninety-nine requests at 10ms and one at 10 seconds give a mean of
about 110ms — a number describing *no actual request*, and one that looks fine. The p99 is
10 seconds and tells you a real user waited that long.

It is worse than misleading: the mean cannot even be recovered usefully from aggregated
data, because **averages of averages are wrong** unless every instance served the same
number of requests. Percentiles from histogram buckets add correctly.

**What are the sharp edges?**

- **High-cardinality labels destroy the system.** A label per user id or per URL path
  creates one time series each, and the memory is in the *scraper*, so the damage lands
  somewhere you are not watching.
- **Bucket boundaries must be chosen before you have data**, and a histogram whose
  buckets are all too small reports only that everything was slow.
- **A metric that nobody alerts on is decoration.** Every one should answer a question
  somebody will actually ask at 3am.
- **`/metrics` is an information leak** if it is public: it publishes traffic volumes and
  error rates.

**In hooklens:** an `internal/metrics` package emitting the Prometheus text format
directly. The official client is 34 modules for something whose wire format is about
thirty lines of text — measured, not assumed, the same way the WebSocket library was
chosen in [18](18-websockets.md).

---

## Decisions

**Hand-rolled exposition, not `prometheus/client_golang`.**
Alternatives were the official client, `VictoriaMetrics/metrics` (a much smaller
third-party client), and OpenTelemetry. The official client pulls 34 modules into
`go.sum` and brings its own registry-as-global-singleton design; OTel brings a whole
SDK, an exporter and a pipeline config for what is currently one scrape endpoint. The
text format the scraper actually consumes is documented in about a page: a HELP
comment line, a TYPE comment line, then `name{labels} value` per series. That is
`internal/metrics/metrics.go`, 470 lines including the comments.
**This is the wrong call if** hooklens ever needs exemplars, native histograms, the
OTLP push protocol, or `/metrics` in protobuf — all of which are real features with
real specs that I would then be reimplementing badly.
**The signal to switch is** the first time I want something the text format cannot
express, or the first bug in this package that a scraper sees and I do not.

**A struct of named fields, not package-level globals.**
The idiomatic Prometheus pattern is a package-scope `var requestsTotal =
promauto.NewCounterVec(...)` registered into a default registry. It is genuinely
convenient: any file can reach any metric. It also means two tests in one binary
share counters, so a test asserting "3 captures" passes alone and fails in a suite,
and it hides which components observe what — a global is reachable from everywhere,
so the dependency appears in no signature. `metrics.App` (`internal/metrics/app.go:20`)
is passed explicitly, and `TestTwoServersDoNotShareMetrics`
(`internal/server/metrics_test.go:115`) is the assertion that it stays that way.
**Wrong call if** the metric set grows to where threading `*App` through every
constructor is real friction. **Signal to switch:** wanting a metric in a leaf
function that has no plausible reason to take an `*App`.

**Labelled by status *class*, never by path.**
`StatusClass` (`internal/metrics/app.go:127`) turns 201 into `"2xx"`. Per-status is
about sixty series per method and answers no question the class does not: "are we
erroring" is 5xx, "are clients misbehaving" is 4xx, and the exact code is in the log
line, where per-event detail belongs. Per-*path* is the one that kills a Prometheus:
it would grow one series for every URL a stranger invents, unbounded, and the memory
is spent in the scraper — somewhere I am not watching.
**Wrong call if** one endpoint's error rate matters independently, which for hooklens
would mean ingest specifically. **Signal to switch:** wanting to alert on "captures
are failing" and finding that the HTTP counter cannot say it — at which point the
answer is a *bounded* route label (the handful of registered patterns), not
`r.URL.Path`.

**Interfaces declared by the consumer, again.**
`ingest.Recorder` (`internal/ingest/ingest.go:517`) is three methods, declared in
`ingest`, and `*metrics.App` happens to satisfy it. `tunnel.TunnelObserver` is two.
Neither package imports `internal/metrics`. Same reasoning as `tunnel.AuthFunc` in
[19](19-tunnel-server.md): the dependency arrow points at the small interface, the
packages stay testable with a four-line fake, and there is no import cycle to
untangle when metrics later needs to know about something in ingest.

**No authentication on `/metrics`, recorded as a deferral.**
It publishes traffic volumes and error rates, which is an information leak on a
public interface. The standard answers are to bind it to a second listener on a
private interface, or to let the ingress terminate it. Both are real work and neither
is free: a second listener is a second port to configure, document and get wrong in
Compose. Deferred deliberately, with the reasoning in the source at
`internal/metrics/app.go:104` so it reads as a decision rather than an oversight.
**The signal to fix it** is the first deployment where hooklens is reachable from the
internet on the same port as the UI — i.e. the launch in Phase 6.

**Zero-initialise the series we know exist.**
Found by scraping a freshly started server: `hooklens_tunnels_connected` was not `0`,
it was *absent*. A metric with no observations emits no line at all. Two consequences,
both bad. A dashboard shows "no data", which is indistinguishable from the exporter
being down — the panel looks the same whether nothing is connected or the process is
dead. And an alert on `hooklens_tunnels_connected == 0` never fires, because the
series does not exist to be zero. The fix is four lines at the end of `NewApp`
(`internal/metrics/app.go:92`). It cannot be done for every series — outcome labels
like `too_large` are only known when they happen — so it covers the ones that are
always meaningful and leaves the rest to appear on first use.

**Buckets fixed at package level, not per metric.**
`DefaultBuckets` (`internal/metrics/metrics.go:275`) runs 1ms to 30s, and both
histograms use it. Per-metric buckets would be tighter, but bucket boundaries have to
be chosen before there is data to choose them from, and a wrong guess is invisible: a
histogram whose buckets are all too small reports only that everything was slow. One
range wide enough for both a loopback HTTP handler and a round trip to a laptop over
a tunnel is the honest starting point. **Signal to switch:** a p99 that sits pinned at
a bucket boundary, which means the real value is somewhere inside that bucket and the
histogram cannot say where.

---

## Walkthrough

### `internal/metrics/metrics.go` — the exposition format

**`Registry` and `collector` (`:30`–`:65`).** The registry is a slice of collectors
plus a mutex; `add` (`:43`) keeps it sorted by name so `Write` emits metrics in a
stable order. Sorting at insert rather than at write is deliberate: registration
happens once at startup, scraping happens every fifteen seconds forever. `Write`
takes a `*strings.Builder` rather than an `io.Writer` because there is no error to
handle, and pretending otherwise would put an `if err != nil` on every line of the
renderer.

**`Labels.render` (`:80`).** Sorts keys before rendering. This is the load-bearing
line in the whole file. Go map iteration is deliberately randomised, so without the
sort the same series renders as `{method="GET",status="2xx"}` on one scrape and
`{status="2xx",method="GET"}` on the next. Prometheus treats those as one series —
but the *text* differs, which breaks any diff-based test and any scraper that hashes
the line. `TestMetricsScrapeIsStable` (`internal/server/metrics_test.go:86`) scrapes
twice and compares.

**`escapeLabel` (`:106`).** Backslash, double quote and newline, in that order —
backslash first, or escaping the quote would then re-escape its own backslash. Label
values in hooklens come from `r.Method`, which is constrained, but this is where a
future label taken from user input stops being an injection into the exposition
format.

**`CounterVec.Add` (`:147`).** Read-lock, look up the series, and on a miss take the
write lock and *re-check* before inserting. The re-check is not redundant: between
dropping the read lock and taking the write lock, another goroutine can insert the
same series, and without the second look one of them would overwrite the other's
counter — losing every increment it had already taken. The stored value is an
`atomic.Int64`, so the common path (series exists) holds only a read lock, and two
goroutines incrementing the same counter do not serialise.

**`HistogramVec.Observe` (`:324`).** The subtle one, and it shipped wrong first.

```go
slot := len(h.buckets) // the +Inf slot
for i, b := range h.buckets {
    if v <= b { slot = i; break }
}
s.counts[slot].Add(1)
```

Exactly **one** slot is incremented: the first bucket the value fits, or `+Inf` if it
fits none. The first version also incremented `+Inf` on every observation, reasoning
that every value is at most infinity — which is true of the *rendered* bucket, and the
render (`:371`) produces cumulative buckets by summing as it walks. So each value was
counted twice in the total. The test asserted a count of 4 and got 7; the arithmetic
only makes sense once you notice the `+Inf` line is built by summation, not stored.
The lesson generalises: when a value is derived at render time, the storage must hold
*non*-cumulative counts, and the two halves have to be read together to see it.

**`addFloat` (`:459`).** A compare-and-swap loop over an `atomic.Uint64` holding
`math.Float64bits`, because Go has no `atomic.Float64`. The loop reloads on every
failed CAS rather than retrying with the stale value — retrying with the stale sum is
exactly how a concurrent `_sum` silently loses observations, and it would only show up
under load. `TestHistogramConcurrentObserve` runs 200 goroutines by 50 observations
and asserts the sum is exactly 5000, not approximately.

**`write` on each type (`:182`, `:246`, `:371`).** Each emits its HELP and TYPE
comment lines, then its series. The histogram emits `_bucket` lines carrying an `le`
label, then `_sum` and `_count`. `withLe` (`:436`) merges `le` into the existing label
set rather than mutating the caller's map — mutating it would corrupt the series key
for every subsequent observation, and the bug would look like metrics randomly
merging into each other.

### `internal/metrics/app.go` — what hooklens actually measures

**The `App` struct (`:20`).** Eight metrics. Each field comment says what question it
answers; the ones that could not answer a question did not get written. Note the pair
at `:41` and `:45`: `TunnelsConnected` is a gauge and goes down, `TunnelConnections`
is a counter and does not. Either alone is ambiguous — a gauge of 0 could mean nobody
has ever connected or everybody just dropped; the counter alongside it distinguishes
"nobody has connected" from "everybody keeps reconnecting", which is a completely
different incident.

**`NewApp` (`:57`) and the zero-init block (`:92`).** Construction, then the four
lines that make the known series exist before anything has happened. `a := &App{…}`
rather than `return &App{…}`, because the zeroing has to run after the metrics are
built — a small mechanical consequence of the fix, and the compiler caught it
immediately.

**`Handler` (`:109`).** Renders into a `strings.Builder` and writes once, so a slow
scraper cannot hold a lock while it reads. `Content-Type: text/plain; version=0.0.4`
— the version parameter is not decoration; without it some scrapers fall back to
content-type guessing.

**`ObserveHTTP` (`:143`) and the `Recorder` methods (`:154`–`:166`).** Thin. All the
judgement is in the labels: `ObserveHTTP` builds one `Labels` map and uses it for both
the counter and the histogram, so the two can never disagree about which series a
request belongs to.

**`TunnelOpened` / `TunnelClosed` (`:171`, `:176`).** The gauge and the lifetime
counter move together, in one place. On the server side `TunnelClosed` is deferred
immediately after the increment (`internal/tunnel/server.go:224`), so every exit path
— including a panic — decrements exactly once. Decrementing by hand before each
`return` is how a gauge slowly becomes fiction, and a gauge nobody trusts is worse
than no gauge.

### Wiring

- `internal/server/middleware.go:85` — `withRequestLog` observes the metric *before*
  writing the log line. Both are cheap, but the metric is the one something alerts on,
  and a logger blocked on a full pipe should not also cost the counter.
- `internal/server/server.go:90` — one `metrics.App` per `Server`, created in `New`.
- `internal/server/server.go:198` — `GET /metrics`, served from the same mux and
  therefore counted by its own middleware. That is intentional and harmless; the
  scrape shows up as a `GET` / `2xx`.
- `internal/ingest/ingest.go:150,182,224` — every call site is guarded by
  `if h.rec != nil`. A nil recorder is a supported configuration, which is what keeps
  every existing ingest test from having to construct a metrics registry.

---

## Verified

- `go build ./...`, `go vet ./...` and `gofmt -l .` clean; `go test ./... -count=1`
  green across all fourteen packages.
- `internal/metrics/metrics_test.go` — 9 tests: counter and gauge round trips, label
  order stability, escaping, cumulative buckets, bucket monotonicity, quantile
  interpolation, 200x50 concurrent `Observe` asserting `_sum` is exactly 5000, sorted
  registry output, float formatting.
- `internal/metrics/app_test.go` — the known series exist at zero on a fresh registry,
  `StatusClass` boundaries, and the tunnel gauge coming back down while the lifetime
  counter does not.
- `internal/server/metrics_test.go` — the endpoint exposes HTTP metrics, has no path
  label, scrapes identically twice, and two `Server`s do not share counters.
- **Scraped a live binary.** Started the server, created an inbox, POSTed three
  webhooks, and read `/metrics`:

  ```
  hooklens_capture_bytes_total 21
  hooklens_captures_total{outcome="stored"} 3
  hooklens_forwards_total{outcome="no_tunnel"} 3
  hooklens_http_requests_total{method="GET",status="4xx"} 1
  hooklens_http_requests_total{method="POST",status="2xx"} 4
  ```

  The numbers are right, and `hooklens_tunnels_connected` being *missing* from that
  output is what produced the zero-initialisation decision above. Every unit test
  passed before that scrape — they exercised the metrics they observed, and could not
  have caught a series that nothing had observed yet. Running the thing found it in
  ten seconds.

### One flake, honestly

During the full-suite run for this unit, `TestShutdownClosesTunnels` failed once with
`read frame: ... EOF` — no close frame at all. It then passed 14 times in isolation
(`-count=8`, and `-count=6 -cpu=1`) and across three more full-suite runs.

The diagnosis, from the shape of it: the test called `cancel()` and only *then* called
`Read`. The server reacts to cancellation by writing the close frame and giving the
closing handshake one second (`closeGrace`) before tearing the socket down. That one
second is a race against the test goroutine being scheduled, and a full
`go test ./...` on a loaded Windows machine is exactly where a goroutine loses a
second. A real CLI is always parked in its read loop, so this was the harness
diverging from production rather than the server misbehaving.

`internal/tunnel/server_test.go:335` now starts the reader in a goroutine and waits
for it to be running before cancelling. That removes the race rather than retrying
through it. What it does **not** do is prove the original diagnosis, because the
failure was never reproduced deliberately. Recorded here as a hypothesis with the
evidence for it, not as a fix with a green test behind it — the same way unit 30's
third flake was recorded.
