// Package loadgen is an open-loop HTTP load generator.
//
// Open-loop is the entire point. The obvious load generator sends a request,
// waits for the response, and sends the next one -- which means it offers
// exactly the rate the server is willing to accept, and every time the server
// slows down the generator politely slows down with it. The requests that
// would have been slow are the ones that were never sent, so they never appear
// in the latency numbers, and the worse the server behaves the better the
// measurements look. That is coordinated omission, and it is the default
// failure of hand-written load tests.
//
// Here, requests are scheduled against a clock fixed before the run starts.
// Request i is due at start+i*interval whatever happened to request i-1, and
// its latency is measured from that due time, not from when a worker happened
// to pick it up. Queue wait is latency, because to the sender it is
// indistinguishable from the server being slow.
package loadgen

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// Config describes one run.
type Config struct {
	// Rate is the OFFERED load in requests per second. Offered, not
	// achieved: the whole design exists so that the two can differ and the
	// difference is visible.
	Rate float64

	// Duration is how long to keep offering that rate.
	Duration time.Duration

	// Workers bounds in-flight requests. A bound is necessary -- unbounded
	// goroutines against a slow server is a memory leak with extra steps --
	// but it is also a measurement hazard, which is why Result.Saturated
	// exists.
	Workers int

	// Build returns a fresh *http.Request for each send. A fresh one, not a
	// shared one: a request carries its own Body reader, which is consumed,
	// so reusing the value sends an empty body the second time.
	Build func() (*http.Request, error)

	// Client is the HTTP client. Callers should pass one with a transport
	// tuned for the run -- see DefaultClient for why the stdlib default is
	// wrong here.
	Client *http.Client
}

// Result is what the run measured, counted by outcome rather than collapsed
// into "errors".
//
// The separation is the deliverable. "Dropped" means at least five different
// things to this system and only one of them is a bug; a single failure count
// cannot tell them apart.
type Result struct {
	// Scheduled is how many requests the clock called for: Rate*Duration.
	Scheduled int64

	// Saturated counts requests the generator could not dispatch because
	// every worker was busy. These never reached the network. It is a
	// finding about capacity -- offered load exceeded what the pipeline
	// drains -- but it is a GENERATOR-side finding, and reporting it as a
	// server error would be a lie.
	Saturated int64

	// Sent is how many requests actually went out.
	Sent int64

	// Transport counts failures before any HTTP status existed: connection
	// refused, reset, timeout, DNS. Distinct from a 5xx, because the server
	// never saw these and its own metrics cannot know about them.
	Transport int64

	// ByStatus counts responses by HTTP status code.
	ByStatus map[int]int64

	// Latencies, one per completed response, measured from the SCHEDULED
	// send time.
	Latencies []time.Duration

	mu sync.Mutex
}

// Accepted is 2xx: the server said it took the webhook.
func (r *Result) Accepted() int64 { return r.classCount(2) }

// RateLimited is 429 specifically, not all of 4xx. A 429 is the rate limiter
// doing its job and is not a drop; a 400 is a different story.
func (r *Result) RateLimited() int64 { return r.ByStatus[http.StatusTooManyRequests] }

// ServerErrors is 5xx: the server saw the request and failed it. This is the
// count that should be zero.
func (r *Result) ServerErrors() int64 { return r.classCount(5) }

func (r *Result) classCount(class int) int64 {
	var n int64
	for code, c := range r.ByStatus {
		if code/100 == class {
			n += c
		}
	}
	return n
}

// Percentile returns the qth percentile latency, q in [0,1].
//
// Exact, from the sorted samples, rather than interpolated from histogram
// buckets as in internal/metrics. A load test holds every sample in memory
// and can afford the truth; a running server cannot.
func (r *Result) Percentile(q float64) time.Duration {
	if len(r.Latencies) == 0 {
		return 0
	}
	// Sort on demand rather than assuming finish() ran: Percentile is also
	// called on a Result built by hand in tests, and a wrong answer from an
	// unsorted slice would be silently plausible.
	if !slices.IsSorted(r.Latencies) {
		slices.Sort(r.Latencies)
	}
	i := int(q * float64(len(r.Latencies)-1))
	return r.Latencies[i]
}

