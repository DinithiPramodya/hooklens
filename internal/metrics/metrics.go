// Package metrics publishes counters, gauges and histograms in the
// Prometheus text exposition format.
//
// Hand-rolled rather than using github.com/prometheus/client_golang, which
// pulls in 34 modules -- measured, not assumed. The wire format is about
// thirty lines of text and the types are a few atomics each; that trade
// looked different for the WebSocket library in unit 18, where the protocol
// was genuinely hard and the library brought zero dependencies.
//
// What is given up: exemplars, native histograms, the Go runtime collectors,
// and a pushgateway client. If any of those become necessary, take the
// dependency -- this package is deliberately small enough to delete.
//
// See docs/learn/32-metrics.md.
package metrics

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Registry holds every metric the process publishes.
type Registry struct {
	mu      sync.RWMutex
	metrics []collector
}

// collector is anything that can write itself in the exposition format.
type collector interface {
	write(*strings.Builder)
	name() string
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(c collector) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.metrics = append(r.metrics, c)
}

// Write renders every metric.
//
// Sorted by name so the output is stable between scrapes. Not cosmetic: a
// diff of two scrapes is a normal debugging move, and reordering makes it
// useless.
func (r *Registry) Write(b *strings.Builder) {
	r.mu.RLock()
	cs := slices.Clone(r.metrics)
	r.mu.RUnlock()

	sort.Slice(cs, func(i, j int) bool { return cs[i].name() < cs[j].name() })
	for _, c := range cs {
		c.write(b)
	}
}

func (r *Registry) String() string {
	var b strings.Builder
	r.Write(&b)
	return b.String()
}

// ---- labels ----

// Labels are the dimensions of one time series.
//
// A map, but always rendered in sorted key order: Prometheus treats label
// ORDER as insignificant while our own internal keying is a string, so
// unsorted rendering would create two series for one logical metric.
type Labels map[string]string

func (l Labels) render() string {
	if len(l) == 0 {
		return ""
	}
	keys := slices.Sorted(maps.Keys(l))

	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(k)
		b.WriteString(`="`)
		b.WriteString(escapeLabel(l[k]))
		b.WriteString(`"`)
	}
	b.WriteByte('}')
	return b.String()
}

// escapeLabel escapes the three characters the format reserves.
//
// Without this, a label value containing a quote produces a line the
// scraper cannot parse -- and label values can come from data we did not
// choose, which makes this a correctness issue rather than tidiness.
func escapeLabel(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// ---- counter ----

// CounterVec is a counter with labels.
//
// Counters only go up. You never read the value directly -- you read its
// RATE, and monotonicity is what makes that meaningful across a restart: a
// counter dropping to zero is unambiguously a restart, where a number that
// moves both ways is not.
type CounterVec struct {
	metricName string
	help       string

	mu     sync.RWMutex
	values map[string]*atomic.Int64
	labels map[string]Labels
}

func (r *Registry) NewCounter(name, help string) *CounterVec {
	c := &CounterVec{
		metricName: name,
		help:       help,
		values:     make(map[string]*atomic.Int64),
		labels:     make(map[string]Labels),
	}
	r.add(c)
	return c
}

func (c *CounterVec) name() string { return c.metricName }

// Inc adds one to the series identified by these labels.
func (c *CounterVec) Inc(l Labels) { c.Add(l, 1) }

func (c *CounterVec) Add(l Labels, n int64) {
	key := l.render()

	// Read lock first: the overwhelmingly common case is a series that
	// already exists, and taking a write lock for every increment would
	// serialise the whole process on one mutex.
	c.mu.RLock()
	v, ok := c.values[key]
	c.mu.RUnlock()

	if !ok {
		c.mu.Lock()
		// Re-check: another goroutine may have created it between the
		// unlock and the lock. Without this, two goroutines racing on a new
		// series would each install a counter and one set of increments
		// would vanish.
		if v, ok = c.values[key]; !ok {
			v = new(atomic.Int64)
			c.values[key] = v
			c.labels[key] = l
		}
		c.mu.Unlock()
	}
	v.Add(n)
}

func (c *CounterVec) Value(l Labels) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if v, ok := c.values[l.render()]; ok {
		return v.Load()
	}
	return 0
}

