package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func scrape(t *testing.T, s *Server) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
	// The version parameter matters: without it some scrapers fall back to
	// guessing the format.
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "version=0.0.4") {
		t.Errorf("Content-Type = %q, want the text format with a version", ct)
	}
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestMetricsEndpointExposesHTTPMetrics(t *testing.T) {
	s, _ := storeServer(t, 0)

	// Two requests with different outcomes.
	for _, path := range []string{"/healthz", "/api/nope"} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
	}

	out := scrape(t, s)

	if !strings.Contains(out, "# TYPE hooklens_http_requests_total counter") {
		t.Error("no HTTP counter in the scrape")
	}
	if !strings.Contains(out, `hooklens_http_requests_total{method="GET",status="2xx"}`) {
		t.Errorf("no 2xx series:\n%s", out)
	}
	if !strings.Contains(out, `hooklens_http_requests_total{method="GET",status="4xx"}`) {
		t.Errorf("no 4xx series:\n%s", out)
	}
	// The histogram must be there and be cumulative-shaped.
	if !strings.Contains(out, "hooklens_http_duration_seconds_bucket") {
		t.Error("no duration histogram")
	}
	if !strings.Contains(out, "hooklens_http_duration_seconds_count") {
		t.Error("no histogram count")
	}
}

// TestMetricsHasNoPathLabel is the cardinality guard, and it is the
// mistake that kills a Prometheus: one series per URL a stranger invents.
func TestMetricsHasNoPathLabel(t *testing.T) {
	s, _ := storeServer(t, 0)

	// Twenty distinct paths, all 404.
	for i := range 20 {
		req := httptest.NewRequest("GET", "/api/made-up-"+string(rune('a'+i)), nil)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
	}

	out := scrape(t, s)

	if strings.Contains(out, "made-up-") {
		t.Fatalf("a URL path reached a metric label; that is unbounded cardinality:\n%s", out)
	}
	// Twenty distinct paths must collapse to ONE series.
	if n := strings.Count(out, `hooklens_http_requests_total{method="GET",status="4xx"}`); n != 1 {
		t.Errorf("got %d series for 20 distinct 404 paths, want 1", n)
	}
}

// TestMetricsScrapeIsStable: a diff of two scrapes is a normal debugging
// move, so the ordering must not change between them.
func TestMetricsScrapeIsStable(t *testing.T) {
	s, _ := storeServer(t, 0)
	for range 3 {
		req := httptest.NewRequest("GET", "/healthz", nil)
		s.ServeHTTP(httptest.NewRecorder(), req)
	}

	first := scrape(t, s)
	second := scrape(t, s)

	firstLines := strings.Split(first, "\n")
	secondLines := strings.Split(second, "\n")
	if len(firstLines) != len(secondLines) {
		t.Fatalf("scrape line count changed: %d then %d", len(firstLines), len(secondLines))
	}
	for i := range firstLines {
		// Values legitimately differ -- the scrape itself is a request --
		// but the metric NAMES and their order must not.
		a, _, _ := strings.Cut(firstLines[i], " ")
		b, _, _ := strings.Cut(secondLines[i], " ")
		if a != b {
			t.Errorf("line %d changed shape: %q then %q", i, firstLines[i], secondLines[i])
		}
	}
}

// TestTwoServersDoNotShareMetrics. Held on the Server rather than in a
// package global specifically so this is true -- a global would make two
// tests in one process accumulate into the same counters.
func TestTwoServersDoNotShareMetrics(t *testing.T) {
	a, _ := storeServer(t, 0)
	b, _ := storeServer(t, 0)

	for range 5 {
		a.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	}

	got := b.Metrics().HTTPRequests.Value(map[string]string{"method": "GET", "status": "2xx"})
	if got != 0 {
		t.Errorf("a second server saw %d requests it never served", got)
	}
}
