package server

import (
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/config"
	"github.com/DinithiPramodya/hooklens/internal/webui"
)

// testServer builds a Server with no database.
//
// Every route exercised in this file -- /healthz, the SPA, the API 404 -- is
// reachable without touching the store, which is what keeps these tests fast
// and independent of Docker. Any test that needs real data belongs in
// internal/store.
func testServer(t *testing.T) *Server {
	t.Helper()
	return New(
		t.Context(),
		config.Config{Env: "dev", Addr: ":0", BaseDomain: "localhost"},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
}

func do(t *testing.T, s *Server, method, target string, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec.Result()
}

// TestNoCORSHeaders pins the single-origin decision.
//
// There is deliberately no CORS anywhere in this codebase: one binary serves
// the page and the API, so every request is same-origin and there is nothing to
// negotiate. If someone later adds a CORS middleware -- usually while chasing a
// devtools error that actually means "you split the origins" -- this test fails
// and forces the question to be answered rather than absorbed.
//
// See docs/learn/12-cors.md.
func TestNoCORSHeaders(t *testing.T) {
	s := testServer(t)

	targets := []struct{ method, path string }{
		{"GET", "/healthz"},
		{"GET", "/"},
		{"GET", "/api/anything"},
		{"POST", "/api/endpoints"},
		// The preflight a browser sends before any authenticated call, because
		// an Authorization header is not a "simple request".
		{"OPTIONS", "/api/endpoints"},
	}

	for _, tt := range targets {
		resp := do(t, s, tt.method, tt.path, map[string]string{
			"Origin":                         "http://evil.example",
			"Access-Control-Request-Method":  "POST",
			"Access-Control-Request-Headers": "authorization,content-type",
		})
		resp.Body.Close()

		for name := range resp.Header {
			if strings.HasPrefix(strings.ToLower(name), "access-control-") {
				t.Errorf("%s %s emitted %s: %q -- this codebase is single-origin and must send no CORS headers",
					tt.method, tt.path, name, resp.Header.Get(name))
			}
		}
	}
}

// TestPreflightIsNotApproved is the consequence of the above, stated as
// behaviour rather than as an absence.
//
// A browser sending this preflight receives no Access-Control-Allow-Origin, so
// it never sends the real request. That is correct: nothing should be calling
// this API cross-origin, and if something is, the fix is to stop splitting the
// origins rather than to start approving them.
func TestPreflightIsNotApproved(t *testing.T) {
	s := testServer(t)

	resp := do(t, s, "OPTIONS", "/api/endpoints", map[string]string{
		"Origin":                        "http://localhost:5173",
		"Access-Control-Request-Method": "POST",
	})
	defer resp.Body.Close()

	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("preflight was approved with Allow-Origin %q", got)
	}
	// 404 rather than 405, because OPTIONS /api/endpoints matches only the
	// /api/ catch-all -- there is no OPTIONS handler to be method-mismatched
	// against. Either is a refusal as far as the browser is concerned.
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("preflight status = %d, want 404", resp.StatusCode)
	}
}

// TestAPINotFoundIsJSON covers the unit 11 sharp edge: an unmatched API path
// must not fall through to the SPA and return an HTML document, or the client's
// fetch tries to JSON.parse a web page and reports a syntax error about "<".
func TestAPINotFoundIsJSON(t *testing.T) {
	s := testServer(t)

	resp := do(t, s, "GET", "/api/definitely-not-a-route", nil)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON -- the SPA fallback has swallowed the API", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "<!doctype") || strings.Contains(string(body), "<html") {
		t.Errorf("API 404 returned an HTML document:\n%s", body)
	}
}

// TestSPAFallback covers the other half: a path the server has never heard of
// IS a valid client-side route and must return the shell, with 200.
func TestSPAFallback(t *testing.T) {
	if !webui.Available() {
		t.Skip("no frontend build embedded -- run `npm run build` in web/")
	}
	s := testServer(t)

	for _, path := range []string{"/", "/requests/abc123", "/some/deep/route"} {
		resp := do(t, s, "GET", path, nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d, want 200 (a client route is not a 404)", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), `<div id="root">`) {
			t.Errorf("%s did not return the SPA shell", path)
		}
	}
}

// TestHealthzBeatsTheCatchAll pins the route-specificity assumption: Go's
// ServeMux picks the most specific pattern, so /healthz never reaches the SPA.
func TestHealthzBeatsTheCatchAll(t *testing.T) {
	s := testServer(t)

	resp := do(t, s, "GET", "/healthz", nil)
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q -- /healthz fell through to the SPA", ct)
	}
}

// TestCacheHeaders pins the two directions from unit 11. Reversing them is a
// classic, and both failure modes are invisible in development.
func TestCacheHeaders(t *testing.T) {
	if !webui.Available() {
		t.Skip("no frontend build embedded -- run `npm run build` in web/")
	}
	s := testServer(t)

	resp := do(t, s, "GET", "/", nil)
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Errorf("index.html Cache-Control = %q, want no-cache -- it names the hashed bundles", got)
	}

	// Discover whatever hashed asset this build produced rather than hardcoding
	// a filename whose hash changes on every frontend edit.
	entries, err := fs.ReadDir(webui.FS(), "assets")
	if err != nil || len(entries) == 0 {
		t.Skipf("no hashed assets in this build (%v)", err)
	}

	resp = do(t, s, "GET", "/assets/"+entries[0].Name(), nil)
	resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("hashed asset Cache-Control = %q, want immutable", got)
	}
}
