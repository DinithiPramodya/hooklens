package tunnel

import (
	"context"
	"testing"
	"time"
)

// TestBackoffCeilingGrowsAndCaps checks the exponential half.
//
// Full jitter means each delay is random within [0, ceiling), so individual
// values cannot be asserted. What CAN be asserted is the ceiling, by taking
// the maximum over many draws at each attempt: it must double, and it must
// stop at the cap.
func TestBackoffCeilingGrowsAndCaps(t *testing.T) {
	const draws = 3000

	observed := func(attempt int) time.Duration {
		var maxSeen time.Duration
		for range draws {
			b := newBackoff()
			b.attempt = attempt
			if d := b.next(); d > maxSeen {
				maxSeen = d
			}
		}
		return maxSeen
	}

	for attempt := range 8 {
		want := backoffBase << attempt
		if want > backoffMax {
			want = backoffMax
		}
		got := observed(attempt)

		// Never at or above the ceiling: rand.N is exclusive.
		if got >= want {
			t.Errorf("attempt %d: saw %v, which is not below the ceiling %v", attempt, got, want)
		}
		// And close to it, or the ceiling is not what we think. With 3000
		// draws the odds of not reaching 80%% are vanishing.
		if got < want*8/10 {
			t.Errorf("attempt %d: max over %d draws was %v, far below the expected ceiling %v",
				attempt, draws, got, want)
		}
	}

	// Well past the cap, nothing grows and nothing overflows into a negative
	// duration -- which would become a zero-length sleep, i.e. a hot loop.
	for _, attempt := range []int{20, 40, 100, 1000} {
		b := newBackoff()
		b.attempt = attempt
		d := b.next()
		if d < 0 {
			t.Fatalf("attempt %d produced a negative delay %v (shift overflow)", attempt, d)
		}
		if d >= backoffMax {
			t.Errorf("attempt %d produced %v, above the cap %v", attempt, d, backoffMax)
		}
	}
}

// TestBackoffIsJittered is the thundering-herd property, stated as a test.
//
// Two clients failing at the same instant must not compute the same delay.
// Without jitter every draw at a given attempt is identical, and this fails
// with exactly one distinct value.
func TestBackoffIsJittered(t *testing.T) {
	seen := map[time.Duration]bool{}
	for range 200 {
		b := newBackoff()
		b.attempt = 6 // a ceiling of 32s, clamped to 30s: plenty of room
		seen[b.next()] = true
	}
	if len(seen) < 100 {
		t.Errorf("200 draws produced only %d distinct delays; clients would reconnect in lockstep",
			len(seen))
	}
}

// TestBackoffSpreadsLoad measures the thing jitter actually buys, rather than
// just asserting that values differ.
//
// A thousand clients drop together. With a fixed delay all thousand arrive in
// one instant. With full jitter they should be spread roughly evenly across
// the window -- so no single one-tenth slice should hold anything close to
// all of them.
func TestBackoffSpreadsLoad(t *testing.T) {
	const clients = 1000
	const buckets = 10

	b0 := newBackoff()
	b0.attempt = 6
	ceiling := backoffBase << 6
	if ceiling > backoffMax {
		ceiling = backoffMax
	}

	counts := make([]int, buckets)
	for range clients {
		b := newBackoff()
		b.attempt = 6
		d := b.next()
		idx := int(int64(d) * buckets / int64(ceiling))
		if idx >= buckets {
			idx = buckets - 1
		}
		counts[idx]++
	}

	expected := clients / buckets
	for i, n := range counts {
		// Generous bounds: this is asserting "roughly uniform", not a
		// distribution test, and a flaky test here would be worse than none.
		if n < expected/2 || n > expected*2 {
			t.Errorf("bucket %d holds %d of %d clients (expected about %d); arrivals are not spread",
				i, n, clients, expected)
		}
	}
}

func TestBackoffReset(t *testing.T) {
	b := newBackoff()
	for range 10 {
		b.next()
	}
	if b.attempt != 10 {
		t.Fatalf("attempt = %d, want 10", b.attempt)
	}
	b.reset()
	if b.attempt != 0 {
		t.Errorf("attempt = %d after reset, want 0", b.attempt)
	}
	// And the next delay is back under the base ceiling.
	if d := b.next(); d >= backoffBase {
		t.Errorf("first delay after reset was %v, not below %v", d, backoffBase)
	}
}

// TestSleepIsCancellable: without this, ctrl-c during a 30s backoff makes the
// CLI look hung.
func TestSleepIsCancellable(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan bool, 1)
	go func() { done <- sleep(ctx, time.Hour) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case completed := <-done:
		if completed {
			t.Error("sleep reported completion after cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("sleep did not return when its context was cancelled")
	}
}

func TestSleepCompletes(t *testing.T) {
	if !sleep(context.Background(), 10*time.Millisecond) {
		t.Error("sleep reported cancellation for an uncancelled context")
	}
}

// TestCloseErrorPermanence pins which failures are worth retrying.
//
// Getting this wrong in either direction is expensive: retry a permanent
// failure and the CLI busy-waits against an auth endpoint, treat a transient
// one as permanent and it gives up on a server that was merely restarting.
func TestCloseErrorPermanence(t *testing.T) {
	// CodeReplaced is permanent for a different reason than the other two:
	// not "the client is wrong" but "reconnecting starts a fight". Two CLIs
	// on one inbox would evict each other forever.
	permanent := []string{CodeUnauthorized, CodeVersion, CodeReplaced}
	transient := []string{
		CodeServerShutdown, CodeMalformed, CodeHandshake, "unknown", "",
	}

	for _, code := range permanent {
		if !(&CloseError{Code: code}).Permanent() {
			t.Errorf("%q should be permanent: waiting cannot fix a client that is wrong", code)
		}
	}
	for _, code := range transient {
		if (&CloseError{Code: code}).Permanent() {
			t.Errorf("%q should be transient: the CLI would give up on a recoverable failure", code)
		}
	}
}

func TestCloseErrorMessage(t *testing.T) {
	e := &CloseError{Code: CodeVersion, Reason: "upgrade the CLI"}
	if got := e.Error(); got != "upgrade the CLI" {
		t.Errorf("Error() = %q, want the server's reason verbatim", got)
	}
	// A close frame with no reason still has to say something.
	bare := &CloseError{Code: CodeReplaced}
	if got := bare.Error(); got == "" {
		t.Error("a reasonless close produced an empty error message")
	}
}
