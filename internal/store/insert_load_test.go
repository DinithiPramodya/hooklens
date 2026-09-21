//go:build loadtest

package store

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
)

// TestInsertThroughput answers the question the server load test raised:
// when the pipeline tops out, is it the HTTP layer, the pool, or Postgres
// committing?
//
// It drives InsertRequest directly at several concurrency levels. If
// throughput climbs with concurrency up to the pool ceiling and then flattens,
// the pool is the limit. If it flattens well before, the database is.
func TestInsertThroughput(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable"
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database reachable (%v)", err)
	}
	// t.Cleanup, not defer: a deferred Close runs BEFORE any t.Cleanup, so the
	// endpoint cleanup below would find a closed pool. Cleanups run LIFO, so
	// registering Close first makes it run last.
	t.Cleanup(st.Close)

	ep, err := st.CreateEndpoint(ctx, "insert throughput")
	if err != nil {
		t.Fatal(err)
	}
	// Clean up after itself. Not tidiness: an earlier load run left 176,000
	// rows in the development database, and the next `go test ./...` failed
	// in the SWEEPER -- whose retention query was doing a full scan and got
	// slow enough to miss its deadline. The failure looked nothing like "a
	// load test did not clean up".
	t.Cleanup(func() {
		if err := st.DeleteEndpoint(context.Background(), ep.ID); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	body := []byte(`{"event":"load.test","data":{"n":1,"s":"a short but not empty payload"}}`)

	for _, conc := range []int{1, 4, 10, 25, 50} {
		const perWorker = 40
		req := func() *capture.Request {
			return &capture.Request{
				Method:       "POST",
				Path:         "/load",
				Body:         body,
				DeclaredSize: int64(len(body)),
				SourceIP:     netip.MustParseAddr("127.0.0.1"),
				Headers:      []capture.Header{{Name: "Content-Type", Value: "application/json"}},
				ReceivedAt:   time.Now(),
			}
		}

		start := time.Now()
		var wg sync.WaitGroup
		for range conc {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perWorker {
					if _, err := st.InsertRequest(ctx, ep.ID, req()); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()

		elapsed := time.Since(start)
		n := conc * perWorker
		fmt.Printf("concurrency %2d: %4d inserts in %8v = %7.0f/s (%.2fms each)\n",
			conc, n, elapsed.Round(time.Millisecond), float64(n)/elapsed.Seconds(),
			float64(elapsed.Microseconds())/float64(n)/1000*float64(conc))
	}
}
