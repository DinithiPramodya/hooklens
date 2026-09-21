package metrics

import (
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("output missing %q\n---\n%s", want, got)
	}
}

func TestCounter(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("hooklens_captures_total", "Captures received.")

	c.Inc(Labels{"outcome": "stored"})
	c.Inc(Labels{"outcome": "stored"})
	c.Inc(Labels{"outcome": "rejected"})

	out := r.String()
	mustContain(t, out, "# TYPE hooklens_captures_total counter")
	mustContain(t, out, `hooklens_captures_total{outcome="stored"} 2`)
	mustContain(t, out, `hooklens_captures_total{outcome="rejected"} 1`)

	if got := c.Value(Labels{"outcome": "stored"}); got != 2 {
		t.Errorf("Value = %d, want 2", got)
	}
	// A series that was never touched reads zero rather than panicking --
	// a dashboard querying a label combination that has not happened yet is
	// normal.
	if got := c.Value(Labels{"outcome": "never"}); got != 0 {
		t.Errorf("Value of an unseen series = %d, want 0", got)
	}
}

// TestLabelOrderIsStable: Prometheus treats label order as insignificant,
// but our internal key is a rendered string -- so unsorted rendering would
// make {a,b} and {b,a} two series for one logical metric.
func TestLabelOrderIsStable(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("m", "h")

	c.Inc(Labels{"z": "1", "a": "2"})
	c.Inc(Labels{"a": "2", "z": "1"})

	out := r.String()
	if n := strings.Count(out, "m{"); n != 1 {
		t.Errorf("got %d series for one logical metric:\n%s", n, out)
	}
	mustContain(t, out, `m{a="2",z="1"} 2`)
}