func (c *CounterVec) write(b *strings.Builder) {
	c.mu.RLock()
	keys := slices.Sorted(maps.Keys(c.values))
	type row struct {
		key string
		val int64
	}
	rows := make([]row, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, row{k, c.values[k].Load()})
	}
	c.mu.RUnlock()

	writeHeader(b, c.metricName, c.help, "counter")
	for _, r := range rows {
		fmt.Fprintf(b, "%s%s %d\n", c.metricName, r.key, r.val)
	}
}

// ---- gauge ----

// GaugeVec goes up and down: connected tunnels, in-flight requests. Unlike
// a counter, the current value IS the information.
type GaugeVec struct {
	metricName string
	help       string

	mu     sync.RWMutex
	values map[string]*atomic.Int64
}

func (r *Registry) NewGauge(name, help string) *GaugeVec {
	g := &GaugeVec{metricName: name, help: help, values: make(map[string]*atomic.Int64)}
	r.add(g)
	return g
}

func (g *GaugeVec) name() string { return g.metricName }

func (g *GaugeVec) Set(l Labels, n int64) { g.slot(l).Store(n) }
func (g *GaugeVec) Inc(l Labels)          { g.slot(l).Add(1) }
func (g *GaugeVec) Dec(l Labels)          { g.slot(l).Add(-1) }

func (g *GaugeVec) Value(l Labels) int64 { return g.slot(l).Load() }

func (g *GaugeVec) slot(l Labels) *atomic.Int64 {
	key := l.render()

	g.mu.RLock()
	v, ok := g.values[key]
	g.mu.RUnlock()
	if ok {
		return v
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	if v, ok = g.values[key]; !ok {
		v = new(atomic.Int64)
		g.values[key] = v
	}
	return v
}

func (g *GaugeVec) write(b *strings.Builder) {
	g.mu.RLock()
	keys := slices.Sorted(maps.Keys(g.values))
	type row struct {
		key string
		val int64
	}
	rows := make([]row, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, row{k, g.values[k].Load()})
	}
	g.mu.RUnlock()

	writeHeader(b, g.metricName, g.help, "gauge")
	for _, r := range rows {
		fmt.Fprintf(b, "%s%s %d\n", g.metricName, r.key, r.val)
	}
}

// ---- histogram ----

// DefaultBuckets covers a webhook round trip: sub-millisecond local work up
// to a thirty-second forward deadline.
//
// Boundaries have to be chosen BEFORE there is data, and the failure is
// asymmetric. Buckets all too small report only "everything was slow" and
// no quantile can be recovered; too large and everything lands in the first
// bucket and p99 is indistinguishable from p50. These straddle the
// interesting range by an order of magnitude at each end.
var DefaultBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30,
}

// HistogramVec counts observations into cumulative buckets.
//
// Cumulative -- each bucket holds everything at or below its boundary -- is
// what makes buckets addable across instances, which is what makes a
// quantile computable for a fleet rather than per process.
type HistogramVec struct {
	metricName string
	help       string
	buckets    []float64

	mu     sync.RWMutex
	series map[string]*histogram
	labels map[string]Labels
}

type histogram struct {
	counts []atomic.Int64 // one per bucket, plus one for +Inf
	sum    atomic.Uint64  // float64 bits, for the _sum line
	total  atomic.Int64
}

func (r *Registry) NewHistogram(name, help string, buckets []float64) *HistogramVec {
	if len(buckets) == 0 {
		buckets = DefaultBuckets
	}
	// Sorted, because the cumulative walk below assumes ascending order and
	// an unsorted list would produce non-monotonic buckets -- which a
	// scraper accepts and a quantile query then silently gets wrong.
	bs := slices.Clone(buckets)
	sort.Float64s(bs)

	h := &HistogramVec{
		metricName: name,
		help:       help,
		buckets:    bs,
		series:     make(map[string]*histogram),
		labels:     make(map[string]Labels),
	}
	r.add(h)
	return h
}

func (h *HistogramVec) name() string { return h.metricName }

