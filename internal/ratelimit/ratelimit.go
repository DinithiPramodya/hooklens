// Package ratelimit is a token-bucket limiter keyed by an arbitrary string.
//
// See docs/learn/31-rate-limiting.md for why a token bucket rather than a
// fixed window or a sliding log.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter caps how often a key may act.
//
// Safe for concurrent use. One mutex for the whole map rather than a
// sharded or sync.Map design: the critical section is a handful of
// arithmetic operations, and the contention at any traffic level this
// project targets is not measurable. The moment it is, the fix is sharding
// by key hash -- noted here so the next person does not have to rediscover
// the option.
type Limiter struct {
	rate  float64 // tokens added per second
	burst float64 // maximum tokens a key may hold

	// now is time.Now by default and a stub in tests. A limiter tested by
	// sleeping is a slow test suite and a flaky one.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter allowing `rate` actions per second per key, with up
// to `burst` accumulated.
//
// Burst is separate from rate because real traffic is clumpy. A limiter with
// burst == rate rejects two requests arriving in the same millisecond even
// when the client is far under its sustained allowance, which makes it
// useless for anything but a synthetic load.
func New(rate, burst float64) *Limiter {
	if rate <= 0 {
		rate = 1
	}
	if burst < 1 {
		burst = 1
	}
	return &Limiter{
		rate:    rate,
		burst:   burst,
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// Allow takes a token for key, reporting whether one was available and how
// long to wait if not.
//
// The wait is returned rather than logged because it belongs in a
// Retry-After header: a 429 with no indication of when to try again leaves
// a well-behaved client guessing, and guessing usually means retrying too
// soon.
func (l *Limiter) Allow(key string) (ok bool, retryAfter time.Duration) {
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	b, exists := l.buckets[key]
	if !exists {
		// A new key starts FULL. Starting empty would make the first
		// request from every client wait, which is indistinguishable from
		// the service being broken.
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	} else {
		// Lazy refill: no timer, no goroutine, no per-key ticker. The
		// tokens that "would have" accumulated since the last access are
		// computed on access, which is the whole reason a token bucket is
		// cheap.
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens = min(b.tokens+elapsed*l.rate, l.burst)
			b.last = now
		}
	}

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	// Time until one whole token exists.
	need := 1 - b.tokens
	return false, time.Duration(need / l.rate * float64(time.Second))
}

// Evict removes buckets untouched for longer than idle.
//
// Without this the map grows one entry per distinct key forever, which is a
// leak driven by exactly the traffic a rate limiter exists to handle -- an
// attacker rotating source addresses would fill memory through the very
// mechanism meant to stop them.
//
// A full bucket is safe to forget: a new key starts full, so discarding a
// full bucket and recreating it later is indistinguishable from keeping it.
func (l *Limiter) Evict(idle time.Duration) int {
	cutoff := l.now().Add(-idle)

	l.mu.Lock()
	defer l.mu.Unlock()

	var removed int
	for k, b := range l.buckets {
		if b.last.Before(cutoff) {
			delete(l.buckets, k)
			removed++
		}
	}
	return removed
}

// Len reports how many keys are tracked. Test and metrics support.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