// DefaultClient returns a client suited to a load run.
//
// The stdlib default transport keeps at most 2 idle connections per host
// (MaxIdleConnsPerHost). At any real rate that means almost every request
// opens a fresh TCP connection, so the run measures handshakes and the
// generator's own ephemeral port supply rather than the server. On Windows
// and macOS it also walks straight into TIME_WAIT exhaustion: ~28k ports, two
// minutes each, gone in half a minute at 1k/s.
func DefaultClient(workers int) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	// Room for every worker to hold a connection open between requests.
	t.MaxIdleConns = workers * 2
	t.MaxIdleConnsPerHost = workers * 2
	t.MaxConnsPerHost = workers * 2
	t.IdleConnTimeout = 90 * time.Second
	// Compression costs CPU on both sides and would be measuring the wrong
	// thing.
	t.DisableCompression = true
	return &http.Client{
		Transport: t,
		// Long enough that a slow response is recorded as slow rather than
		// converted into a transport error, which would put it in the wrong
		// bucket entirely.
		Timeout: 30 * time.Second,
	}
}

// Run offers Config.Rate for Config.Duration and returns what happened.
//
// It returns when every dispatched request has completed or failed, so the
// counts are final. Cancelling ctx stops the scheduler; in-flight requests
// still finish, because abandoning them would leave the counts incomplete in
// exactly the situation where you most want them.
func Run(ctx context.Context, cfg Config) *Result {
	if cfg.Workers <= 0 {
		cfg.Workers = 64
	}
	res := &Result{ByStatus: map[int]int64{}}

	interval := time.Duration(float64(time.Second) / cfg.Rate)
	total := int64(cfg.Duration.Seconds() * cfg.Rate)
	res.Scheduled = total

	// A tiny struct rather than a closure, so the due time travels with the
	// work item. This is the open-loop invariant made concrete: the worker
	// measures against `due`, which was computed before the run began and
	// owes nothing to how long anything took.
	type job struct{ due time.Time }

	// Buffered by one worker's worth. The buffer absorbs jitter in worker
	// pickup without decoupling the scheduler from reality: when it fills,
	// the scheduler does NOT block -- it records Saturated and moves on, so
	// the clock keeps running at the offered rate.
	queue := make(chan job, cfg.Workers)

	var saturated, sent, transport atomic.Int64

	var wg sync.WaitGroup
	for range cfg.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				req, err := cfg.Build()
				if err != nil {
					transport.Add(1)
					continue
				}
				sent.Add(1)

				resp, err := cfg.Client.Do(req)
				// Latency from the DUE time, not from now. If this worker was
				// busy for 200ms before picking the job up, those 200ms are
				// part of what a real sender would have experienced.
				lat := time.Since(j.due)

				if err != nil {
					transport.Add(1)
					continue
				}
				// Drain and close, or the connection cannot be reused and the
				// pool tuning above is pointless. io.Discard, not ReadAll into
				// a slice: 60,000 response bodies retained would measure the
				// generator's garbage collector.
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()

				res.mu.Lock()
				res.ByStatus[resp.StatusCode]++
				res.Latencies = append(res.Latencies, lat)
				res.mu.Unlock()
			}
		}()
	}

	start := time.Now()
	for i := int64(0); i < total; i++ {
		due := start.Add(time.Duration(i) * interval)

		// Sleep until this request is due. Not "sleep interval" -- that
		// accumulates every scheduling delay into permanent drift, so a run
		// asking for 60s at 1k/s quietly becomes 70s at 860/s and the report
		// says 1k/s.
		if d := time.Until(due); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-ctx.Done():
				t.Stop()
				close(queue)
				wg.Wait()
				res.finish(saturated.Load(), sent.Load(), transport.Load())
				return res
			}
		}

		select {
		case queue <- job{due: due}:
		default:
			// Every worker is busy. Recording and continuing is the honest
			// behaviour: blocking here would silently convert an overloaded
			// system into a slower offered rate, which is coordinated
			// omission reintroduced at the last possible moment.
			saturated.Add(1)
		}
	}

	close(queue)
	wg.Wait()
	res.finish(saturated.Load(), sent.Load(), transport.Load())
	return res
}

func (r *Result) finish(saturated, sent, transport int64) {
	r.Saturated = saturated
	r.Sent = sent
	r.Transport = transport
	slices.Sort(r.Latencies)
}