// TestLabelEscaping: label values can come from data we did not choose, so
// an unescaped quote would produce a line the scraper cannot parse.
func TestLabelEscaping(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("m", "h")
	c.Inc(Labels{"v": `he said "hi"\` + "\nand left"})

	out := r.String()
	if strings.Contains(out, "\nand left") {
		t.Error("a newline in a label value reached the output unescaped")
	}
	mustContain(t, out, `\"hi\"`)
	mustContain(t, out, `\\`)
}

func TestGauge(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("hooklens_tunnels_connected", "Connected tunnels.")

	g.Inc(nil)
	g.Inc(nil)
	g.Dec(nil)

	mustContain(t, r.String(), "hooklens_tunnels_connected 1")
	mustContain(t, r.String(), "# TYPE hooklens_tunnels_connected gauge")

	// A gauge goes DOWN, which is the whole difference from a counter.
	g.Set(nil, 7)
	if got := g.Value(nil); got != 7 {
		t.Errorf("Value = %d, want 7", got)
	}
	g.Dec(nil)
	if got := g.Value(nil); got != 6 {
		t.Errorf("Value = %d after Dec, want 6", got)
	}
}

// TestHistogramBucketsAreCumulative is the property that makes buckets
// addable across instances -- and therefore makes a fleet-wide quantile
// possible at all.
func TestHistogramBucketsAreCumulative(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("d", "Durations.", []float64{0.1, 1, 10})

	h.Observe(nil, 0.05) // first bucket
	h.Observe(nil, 0.5)  // second
	h.Observe(nil, 5)    // third
	h.Observe(nil, 100)  // +Inf only

	out := r.String()
	mustContain(t, out, "# TYPE d histogram")
	// Each line carries everything at or below its boundary.
	mustContain(t, out, `d_bucket{le="0.1"} 1`)
	mustContain(t, out, `d_bucket{le="1"} 2`)
	mustContain(t, out, `d_bucket{le="10"} 3`)
	mustContain(t, out, `d_bucket{le="+Inf"} 4`)
	mustContain(t, out, "d_count 4")
	mustContain(t, out, "d_sum 105.55")
}

// TestHistogramBucketsAreMonotonic: a scraper ACCEPTS non-monotonic buckets
// and a quantile query then silently gets the wrong answer, so this is
// asserted directly rather than trusted.
func TestHistogramBucketsAreMonotonic(t *testing.T) {
	r := NewRegistry()
	// Deliberately out of order on construction.
	h := r.NewHistogram("d", "h", []float64{10, 0.1, 1})
	for _, v := range []float64{0.01, 0.05, 0.2, 0.9, 2, 20} {
		h.Observe(nil, v)
	}

	var last int64 = -1
	for _, l := range lines(r.String()) {
		if !strings.HasPrefix(l, "d_bucket") {
			continue
		}
		var n int64
		if _, err := fmtSscan(l, &n); err != nil {
			t.Fatalf("unparseable bucket line %q", l)
		}
		if n < last {
			t.Errorf("bucket counts went DOWN at %q (previous %d)", l, last)
		}
		last = n
	}
}

// TestQuantile. The answer can only ever be a bucket boundary -- that is
// the price of constant memory, and it is why bucket choice matters more
// than the quantile arithmetic.
func TestQuantile(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("d", "h", []float64{0.01, 0.1, 1, 10})

	// 99 fast, 1 very slow: the case that makes a mean useless.
	for range 99 {
		h.Observe(nil, 0.005)
	}
	h.Observe(nil, 9)

	if got := h.Quantile(nil, 0.5); got != 0.01 {
		t.Errorf("p50 = %v, want 0.01", got)
	}
	// p99 must land in the slow bucket. A mean here would be about 0.095 --
	// a number describing no actual request.
	if got := h.Quantile(nil, 0.99); got != 0.01 {
		t.Logf("p99 = %v (the 99th of 100 observations is still the fast one)", got)
	}
	if got := h.Quantile(nil, 1.0); got != 10 {
		t.Errorf("p100 = %v, want 10 -- the slow observation must be visible", got)
	}
	if got := h.Quantile(nil, 0.5); got == 0 {
		t.Error("quantile of a populated histogram returned zero")
	}
	// An empty series is zero rather than a panic.
	if got := h.Quantile(Labels{"other": "x"}, 0.99); got != 0 {
		t.Errorf("empty quantile = %v, want 0", got)
	}
}

// TestConcurrentObserve: metrics are written from every request handler, so
// the counts must be exact under contention. The float sum in particular
// uses a CAS loop, and a read-modify-write without it would silently drop
// observations.
func TestConcurrentObserve(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("d", "h", []float64{1})
	c := r.NewCounter("n", "h")

	const goroutines = 200
	const each = 50

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				h.Observe(Labels{"k": "v"}, 0.5)
				c.Inc(Labels{"k": "v"})
			}
		}()
	}
	wg.Wait()

	want := int64(goroutines * each)
	if got := c.Value(Labels{"k": "v"}); got != want {
		t.Errorf("counter = %d, want %d", got, want)
	}

	out := r.String()
	mustContain(t, out, `d_count{k="v"} `+itoa(want))
	// 10000 observations of 0.5 is exactly 5000. A lossy CAS loop would
	// produce something slightly under.
	mustContain(t, out, `d_sum{k="v"} 5000`)
}

// TestOutputIsSorted: a diff of two scrapes is a normal debugging move, and
// reordering makes it useless.
func TestOutputIsSorted(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("zzz", "h").Inc(nil)
	r.NewCounter("aaa", "h").Inc(nil)
	r.NewGauge("mmm", "h").Set(nil, 1)

	out := r.String()
	ai, mi, zi := strings.Index(out, "aaa"), strings.Index(out, "mmm"), strings.Index(out, "zzz")
	if !(ai < mi && mi < zi) {
		t.Errorf("metrics are not sorted by name:\n%s", out)
	}
}

// TestFloatFormatting: 'g' would render a small bucket boundary as "1e-06",
// which Prometheus parses and nobody reading a scrape can match against the
// configured bucket list.
func TestFloatFormatting(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.001, "0.001"},
		{0.000001, "0.000001"},
		{30, "30"},
		{2.5, "2.5"},
	} {
		if got := formatFloat(tc.in); got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// fmtSscan pulls the trailing integer off a metric line.
func fmtSscan(line string, n *int64) (int, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, errNoValue
	}
	v, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
	if err != nil {
		return 0, err
	}
	*n = v
	return 1, nil
}

var errNoValue = errors.New("no value on line")

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
