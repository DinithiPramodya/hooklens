//go:build loadtest

package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
)

// TestCaptureChainThroughput measures the database work one capture actually
// costs, rather than the insert alone.
//
// The first fix took the pipeline from 280 to 690 req/s and the next
// candidate was a guess, which is how a morning gets spent optimising the
// wrong thing. A capture makes three round trips -- resolve the inbox,
// insert the request, record the forward outcome -- so this drives all three
// at the concurrency the load test uses and reports the ceiling they impose.
// If the number lands near the observed rate, the database is the limit and
// the HTTP layer is innocent.
func TestCaptureChainThroughput(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable"
	}
	ctx := context.Background()
	st, err := Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database reachable (%v)", err)
	}
	defer st.Close()

	ep, err := st.CreateEndpoint(ctx, "chain throughput")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"event":"load.test","data":{"n":1,"s":"a short but not empty payload"}}`)

	steps := []struct {
		name string
		run  func() error
	}{
		{"lookup only", func() error {
			_, err := st.EndpointBySlug(ctx, ep.Slug)
			return err
		}},
		{"insert only", func() error {
			_, err := st.InsertRequest(ctx, ep.ID, newReq(body))
			return err
		}},
		{"lookup+insert", func() error {
			if _, err := st.EndpointBySlug(ctx, ep.Slug); err != nil {
				return err
			}
			_, err := st.InsertRequest(ctx, ep.ID, newReq(body))
			return err
		}},
		{"lookup+insert+forward", func() error {
			if _, err := st.EndpointBySlug(ctx, ep.Slug); err != nil {
				return err
			}
			id, err := st.InsertRequest(ctx, ep.ID, newReq(body))
			if err != nil {
				return err
			}
			return st.RecordForward(ctx, id, ForwardOutcome{Error: "no_tunnel", Elapsed: 1})
		}},
		{"all four (with touch)", func() error {
			if _, err := st.EndpointBySlug(ctx, ep.Slug); err != nil {
				return err
			}
			id, err := st.InsertRequest(ctx, ep.ID, newReq(body))
			if err != nil {
				return err
			}
			if err := st.TouchEndpoint(ctx, ep.ID, time.Now()); err != nil {
				return err
			}
			return st.RecordForward(ctx, id, ForwardOutcome{Error: "no_tunnel", Elapsed: 1})
		}},
	}

	const conc = 64
	const perWorker = 30

	for _, step := range steps {
		start := time.Now()
		var wg sync.WaitGroup
		for range conc {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perWorker {
					if err := step.run(); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()

		n := conc * perWorker
		fmt.Printf("%-24s %5d ops in %8v = %7.0f captures/s\n",
			step.name, n, time.Since(start).Round(time.Millisecond),
			float64(n)/time.Since(start).Seconds())
	}
}

func newReq(body []byte) *capture.Request {
	return newCaptureRequest(body)
}
