//go:build loadtest

// This file is behind a build tag on purpose.
//
// A 60-second test in the default suite is a 60-second tax on every `go test
// ./...`, and a test that slow gets skipped, then ignored, then deleted. It
// also cannot be trusted in CI: a shared runner under an unknown neighbour's
// load produces a throughput number that means nothing, and a load test that
// fails for reasons unrelated to the code is worse than no load test, because
// it teaches everyone to ignore a red build.
//
// Run it deliberately:
//
//	go test -tags loadtest -run TestLoad -timeout 5m ./internal/server/
//
// Tunable with HOOKLENS_LOAD_RATE, HOOKLENS_LOAD_SECONDS, HOOKLENS_LOAD_WORKERS.
package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/loadgen"
	"github.com/DinithiPramodya/hooklens/internal/store"
)

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

// TestLoadCapturesNothingDropped is the Phase 5 target: 1,000 req/s for 60
// seconds with zero dropped captures.
//
// "Dropped" is the word that needed defining before this test could be
// written, because it means five different things here and only one of them
// is a bug:
//
//  1. A connection refused or reset before HTTP began. The server never saw
//     it, so no server-side metric can know about it. Counted as Transport.
//     Must be zero -- a refused connection at this rate means the accept
//     queue overflowed.
//  2. A 429 from the rate limiter. Not a drop: that is the limiter doing its
//     job, and this run configures the limit above the offered rate so it
//     should not fire at all. If it does, the test says so rather than
//     counting it as a loss.
//  3. A 5xx. The server saw the request and failed it. A bug, and must be
//     zero.
//  4. A 2xx whose row never reached Postgres. THIS is the dropped capture
//     the phrase is really about: the server promised durability and did not
//     deliver. The only way to test it is to ask the database, which is why
//     store.CountRequests exists.
//  5. A capture stored but not broadcast to a slow SSE consumer. Deliberate
//     backpressure from unit 14 -- dropping a live update to protect the
//     process is a design decision, not a failure, and it is not what this
//     test measures.
//
// Plus a sixth that belongs to the generator rather than the server:
// requests the load generator could not dispatch because all its workers
// were busy. Those never reached the network, and calling them server drops
// would be a lie about which side ran out of capacity.
func TestLoadCapturesNothingDropped(t *testing.T) {
	rate := float64(envInt("HOOKLENS_LOAD_RATE", 1000))
	seconds := envInt("HOOKLENS_LOAD_SECONDS", 60)
	workers := envInt("HOOKLENS_LOAD_WORKERS", 256)

	st, ep := loadStore(t)

	// Rate limits above the offered load. The limiter is unit 31's subject
	// and works; leaving it at 50/s here would mean measuring the limiter,
	// getting 95% 429s, and learning nothing about the capture pipeline.
	// Note the assertion below still checks that it did not fire -- an
	// unexpected 429 would otherwise silently shrink the denominator.
	cfg := config.Config{
		Env:         "dev",
		Addr:        ":0",
		BaseDomain:  "localhost",
		RateCapture: rate * 10,
		RateCreate:  rate * 10,
	}
	s := New(context.Background(), cfg, quiet(), st)

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	before, err := st.CountRequests(t.Context(), ep.ID)
	if err != nil {
		t.Fatalf("count before: %v", err)
	}

	url := srv.URL + "/e/" + ep.Slug + "/load"
	body := `{"event":"load.test","data":{"n":1,"s":"a short but not empty payload"}}`

	res := loadgen.Run(context.Background(), loadgen.Config{
		Rate:     rate,
		Duration: time.Duration(seconds) * time.Second,
		Workers:  workers,
		Client:   loadgen.DefaultClient(workers),
		Build: func() (*http.Request, error) {
			req, err := http.NewRequest("POST", url, strings.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return req, nil
		},
	})

	after, err := st.CountRequests(context.Background(), ep.ID)
	if err != nil {
		t.Fatalf("count after: %v", err)
	}
	stored := after - before

	// Report before asserting. A failing load test whose output is one
	// assertion message tells you it failed and nothing about why; the run
	// took a minute and every number it produced is worth reading.
	t.Log(report(res, stored, scrape(t, s)))

	if res.Transport != 0 {
		t.Errorf("%d transport failures: connections were refused or reset before HTTP", res.Transport)
	}
	if res.ServerErrors() != 0 {
		t.Errorf("%d 5xx responses, want 0", res.ServerErrors())
	}
	if res.RateLimited() != 0 {
		t.Errorf("%d requests were rate limited, but the limit was set to %.0f/s -- "+
			"the limiter fired for a reason this test does not understand",
			res.RateLimited(), cfg.RateCapture)
	}

	// The assertion the phase is actually about.
	if stored != res.Accepted() {
		t.Errorf("DROPPED CAPTURES: the server returned %d 2xx but Postgres holds %d new rows (%d lost)",
			res.Accepted(), stored, res.Accepted()-stored)
	}

	// Saturation is a finding about this machine, not about the server, so
	// it is reported loudly and tolerated in small amounts: a few percent is
	// the generator's own scheduling noise, and more than that means the
	// offered rate was never actually offered and the run proves nothing.
	if pct := 100 * float64(res.Saturated) / float64(res.Scheduled); pct > 5 {
		t.Errorf("the generator could not dispatch %.1f%% of the schedule (%d of %d): "+
			"offered load never reached %.0f/s, so this run does not demonstrate the target",
			pct, res.Saturated, res.Scheduled, rate)
	}
}

// loadStore opens the database and creates a throwaway inbox.
//
// Separate from storeServer because that helper builds a Server with default
// config, and this test needs the rate limits raised before New runs -- the
// limiters are constructed there.
func loadStore(t *testing.T) (*store.Store, *store.NewEndpoint) {
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

	ep, err := st.CreateEndpoint(t.Context(), "load test")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	return st, ep
}

// report prints the three independent accounts of what happened -- the
// generator's, the server's own metrics, and the database's -- side by side.
//
// Three sources on purpose. The generator knows what it sent and cannot know
// what was stored; the metrics know what the handler believes and are wrong
// in exactly the cases that matter; the row count is the only one that is
// ground truth, and it is also the slowest and least detailed. Disagreement
// between them is the finding.
func report(res *loadgen.Result, stored int64, metrics string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\n--- offered ---\n")
	fmt.Fprintf(&b, "scheduled     %d\n", res.Scheduled)
	fmt.Fprintf(&b, "sent          %d\n", res.Sent)
	fmt.Fprintf(&b, "saturated     %d  (generator could not dispatch)\n", res.Saturated)
	fmt.Fprintf(&b, "--- outcomes ---\n")
	for _, code := range sortedCodes(res.ByStatus) {
		fmt.Fprintf(&b, "status %d    %d\n", code, res.ByStatus[code])
	}
	fmt.Fprintf(&b, "transport     %d\n", res.Transport)
	fmt.Fprintf(&b, "--- latency (from scheduled send time) ---\n")
	fmt.Fprintf(&b, "p50 %v   p95 %v   p99 %v   max %v\n",
		res.Percentile(0.50), res.Percentile(0.95), res.Percentile(0.99), res.Percentile(1))
	fmt.Fprintf(&b, "--- durability ---\n")
	fmt.Fprintf(&b, "accepted (2xx) %d\n", res.Accepted())
	fmt.Fprintf(&b, "rows in postgres %d\n", stored)
	for _, line := range strings.Split(metrics, "\n") {
		if strings.HasPrefix(line, "hooklens_captures_total") ||
			strings.HasPrefix(line, "hooklens_capture_bytes_total") {
			fmt.Fprintf(&b, "metric %s\n", line)
		}
	}
	return b.String()
}

func sortedCodes(m map[int]int64) []int {
	codes := make([]int, 0, len(m))
	for c := range m {
		codes = append(codes, c)
	}
	for i := 1; i < len(codes); i++ {
		for j := i; j > 0 && codes[j] < codes[j-1]; j-- {
			codes[j], codes[j-1] = codes[j-1], codes[j]
		}
	}
	return codes
}
