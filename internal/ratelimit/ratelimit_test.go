package ratelimit

import (
	"sync"
	"testing"
	"time"
)

// fakeClock makes every timing assertion here exact. A limiter tested by
// sleeping is slow AND flaky: slow because the waits are real, flaky
// because a loaded CI machine sleeps longer than you asked.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newTestLimiter(rate, burst float64) (*Limiter, *fakeClock) {
	c := newClock()
	l := New(rate, burst)
	l.now = c.now
	return l, c
}

// TestFirstRequestIsAllowed: a new key starts FULL. Starting empty would
// make every client's first request wait, which is indistinguishable from
// the service being broken.
func TestFirstRequestIsAllowed(t *testing.T) {
	l, _ := newTestLimiter(1, 5)
	if ok, _ := l.Allow("fresh"); !ok {
		t.Error("the first request from a new key was refused")
	}
}

func TestBurstThenRefuse(t *testing.T) {
	l, _ := newTestLimiter(1, 3)

	for i := range 3 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d refused inside the burst of 3", i+1)
		}
	}

	ok, retry := l.Allow("k")
	if ok {
		t.Fatal("a fourth request was allowed on a burst of 3")
	}
	// The wait has to be usable as Retry-After, or a well-behaved client
	// cannot behave well.
	if retry <= 0 {
		t.Error("no retryAfter on a refusal")
	}
	if retry > 2*time.Second {
		t.Errorf("retryAfter = %v, want about 1s at 1/sec", retry)
	}
}

// TestLazyRefill is the property that makes a token bucket cheap: no timer,
// no goroutine, tokens computed on access.
func TestLazyRefill(t *testing.T) {
	l, clock := newTestLimiter(2, 2) // 2/sec, burst 2

	l.Allow("k")
	l.Allow("k")
	if ok, _ := l.Allow("k"); ok {
		t.Fatal("bucket was not empty after its burst")
	}

	// Half a second at 2/sec is exactly one token.
	clock.advance(500 * time.Millisecond)
	if ok, _ := l.Allow("k"); !ok {
		t.Error("no token after half a second at 2/sec")
	}
	if ok, _ := l.Allow("k"); ok {
		t.Error("two tokens materialised where one was earned")
	}
}

// TestRefillIsCappedAtBurst: an idle client does not bank unlimited
// allowance. Without the clamp, a key idle for an hour could send 3600
// requests instantly -- which is the burst the limiter exists to prevent.
func TestRefillIsCappedAtBurst(t *testing.T) {
	l, clock := newTestLimiter(1, 3)

	l.Allow("k")
	clock.advance(time.Hour)

	var allowed int
	for range 100 {
		if ok, _ := l.Allow("k"); ok {
			allowed++
		}
	}
	if allowed != 3 {
		t.Errorf("an hour idle yielded %d immediate requests, want the burst of 3", allowed)
	}
}

// TestKeysAreIndependent: one noisy client must not exhaust another's
// allowance. This is the entire point of keying.
func TestKeysAreIndependent(t *testing.T) {
	l, _ := newTestLimiter(1, 2)

	l.Allow("noisy")
	l.Allow("noisy")
	if ok, _ := l.Allow("noisy"); ok {
		t.Fatal("setup: noisy should be exhausted")
	}

	if ok, _ := l.Allow("quiet"); !ok {
		t.Error("a second key was refused because the first was exhausted")
	}
}

// TestEvictRemovesIdleKeys. Without eviction the map grows one entry per
// distinct key forever -- a leak driven by exactly the traffic a limiter
// exists to handle, so an attacker rotating addresses fills memory THROUGH
// the mechanism meant to stop them.
func TestEvictRemovesIdleKeys(t *testing.T) {
	l, clock := newTestLimiter(1, 1)

	for _, k := range []string{"a", "b", "c"} {
		l.Allow(k)
	}
	if l.Len() != 3 {
		t.Fatalf("Len = %d, want 3", l.Len())
	}

	clock.advance(30 * time.Minute)
	l.Allow("c") // c stays fresh

	if n := l.Evict(10 * time.Minute); n != 2 {
		t.Errorf("evicted %d, want 2", n)
	}
	if l.Len() != 1 {
		t.Errorf("Len = %d after eviction, want 1", l.Len())
	}

	// And an evicted key behaves like a new one -- full, so forgetting it
	// cost nothing.
	if ok, _ := l.Allow("a"); !ok {
		t.Error("an evicted key was not treated as new")
	}
}

// TestConcurrentAllow: the limiter is shared across every request handler,
// so the count must be exact under contention. A racy implementation would
// hand out more tokens than exist.
func TestConcurrentAllow(t *testing.T) {
	const burst = 100
	const goroutines = 500

	l, _ := newTestLimiter(0.0001, burst) // effectively no refill during the test

	var mu sync.Mutex
	var allowed int
	var wg sync.WaitGroup

	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := l.Allow("shared"); ok {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if allowed != burst {
		t.Errorf("%d of %d goroutines were allowed, want exactly the burst of %d",
			allowed, goroutines, burst)
	}
}

func TestNewClampsNonsense(t *testing.T) {
	// A zero or negative rate would make the retryAfter arithmetic divide by
	// zero, and a burst below one would refuse everything forever.
	for _, tc := range []struct{ rate, burst float64 }{
		{0, 0}, {-1, -1}, {0, 5}, {5, 0},
	} {
		l := New(tc.rate, tc.burst)
		if ok, _ := l.Allow("k"); !ok {
			t.Errorf("New(%v, %v) refused the first request", tc.rate, tc.burst)
		}
	}
}
