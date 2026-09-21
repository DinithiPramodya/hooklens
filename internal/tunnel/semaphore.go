package tunnel

import "context"

// maxInFlightPerTunnel bounds how many requests one tunnel may have out at
// once -- failure mode 9.
//
// Per tunnel rather than per process, so one busy inbox cannot starve every
// other one. 32 is chosen from what the slow end can take: the far side is a
// development server on somebody's laptop, often single-threaded, and handing
// it hundreds of simultaneous requests is not a kindness. It is also well
// above what any real provider sends for a single inbox, so the limit should
// be invisible until something is genuinely wrong.
const maxInFlightPerTunnel = 32

// semaphore is a counting semaphore.
//
// A buffered channel and nothing else: sending is acquire, receiving is
// release, and the capacity is the limit. When the buffer is full the send
// blocks, and that blocking IS the waiting -- no condition variable, no
// counter, nothing to get wrong under contention.
type semaphore chan struct{}

func newSemaphore(n int) semaphore {
	if n <= 0 {
		n = 1
	}
	return make(semaphore, n)
}

// acquire takes a slot, waiting until one is free or ctx expires.
//
// Returning ctx.Err() rather than blocking forever is what keeps the queue
// bounded. A bounded worker count with an unbounded queue is not bounded --
// it has only moved the problem somewhere less visible -- and here the
// caller's deadline is what bounds it.
func (s semaphore) acquire(ctx context.Context) error {
	select {
	case s <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tryAcquire takes a slot only if one is free right now.
func (s semaphore) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

// release returns a slot.
//
// Always deferred at the point of acquisition. One early return that skips it
// permanently shrinks the capacity, and enough of those deadlock the tunnel
// with no error logged anywhere -- the failure is silent and cumulative.
func (s semaphore) release() { <-s }

// inFlight reports how many slots are taken. Test support.
func (s semaphore) inFlight() int { return len(s) }
