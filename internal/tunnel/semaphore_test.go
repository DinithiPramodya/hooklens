package tunnel

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSemaphoreBounds(t *testing.T) {
	s := newSemaphore(3)

	for i := range 3 {
		if err := s.acquire(context.Background()); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
	}
	if s.inFlight() != 3 {
		t.Fatalf("inFlight = %d, want 3", s.inFlight())
	}

	// The fourth must not be granted.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.acquire(ctx); err == nil {
		t.Fatal("a fourth slot was granted on a semaphore of 3")
	}

	s.release()
	if err := s.acquire(context.Background()); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestSemaphoreTryAcquire(t *testing.T) {
	s := newSemaphore(1)
	if !s.tryAcquire() {
		t.Fatal("tryAcquire failed on an empty semaphore")
	}
	if s.tryAcquire() {
		t.Fatal("tryAcquire succeeded on a full semaphore")
	}
	s.release()
	if !s.tryAcquire() {
		t.Fatal("tryAcquire failed after release")
	}
}

// TestSemaphoreNeverExceedsLimit is the property that matters, checked under
// real contention rather than by reading the code.
func TestSemaphoreNeverExceedsLimit(t *testing.T) {
	const limit = 8
	const workers = 200

	s := newSemaphore(limit)
	var current, peak atomic.Int64

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.acquire(context.Background()); err != nil {
				return
			}
			defer s.release()

			n := current.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			current.Add(-1)
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > limit {
		t.Errorf("peak concurrency was %d, above the limit of %d", got, limit)
	}
	if got := peak.Load(); got < limit/2 {
		t.Errorf("peak concurrency was only %d of %d; the semaphore is over-restricting", got, limit)
	}
	if s.inFlight() != 0 {
		t.Errorf("%d slots still held after every worker finished", s.inFlight())
	}
}

// TestSemaphoreRespectsCancellation: the wait must be bounded, or a bounded
// worker count has simply moved the unboundedness into the queue.
func TestSemaphoreRespectsCancellation(t *testing.T) {
	s := newSemaphore(1)
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.acquire(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a queued acquire did not return when its context was cancelled")
	}

	// And the cancelled waiter must not have taken a slot on its way out.
	if s.inFlight() != 1 {
		t.Errorf("inFlight = %d after a cancelled acquire, want 1", s.inFlight())
	}
}

// TestHubBoundsInFlightPerTunnel is failure mode 9 end to end: a burst far
// larger than the limit must never put more than `limit` requests on the wire
// at once, and must not lose any of them.
func TestHubBoundsInFlightPerTunnel(t *testing.T) {
	const burst = 120

	var concurrent, peak atomic.Int64
	release := make(chan struct{})

	ts, srv, _ := newTestServer(t, Options{})
	startFakeCLI(t, srv, func(req Request) *Response {
		n := concurrent.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		<-release // hold every request open until the burst has landed
		concurrent.Add(-1)
		return &Response{Status: 200, BodyB64: req.Path}
	})
	waitConnected(t, ts.Hub(), goodID)

	results := make(chan error, burst)
	for i := range burst {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_, err := ts.Hub().Forward(ctx, goodID, Request{
				Method: "POST", Path: "p" + string(rune('a'+i%26)),
			})
			results <- err
		}()
	}

	// Let the in-flight ones pile up to whatever the limit allows.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && concurrent.Load() < maxInFlightPerTunnel {
		time.Sleep(10 * time.Millisecond)
	}
	// Give any excess a chance to leak through before measuring.
	time.Sleep(300 * time.Millisecond)

	got := peak.Load()
	close(release)

	if got > maxInFlightPerTunnel {
		t.Errorf("peak in-flight was %d, above the per-tunnel limit of %d",
			got, maxInFlightPerTunnel)
	}
	if got < maxInFlightPerTunnel {
		t.Errorf("peak in-flight was only %d of %d; the limit is throttling more than it should",
			got, maxInFlightPerTunnel)
	}

	// Every request must still complete. Bounding is not dropping.
	var failures int
	for range burst {
		select {
		case err := <-results:
			if err != nil {
				failures++
				t.Logf("forward failed: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("timed out waiting for the burst to drain")
		}
	}
	if failures > 0 {
		t.Errorf("%d of %d requests failed; excess should WAIT, not be dropped", failures, burst)
	}

	assertNoPending(t, ts)
}

// TestHubReportsOverloadedNotTimeout: when a slot never comes free before the
// deadline, the recorded reason must say so. Folding it into ErrTimeout would
// send a developer to debug a handler that never ran.
func TestHubReportsOverloadedNotTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	ts, srv, _ := newTestServer(t, Options{})
	startFakeCLI(t, srv, func(req Request) *Response {
		<-release
		return &Response{Status: 200}
	})
	waitConnected(t, ts.Hub(), goodID)

	// Fill every slot with requests that will not answer.
	for range maxInFlightPerTunnel {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_, _ = ts.Hub().Forward(ctx, goodID, Request{Method: "GET", Path: "/hold"})
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ts.hub.mu.Lock()
		c := ts.hub.clients[goodID]
		ts.hub.mu.Unlock()
		if c != nil && c.sem.inFlight() == maxInFlightPerTunnel {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// One more, with a short deadline. It will never get a slot.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := ts.Hub().Forward(ctx, goodID, Request{Method: "GET", Path: "/too-many"})

	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("err = %v, want ErrOverloaded", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("an overloaded tunnel reported ErrTimeout, which blames the local app")
	}
}
