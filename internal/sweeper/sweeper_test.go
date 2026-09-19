package sweeper

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database reachable (%v) -- run `docker compose up -d`", err)
	}
	t.Cleanup(st.Close)
	return st
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRunStopsOnCancel is the property main depends on. If Run does not return
// when its context is cancelled, the WaitGroup in run() blocks forever and
// SIGTERM turns into a hang until the platform SIGKILLs the process.
func TestRunStopsOnCancel(t *testing.T) {
	st := testStore(t)

	sw := New(st, quietLogger(), 50*time.Millisecond, 10)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(done)
	}()

	// Let at least one tick fire so we are cancelling a running worker rather
	// than one still on its first select.
	time.Sleep(120 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}
}

// TestRunAlreadyCancelled covers the ordering hazard: if the context is dead
// before Run starts, it must return immediately rather than waiting a full
// interval for a tick it will then ignore.
func TestRunAlreadyCancelled(t *testing.T) {
	st := testStore(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sw := New(st, quietLogger(), time.Hour, 10) // interval far longer than the timeout

	done := make(chan struct{})
	go func() {
		sw.Run(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run blocked on a dead context -- select must check ctx.Done()")
	}
}

// TestSweepDeletes is the end-to-end behaviour: leave it running and expired
// rows disappear without anyone asking.
func TestSweepDeletes(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if err := st.SetRetention(ctx, ep.ID, 1); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}

	id, err := st.InsertRequest(ctx, ep.ID, &capture.Request{
		Method: "POST", Path: "/ancient", DeclaredSize: -1,
		ReceivedAt: time.Now().UTC().Add(-5 * time.Hour),
	})
	if err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	sw := New(st, quietLogger(), 30*time.Millisecond, 100)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go sw.Run(runCtx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := st.GetRequest(ctx, id); err != nil {
			return // swept
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("the expired request was still present after 3s of sweeping")
}

// TestSweepOncePanicIsContained is the one that matters most.
//
// A panic in a bare goroutine takes down the entire process -- there is no
// per-connection recover the way net/http provides for handlers. A nil *Store
// makes sweepOnce dereference nil; without the recover in sweepOnce this test
// does not fail, it CRASHES the test binary.
func TestSweepOncePanicIsContained(t *testing.T) {
	sw := &Sweeper{
		store:    nil, // guarantees a nil dereference inside
		log:      quietLogger(),
		interval: time.Second,
		batch:    10,
	}

	sw.sweepOnce(context.Background())

	// Reaching this line at all is the assertion.
	t.Log("sweepOnce contained the panic; the process survived")
}
