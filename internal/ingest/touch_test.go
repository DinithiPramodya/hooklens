package ingest

import (
	"testing"
	"time"
)

// newTouchHandler builds the minimum Handler these tests need. shouldTouch
// touches nothing but its own map, so no store, broker or logger is
// required -- which is itself the reason it is a separate method rather than
// three lines inlined in capture().
func newTouchHandler() *Handler {
	return &Handler{touched: map[string]time.Time{}}
}

// TestTouchIsCoalesced is the regression test for the load-test finding: one
// UPDATE per inbox per interval, not one per capture. Before this, 1,000
// captures a second to one inbox meant 1,000 updates a second to one row,
// which Postgres serialises on a row lock.
func TestTouchIsCoalesced(t *testing.T) {
	h := newTouchHandler()
	now := time.Now()

	if !h.shouldTouch("ep-1", now) {
		t.Fatal("the first capture for an inbox must write last_seen_at")
	}

	// A thousand more captures inside the interval.
	for i := range 1000 {
		at := now.Add(time.Duration(i) * time.Millisecond)
		if h.shouldTouch("ep-1", at) {
			t.Fatalf("capture %d inside the interval asked for a second write", i)
		}
	}

	// Past the interval, exactly one more.
	if !h.shouldTouch("ep-1", now.Add(touchInterval+time.Millisecond)) {
		t.Error("no write after the interval elapsed; last_seen_at would freeze")
	}
}

// TestTouchIsPerInbox: the coalescing must not make one busy inbox suppress
// another's first write. The map is keyed by endpoint id for exactly this.
func TestTouchIsPerInbox(t *testing.T) {
	h := newTouchHandler()
	now := time.Now()

	h.shouldTouch("ep-1", now)
	if !h.shouldTouch("ep-2", now) {
		t.Error("a second inbox was suppressed by the first inbox's write")
	}
}

// TestEvictTouched: without eviction the map is one entry per inbox that ever
// received a capture, for the life of the process -- a slow leak on a public
// service where inboxes are free to create.
func TestEvictTouched(t *testing.T) {
	h := newTouchHandler()

	h.shouldTouch("busy", time.Now())
	h.shouldTouch("quiet", time.Now().Add(-time.Hour))

	if n := h.EvictTouched(30 * time.Minute); n != 1 {
		t.Errorf("evicted %d, want 1", n)
	}
	if _, ok := h.touched["busy"]; !ok {
		t.Error("evicted an inbox that was active")
	}

	// Forgetting is always safe: the next capture just writes again.
	if !h.shouldTouch("quiet", time.Now()) {
		t.Error("a forgotten inbox did not write on its next capture")
	}
}

// TestTouchIsMarkedBeforeTheWrite. shouldTouch records the decision even
// though the UPDATE may fail, and that is deliberate -- retrying a failing
// nicety once per capture is how a nicety becomes an outage. This pins the
// behaviour so it is not "fixed" later by someone who reads it as a bug.
func TestTouchIsMarkedBeforeTheWrite(t *testing.T) {
	h := newTouchHandler()
	now := time.Now()

	h.shouldTouch("ep-1", now) // caller's UPDATE fails; nobody tells us

	if h.shouldTouch("ep-1", now.Add(time.Second)) {
		t.Error("a failed write is being retried on the next capture")
	}
}

func TestTouchIsConcurrencySafe(t *testing.T) {
	h := newTouchHandler()
	done := make(chan struct{})

	for range 50 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 200 {
				h.shouldTouch("ep-1", time.Now())
				h.EvictTouched(time.Hour)
			}
		}()
	}
	for range 50 {
		<-done
	}
}