// Observe records one value, in seconds.
func (h *HistogramVec) Observe(l Labels, v float64) {
	s := h.slot(l)

	// Exactly ONE slot is incremented: the first bucket the value fits, or
	// the +Inf slot if it fits none.
	//
	// The counts stored here are therefore per-bucket, not cumulative --
	// the cumulative view is built when rendering. Incrementing +Inf as
	// well as the matching bucket looks right and double-counts: the render
	// sums as it goes, so every observation would be counted twice in the
	// +Inf line. A test caught exactly that.
	//
	// Linear scan. With thirteen buckets a binary search is slower once the
	// branch predictor is considered, and this runs on every request.
	slot := len(h.buckets) // +Inf
	for i, b := range h.buckets {
		if v <= b {
			slot = i
			break
		}
	}
	s.counts[slot].Add(1)

	s.total.Add(1)
	addFloat(&s.sum, v)
}

func (h *HistogramVec) slot(l Labels) *histogram {
	key := l.render()

	h.mu.RLock()
	s, ok := h.series[key]
	h.mu.RUnlock()
	if ok {
		return s
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if s, ok = h.series[key]; !ok {
		s = &histogram{counts: make([]atomic.Int64, len(h.buckets)+1)}
		h.series[key] = s
		h.labels[key] = l
	}
	return s
}

func (h *HistogramVec) write(b *strings.Builder) {
	h.mu.RLock()
	keys := slices.Sorted(maps.Keys(h.series))
	series := make([]*histogram, len(keys))
	labels := make([]Labels, len(keys))
	for i, k := range keys {
		series[i], labels[i] = h.series[k], h.labels[k]
	}
	h.mu.RUnlock()

	writeHeader(b, h.metricName, h.help, "histogram")

	for i, s := range series {
		// Cumulative: each bucket line carries everything at or below its
		// boundary, so the counts must be summed as we go rather than
		// written raw.
		var cumulative int64
		for j, bound := range h.buckets {
			cumulative += s.counts[j].Load()
			fmt.Fprintf(b, "%s_bucket%s %d\n",
				h.metricName, withLe(labels[i], formatFloat(bound)), cumulative)
		}
		cumulative += s.counts[len(h.buckets)].Load()
		fmt.Fprintf(b, "%s_bucket%s %d\n",
			h.metricName, withLe(labels[i], "+Inf"), cumulative)

		fmt.Fprintf(b, "%s_sum%s %s\n",
			h.metricName, labels[i].render(), formatFloat(loadFloat(&s.sum)))
		fmt.Fprintf(b, "%s_count%s %d\n",
			h.metricName, labels[i].render(), s.total.Load())
	}
}

// Quantile estimates a quantile from the buckets, for tests and for the
// human-readable summary.
//
// An ESTIMATE, and the limit is worth being explicit about: the answer can
// only ever be a bucket boundary, so a p99 of 0.5 means "somewhere between
// 0.25 and 0.5". That is the price of a histogram costing constant memory,
// and it is why bucket choice matters more than the quantile maths.
func (h *HistogramVec) Quantile(l Labels, q float64) float64 {
	s := h.slot(l)
	total := s.total.Load()
	if total == 0 {
		return 0
	}

	target := float64(total) * q
	var cumulative int64
	for i, bound := range h.buckets {
		cumulative += s.counts[i].Load()
		if float64(cumulative) >= target {
			return bound
		}
	}
	return -1 // above the largest bucket; the caller renders this as "+Inf"
}

// ---- helpers ----

func writeHeader(b *strings.Builder, name, help, typ string) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
}

// withLe adds the le label, which a histogram bucket line requires.
func withLe(l Labels, le string) string {
	merged := make(Labels, len(l)+1)
	maps.Copy(merged, l)
	merged["le"] = le
	return merged.render()
}

// formatFloat renders without an exponent and without trailing zeros.
//
// 'g' would produce "1e-06" for a small bucket boundary, which Prometheus
// parses but nobody reading a scrape can match against the configured
// bucket list.
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// addFloat adds to a float64 stored as bits in an atomic, via
// compare-and-swap.
//
// There is no atomic.Float64. The CAS loop is the standard workaround and
// it is correct under contention: a losing racer re-reads and retries, so
// no observation is lost -- which a read-modify-write without the CAS would
// silently drop.
func addFloat(dst *atomic.Uint64, delta float64) {
	for {
		old := dst.Load()
		next := math.Float64bits(math.Float64frombits(old) + delta)
		if dst.CompareAndSwap(old, next) {
			return
		}
	}
}

func loadFloat(src *atomic.Uint64) float64 { return math.Float64frombits(src.Load()) }
