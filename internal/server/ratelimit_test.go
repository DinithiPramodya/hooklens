package server

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/ratelimit"
)

// TestClientIP is the hard part of per-IP anything, and it fails in two
// directions: trusting RemoteAddr behind a proxy turns per-IP limiting into
// GLOBAL limiting, and trusting X-Forwarded-For lets anyone forge their
// identity.
func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		xff        string
		trust      bool
		want       string
	}{
		{"plain", "203.0.113.5:54321", "", false, "203.0.113.5"},
		{"ipv6", "[2001:db8::1]:443", "", false, "2001:db8::1"},
		// Untrusted: the header is ignored entirely, so it cannot be forged.
		{"forged xff, untrusted", "203.0.113.5:1", "1.2.3.4", false, "203.0.113.5"},
		// Trusted: the FIRST entry is the original client; the rest are hops.
		{"trusted xff", "10.0.0.1:1", "1.2.3.4", true, "1.2.3.4"},
		{"trusted xff chain", "10.0.0.1:1", "1.2.3.4, 10.0.0.9, 10.0.0.1", true, "1.2.3.4"},
		{"trusted xff spaces", "10.0.0.1:1", "  1.2.3.4  ", true, "1.2.3.4"},
		// Junk in the header must not become a limiter key, or the header
		// itself is an unbounded-memory vector.
		{"trusted junk xff", "203.0.113.5:1", "not-an-ip", true, "203.0.113.5"},
		{"trusted empty xff", "203.0.113.5:1", "", true, "203.0.113.5"},
		// Mapped form normalised, so ::ffff:1.2.3.4 and 1.2.3.4 are one key.
		{"mapped", "10.0.0.1:1", "::ffff:1.2.3.4", true, "1.2.3.4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/", nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			if got := clientIP(r, tc.trust); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCreateEndpointRateLimited: that endpoint is unauthenticated by
// design, so this limiter is the only thing between a loop and a full
// database.
func TestCreateEndpointRateLimited(t *testing.T) {
	s, _ := storeServer(t, 0)

	var lastCode int
	var retryAfter string
	// The burst is 5; the eleventh request cannot possibly be inside it.
	for range 11 {
		req := httptest.NewRequest("POST", "/api/endpoints", strings.NewReader(`{"name":"x"}`))
		req.RemoteAddr = "203.0.113.99:1234"
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		lastCode = rec.Code
		retryAfter = rec.Header().Get("Retry-After")
	}

	if lastCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d after 11 rapid creations, want 429", lastCode)
	}
	// A 429 with no Retry-After leaves a well-behaved client guessing, and
	// guessing means retrying too soon.
	if retryAfter == "" {
		t.Fatal("no Retry-After on a 429")
	}
	n, err := strconv.Atoi(retryAfter)
	if err != nil || n < 1 {
		t.Errorf("Retry-After = %q, want a positive integer number of seconds", retryAfter)
	}
}

// TestCreateEndpointLimitIsPerIP: one noisy source must not lock everyone
// else out. That is the whole reason the limiter is keyed.
func TestCreateEndpointLimitIsPerIP(t *testing.T) {
	s, _ := storeServer(t, 0)

	exhaust := func(ip string) int {
		var code int
		for range 11 {
			req := httptest.NewRequest("POST", "/api/endpoints", strings.NewReader(`{}`))
			req.RemoteAddr = ip + ":1"
			rec := httptest.NewRecorder()
			s.ServeHTTP(rec, req)
			code = rec.Code
		}
		return code
	}

	if got := exhaust("198.51.100.1"); got != http.StatusTooManyRequests {
		t.Fatalf("setup: first IP was not limited, got %d", got)
	}

	// A different address starts with a full bucket.
	req := httptest.NewRequest("POST", "/api/endpoints", strings.NewReader(`{}`))
	req.RemoteAddr = "198.51.100.2:1"
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code == http.StatusTooManyRequests {
		t.Error("a second IP was limited because the first was exhausted")
	}
}

// TestCaptureRateLimitedPerInbox, and critically: the 429 must arrive with
// NOTHING stored, so a provider's retry cannot produce a duplicate.
func TestCaptureRateLimitedPerInbox(t *testing.T) {
	s, ep := storeServer(t, 0)
	// A tiny limit for this test; the production default is 50/sec.
	s.ingest.SetLimiter(newTestLimiter(t))

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	var limited bool
	for range 5 {
		resp := post(t, srv, ep.Slug, "/hook", `{"a":1}`)
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			if resp.Header.Get("Retry-After") == "" {
				t.Error("no Retry-After on a capture 429")
			}
			// Nothing must have been stored for a refused capture.
			if resp.Header.Get("X-Hooklens-Id") != "" {
				t.Error("a rate-limited capture was still assigned an id")
			}
		}
		resp.Body.Close()
	}

	if !limited {
		t.Fatal("no capture was rate limited with a limit of 2")
	}

	// And the stored count matches what was accepted, not what was sent --
	// a 429 above the durability line means nothing was written.
	page, err := s.store.ListRequests(t.Context(), ep.ID, 50, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Requests) >= 5 {
		t.Errorf("stored %d captures, but some should have been refused", len(page.Requests))
	}
}

// newTestLimiter allows 2 immediately and then effectively nothing.
func newTestLimiter(t *testing.T) *ratelimit.Limiter {
	t.Helper()
	return ratelimit.New(0.001, 2)
}
