package ingest

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

// TestForwardErrorCodeCoversEveryHubError is a guard against a specific
// production failure rather than a check of a switch statement.
//
// The permitted values are fixed by a CHECK constraint in migration 00006. An
// error that fell through to its own text would violate that constraint on the
// UPDATE, and surface as a database fault logged far from the actual cause --
// a new error type added in internal/tunnel.
func TestForwardErrorCodeCoversEveryHubError(t *testing.T) {
	// The codes the CHECK constraint allows.
	allowed := map[string]bool{
		"no_tunnel": true, "timeout": true, "disconnected": true,
		"unreachable": true, "protocol": true,
	}

	cases := []struct {
		err  error
		want string
	}{
		{tunnel.ErrNoTunnel, "no_tunnel"},
		{tunnel.ErrTimeout, "timeout"},
		{tunnel.ErrDisconnected, "disconnected"},
		{errors.New("something nobody anticipated"), "protocol"},
		// Wrapped, because the hub wraps write failures.
		{fmt.Errorf("write request frame: %w", tunnel.ErrDisconnected), "disconnected"},
	}

	for _, tc := range cases {
		got := forwardErrorCode(tc.err)
		if got != tc.want {
			t.Errorf("forwardErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
		if !allowed[got] {
			t.Errorf("forwardErrorCode(%v) = %q, which the 00006 CHECK constraint rejects", tc.err, got)
		}
	}
}

func TestIsHopByHop(t *testing.T) {
	for _, name := range []string{
		"Connection", "connection", "CONNECTION",
		"Keep-Alive", "Transfer-Encoding", "Upgrade", "Content-Length",
	} {
		if !isHopByHop(name) {
			t.Errorf("isHopByHop(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"Content-Type", "X-Request-Id", "Set-Cookie", "Location"} {
		if isHopByHop(name) {
			t.Errorf("isHopByHop(%q) = true, want false", name)
		}
	}
}

// TestWriteForwardedRelaysVerbatim: the developer's response is the product.
func TestWriteForwardedRelaysVerbatim(t *testing.T) {
	rec := httptest.NewRecorder()
	writeForwarded(rec, &tunnel.Response{
		Status: 422,
		Headers: []tunnel.Header{
			{Name: "Content-Type", Value: "application/problem+json"},
			{Name: "X-App", Value: "mine"},
			// Duplicates must survive, for the reason they survive everywhere
			// else in this codebase.
			{Name: "Set-Cookie", Value: "a=1"},
			{Name: "Set-Cookie", Value: "b=2"},
			// Hop-by-hop: describes the CLI's connection to the local app,
			// not ours to the provider.
			{Name: "Connection", Value: "keep-alive"},
			{Name: "Content-Length", Value: "999"},
		},
		BodyB64: "eyJlcnJvciI6ImJhZCJ9", // {"error":"bad"}
	})

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != 422 {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 {
		t.Errorf("Set-Cookie = %v, want both values", got)
	}
	if resp.Header.Get("Connection") != "" {
		t.Error("hop-by-hop Connection header was relayed")
	}
	if got := resp.Header.Get("Content-Length"); got == "999" {
		t.Error("the local app's Content-Length was relayed; it describes a different response")
	}
	if got := rec.Body.String(); got != `{"error":"bad"}` {
		t.Errorf("body = %q", got)
	}
	if got := resp.Header.Get("X-Hooklens-Forward"); got != "delivered" {
		t.Errorf("X-Hooklens-Forward = %q, want delivered", got)
	}
}

// TestWriteForwardedRejectsNonsenseStatus: a buggy CLI must not panic us.
// net/http panics on WriteHeader with an out-of-range code.
func TestWriteForwardedRejectsNonsenseStatus(t *testing.T) {
	for _, status := range []int{0, 99, 600, -1} {
		rec := httptest.NewRecorder()
		writeForwarded(rec, &tunnel.Response{Status: status})
		if got := rec.Result().StatusCode; got != 502 {
			t.Errorf("status %d relayed as %d, want 502", status, got)
		}
	}
}

// TestWriteForwardedSurvivesBadBase64: likewise, a malformed body must relay
// the status rather than failing. The status is the part a provider acts on.
func TestWriteForwardedSurvivesBadBase64(t *testing.T) {
	rec := httptest.NewRecorder()
	writeForwarded(rec, &tunnel.Response{Status: 200, BodyB64: "!!!not base64!!!"})
	if got := rec.Result().StatusCode; got != 200 {
		t.Errorf("status = %d, want 200", got)
	}
	if got := rec.Body.Len(); got != 0 {
		t.Errorf("body = %d bytes, want empty", got)
	}
}
