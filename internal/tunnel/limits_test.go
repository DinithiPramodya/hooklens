package tunnel

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/capture"
)

// TestFrameLimitExceedsEncodedBodyLimit is the arithmetic that was wrong.
//
// A body travels base64-encoded, four bytes per three. If the frame limit
// equals the body limit, a body at the limit produces a frame over it -- and
// a WebSocket read-limit violation CLOSES THE CONNECTION rather than skipping
// the message, so one large response would drop the tunnel and every
// unrelated request in flight on it.
func TestFrameLimitExceedsEncodedBodyLimit(t *testing.T) {
	encoded := base64.StdEncoding.EncodedLen(maxBodyBytes)
	if maxFrameBytes <= encoded {
		t.Fatalf("maxFrameBytes (%d) must exceed base64(maxBodyBytes) = %d; "+
			"a body at the limit would close the connection", maxFrameBytes, encoded)
	}

	// And with room left for the JSON envelope and the headers, which are
	// bounded separately at the HTTP server (MaxHeaderBytes).
	const headerSlack = 64 << 10
	if maxFrameBytes < encoded+headerSlack {
		t.Errorf("maxFrameBytes (%d) leaves only %d bytes over the encoded body "+
			"for headers and JSON; headers alone may be %d",
			maxFrameBytes, maxFrameBytes-encoded, headerSlack)
	}
}

// TestBodyLimitMatchesCaptureLimit pins the fact the comment in server.go
// asserts. internal/tunnel deliberately does not import internal/capture, so
// the two constants agreeing is checked here rather than enforced by the
// compiler -- which means it has to be checked.
func TestBodyLimitMatchesCaptureLimit(t *testing.T) {
	if int64(maxBodyBytes) != capture.DefaultMaxBody {
		t.Errorf("maxBodyBytes = %d but capture.DefaultMaxBody = %d; "+
			"a capture that ingest accepts would be refused by the relay",
			maxBodyBytes, capture.DefaultMaxBody)
	}
}

// TestCallLocalRefusesOversizedResponse: refused, not truncated. A truncated
// response relayed to the provider is a corrupt payload that looks complete,
// and the developer would debug a handler that did nothing wrong.
func TestCallLocalRefusesOversizedResponse(t *testing.T) {
	huge := strings.Repeat("x", maxBodyBytes+1024)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(huge))
	}))
	defer app.Close()

	c := newTestClient(t, app.URL)
	resp := c.callLocal(t.Context(), Request{Method: "GET", Path: "/big"})

	if resp.Error == "" {
		t.Fatal("an oversized response was accepted; it would have been silently truncated")
	}
	if resp.Status != 0 {
		t.Errorf("status = %d, want 0 -- a status would look like a real answer", resp.Status)
	}
	if !strings.Contains(resp.Error, "larger than") {
		t.Errorf("Error = %q, which does not say the response was too big", resp.Error)
	}
	if resp.BodyB64 != "" {
		t.Error("a partial body was relayed alongside the error")
	}
}

// TestCallLocalAcceptsResponseAtTheLimit: the boundary itself must work, or
// the limit is really limit-minus-one and nobody knows.
func TestCallLocalAcceptsResponseAtTheLimit(t *testing.T) {
	exact := strings.Repeat("y", maxBodyBytes)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(exact))
	}))
	defer app.Close()

	c := newTestClient(t, app.URL)
	resp := c.callLocal(t.Context(), Request{Method: "GET", Path: "/exact"})

	if resp.Error != "" {
		t.Fatalf("a response exactly at the limit was refused: %s", resp.Error)
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.BodyB64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != maxBodyBytes {
		t.Errorf("relayed %d bytes, want %d", len(decoded), maxBodyBytes)
	}

	// And the frame it produces must fit the read limit, which is the whole
	// point of the two constants differing.
	frame, err := Encode(TypeResponse, resp)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(frame) > maxFrameBytes {
		t.Errorf("a response at the body limit produced a %d byte frame, over the %d limit",
			len(frame), maxFrameBytes)
	}
}
