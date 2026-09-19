// Package sweeper deletes captured requests past their inbox's retention
// window.
//
// It runs inside the server process on a ticker rather than as a separate cron
// job or container. That choice ships with the binary and cannot drift from it,
// and it stops being correct the moment there is more than one instance -- see
// docs/learn/10-background-workers.md.
package sweeper

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/store"
)

const (
	// DefaultInterval is how often the sweep runs. Retention is measured in
	// hours, so nothing is gained by sweeping more often than minutes -- rows
	// linger a little past their window either way, and that is fine.
	DefaultInterval = 5 * time.Minute

	// DefaultBatch caps one DELETE statement. A single unbounded delete over a
	// large backlog holds locks for its whole duration and writes one WAL
	// record per row before anything commits. Batching turns that into many
	// short transactions that can be interrupted between them.
	DefaultBatch = 1000

	// maxBatchesPerTick stops one tick from running until the backlog is gone.
	// Without it, a first run against months of accumulated data would hold a
	// connection and hammer the database for as long as it took. Leftovers are
	// simply collected by the next tick.
	maxBatchesPerTick = 20
)

type Sweeper struct {
	store    *store.Store
	log      *slog.Logger
	interval time.Duration
	batch    int
}

func New(st *store.Store, log *slog.Logger, interval time.Duration, batch int) *Sweeper {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if batch <= 0 {
		batch = DefaultBatch
	}
	return &Sweeper{store: st, log: log, interval: interval, batch: batch}
}

// Run sweeps on a ticker until ctx is cancelled. It blocks, so callers run it
// in a goroutine -- and must wait for it to return before closing the database
// pool.
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	// Not optional. Without Stop the runtime keeps this ticker's timer entry
	// alive after Run has returned.
	defer ticker.Stop()

	s.log.Info("sweeper started", "interval", s.interval, "batch", s.batch)

	for {
		select {
		case <-ctx.Done():
			s.log.Info("sweeper stopped")
			return
		case <-ticker.C:
			// Ticks are DROPPED, not queued: the channel buffers one. If a
			// sweep outruns the interval we simply run again immediately
			// afterwards rather than accumulating a backlog of ticks.
			s.sweepOnce(ctx)
		}
	}
}

// sweepOnce deletes one tick's worth of expired requests.
//
// It never returns an error. A sweep failure is operational noise, not
// something a caller can act on -- the next tick will try again -- so it is
// logged and swallowed here rather than propagated to a caller that would only
// log it.
func (s *Sweeper) sweepOnce(ctx context.Context) {
	// A panic in a goroutine takes down the whole process. There is no
	// equivalent of net/http's per-connection recover above a bare goroutine,
	// so one nil dereference in here would kill the server mid-request for
	// every user. This recover is the only thing standing between a bug in the
	// sweeper and an outage.
	defer func() {
		if rec := recover(); rec != nil {
			s.log.Error("sweeper panicked; continuing",
				"panic", rec, "stack", string(debug.Stack()))
		}
	}()

	start := time.Now()
	var total int64

	for i := range maxBatchesPerTick {
		// Check cancellation between batches, not only between ticks. A sweep
		// working through a backlog would otherwise ignore SIGTERM for as long
		// as it took, and the shutdown drain would time out around it.
		if ctx.Err() != nil {
			return
		}

		n, err := s.store.DeleteExpiredRequests(ctx, s.batch)
		if err != nil {
			// A cancelled context during shutdown is expected, not a fault.
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			s.log.Error("sweep failed", "batch", i, "deleted_so_far", total, "err", err)
			return
		}

		total += n
		if n < int64(s.batch) {
			// A short batch means the backlog is drained.
			break
		}
	}

	if total > 0 {
		s.log.Info("sweep complete", "deleted", total, "duration_ms", time.Since(start).Milliseconds())
	}
}
