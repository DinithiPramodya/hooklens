package tunnel

import (
	"context"
	"math/rand/v2"
	"time"
)

// Reconnection timing. See docs/learn/22-backoff-and-jitter.md.
const (
	// backoffBase is the ceiling for the first retry. Short, because the
	// overwhelmingly common cause of a drop is a laptop's Wi-Fi blinking, and
	// making that cost five seconds of downtime would be felt on every
	// coffee-shop connection.
	backoffBase = 500 * time.Millisecond

	// backoffMax caps the ceiling. Without one, doubling reaches hours and
	// the tool looks dead when it is merely patient.
	backoffMax = 30 * time.Second

	// stableSession is how long a connection must last before the attempt
	// counter resets.
	//
	// Resetting on connect -- the obvious choice -- is a bug: a server that
	// accepts and immediately drops produces a tight reconnect loop in which
	// every attempt technically succeeded, so the backoff never grows and the
	// herd never disperses. Requiring the session to have lasted means only a
	// genuinely working connection clears the counter.
	stableSession = 30 * time.Second
)

// backoff produces the delay before the next reconnection attempt.
//
// Full jitter: a uniformly random duration in [0, ceiling), where the ceiling
// doubles per consecutive failure. Not the ceiling itself -- see the note for
// why exponential backoff alone leaves clients synchronised.
type backoff struct {
	base    time.Duration
	max     time.Duration
	attempt int
}

func newBackoff() *backoff {
	return &backoff{base: backoffBase, max: backoffMax}
}

// next returns the delay to wait and advances the attempt counter.
func (b *backoff) next() time.Duration {
	ceiling := b.base << min(b.attempt, 32) // shift, not math.Pow: no floats, no overflow past the clamp
	if ceiling > b.max || ceiling <= 0 {
		// The <= 0 guard catches the shift overflowing into a negative
		// duration, which min(attempt, 32) makes unreachable today and would
		// silently become a zero-length sleep -- a hot loop -- if either
		// constant changed.
		ceiling = b.max
	}
	b.attempt++

	// rand/v2's global source is automatically and randomly seeded per
	// process. That matters here specifically: clients seeded identically
	// would compute identical "random" delays and stay in lockstep, which is
	// the exact failure jitter exists to prevent.
	return rand.N(ceiling)
}

// reset clears the counter. Called only after a session that lasted.
func (b *backoff) reset() { b.attempt = 0 }

// sleep waits for d, or returns false if ctx is cancelled first.
//
// A plain time.Sleep would make ctrl-c take up to backoffMax, which reads as
// a hung program.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
