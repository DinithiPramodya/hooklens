package loadgen

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// An untested load generator proves nothing about the system it measures: if
// it silently offers half the rate it claims, a passing load test is a
// passing test of nothing. These tests are about the generator's own honesty,
// not about hooklens.

func build(url string) func() (*http.Request, error) {
	return func() (*http.Request, error) {
		return http.NewRequest("POST", url, strings.NewReader(`{"ok":true}`))
	}
}

// TestSaturationIsReportedNotAbsorbed is the coordinated-omission test.
//
// The server here takes 50ms per request and there are 4 workers, so the
// pipeline drains at most 80 req/s. Offering 400/s must NOT quietly become
// 80/s: the scheduler has to keep its clock, fail to dispatch, and say so.
// A generator that blocked on the queue instead would report a tidy 100%
// success rate and a low latency, having measured the server's comfortable
// speed rather than its behaviour under the load asked for.
func TestSaturationIsReportedNotAbsorbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	res := Run(context.Background(), Config{
		Rate:     400,
		Duration: 500 * time.Millisecond,
		Workers:  4,
		Build:    build(srv.URL),
		Client:   DefaultClient(4),
	})

	if res.Scheduled != 200 {
		t.Errorf("Scheduled = %d, want 200 (400/s for 0.5s)", res.Scheduled)
	}
	if res.Saturated == 0 {
		t.Error("Saturated = 0: the generator absorbed the overload instead of reporting it")
	}
	if res.Sent+res.Saturated != res.Scheduled {
		t.Errorf("Sent(%d) + Saturated(%d) = %d, want Scheduled %d -- requests went missing",
			res.Sent, res.Saturated, res.Sent+res.Saturated, res.Scheduled)
	}
}

// TestTheClockDoesNotDrift: scheduling against a fixed start, rather than
// sleeping for one interval at a time, is what keeps a 60s run at 1k/s from
// becoming a 70s run at 860/s while still reporting 1k/s.
func TestTheClockDoesNotDrift(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	start := time.Now()
	res := Run(context.Background(), Config{
		Rate:     200,
		Duration: time.Second,
		Workers:  16,
		Build:    build(srv.URL),
		Client:   DefaultClient(16),
	})
	elapsed := time.Since(start)

	if res.Sent != 200 {
		t.Errorf("Sent = %d, want 200", res.Sent)
	}
	// Generous upper bound: this asserts the absence of per-iteration drift,
	// not the precision of the OS timer. Windows timer granularity alone is
	// ~15ms, which over 200 iterations would be 3 seconds of drift if each
	// sleep were independent.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("a 1s run took %v -- the schedule is drifting", elapsed)
	}
}

// TestLatencyIncludesQueueWait. To the sender, waiting for a free worker and
// waiting for a slow server are the same wait. A generator that timed only
// Client.Do would report this server as fast while every caller experienced
// it as slow.
func TestLatencyIncludesQueueWait(t *testing.T) {
	const handler = 30 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(handler)
	}))
	defer srv.Close()

	res := Run(context.Background(), Config{
		Rate:     200,
		Duration: 500 * time.Millisecond,
		Workers:  2, // drains ~66/s, so a queue forms immediately
		Build:    build(srv.URL),
		Client:   DefaultClient(2),
	})

	if p99 := res.Percentile(0.99); p99 <= handler {
		t.Errorf("p99 = %v, not more than the %v handler: queue wait was not counted", p99, handler)
	}
}

// TestOutcomesAreCountedSeparately. The whole reason Result has five fields
// instead of a failure count: 429 is the rate limiter working, 500 is a bug,
// and collapsing them loses the only thing worth knowing.
func TestOutcomesAreCountedSeparately(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) % 3 {
		case 0:
			w.WriteHeader(http.StatusOK)
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	res := Run(context.Background(), Config{
		Rate:     300,
		Duration: 300 * time.Millisecond,
		Workers:  16,
		Build:    build(srv.URL),
		Client:   DefaultClient(16),
	})

	if res.Accepted() == 0 || res.RateLimited() == 0 || res.ServerErrors() == 0 {
		t.Fatalf("accepted=%d rate_limited=%d server_errors=%d: an outcome class was lost",
			res.Accepted(), res.RateLimited(), res.ServerErrors())
	}
	if got := res.Accepted() + res.RateLimited() + res.ServerErrors() + res.Transport; got != res.Sent {
		t.Errorf("outcomes sum to %d but %d were sent", got, res.Sent)
	}
}

// TestTransportErrorsAreNotServerErrors. A connection refused never reached a
// handler, so the server's own metrics cannot know about it -- which is
// exactly why the generator has to count it, and why it must not be filed as
// a 5xx.
func TestTransportErrorsAreNotServerErrors(t *testing.T) {
	// Bind and immediately release, so the port is almost certainly free and
	// nothing is listening on it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	url := "http://" + l.Addr().String() + "/"
	l.Close()

	res := Run(context.Background(), Config{
		Rate:     100,
		Duration: 200 * time.Millisecond,
		Workers:  8,
		Build:    build(url),
		Client:   DefaultClient(8),
	})

	if res.Transport != res.Sent {
		t.Errorf("Transport = %d of %d sent, want all of them", res.Transport, res.Sent)
	}
	if res.ServerErrors() != 0 {
		t.Errorf("ServerErrors = %d: a refused connection was filed as a server failure", res.ServerErrors())
	}
}

func TestPercentileIsExact(t *testing.T) {
	r := &Result{ByStatus: map[int]int64{}}
	for i := 100; i >= 1; i-- {
		r.Latencies = append(r.Latencies, time.Duration(i)*time.Millisecond)
	}
	// Unsorted on purpose: Percentile must not assume finish() ran.
	if got, want := r.Percentile(0.5), 50*time.Millisecond; got != want {
		t.Errorf("p50 = %v, want %v", got, want)
	}
	if got, want := r.Percentile(0.99), 99*time.Millisecond; got != want {
		t.Errorf("p99 = %v, want %v", got, want)
	}
	if got := (&Result{}).Percentile(0.99); got != 0 {
		t.Errorf("p99 of nothing = %v, want 0", got)
	}
}
