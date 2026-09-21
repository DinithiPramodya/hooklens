package server

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/DinithiPramodya/hooklens/internal/store"
	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

// These tests run the whole path: an HTTP request to the capture endpoint, a
// row in Postgres, a frame down a real WebSocket, a fake CLI's answer, and the
// response the provider actually receives. The unit tests in internal/tunnel
// and internal/ingest cover the pieces; this covers the wiring between them,
// which is where the pieces being individually correct stops being enough.

// fakeTunnel connects a CLI to a running server and answers request frames.
func fakeTunnel(t *testing.T, base, slug, token string, respond func(tunnel.Request) tunnel.Response) {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/api/tunnel", nil)
	if err != nil {
		t.Fatalf("dial tunnel: %v", err)
	}
	t.Cleanup(func() { c.CloseNow() })

	hello, err := tunnel.Encode(tunnel.TypeHello, tunnel.Hello{
		Slug: slug, Token: token, Version: tunnel.ProtocolVersion,
	})
	if err != nil {
		t.Fatalf("encode hello: %v", err)
	}
	if err := c.Write(ctx, websocket.MessageText, hello); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read hello_ok: %v", err)
	}
	env, err := tunnel.DecodeEnvelope(data)
	if err != nil || env.Type != tunnel.TypeHelloOK {
		t.Fatalf("handshake failed: %v %v", env.Type, err)
	}

	go func() {
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			env, err := tunnel.DecodeEnvelope(data)
			if err != nil || env.Type != tunnel.TypeRequest {
				continue
			}
			var req tunnel.Request
			if err := tunnel.DecodePayload(env, &req); err != nil {
				continue
			}
			go func() {
				resp := respond(req)
				resp.ReqID = req.ReqID
				b, err := tunnel.Encode(tunnel.TypeResponse, resp)
				if err != nil {
					return
				}
				wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer wcancel()
				_ = c.Write(wctx, websocket.MessageText, b)
			}()
		}
	}()
}

// post sends a webhook to the inbox by path, the way a provider would.
func post(t *testing.T, srv *httptest.Server, slug, path, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST",
		srv.URL+"/e/"+slug+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	return resp
}

func waitTunnel(t *testing.T, s *Server, endpointID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.tunnel.Hub().Connected(endpointID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("tunnel never registered")
}

// TestForwardEndToEnd is the headline: the developer's response reaches the
// provider unchanged.
func TestForwardEndToEnd(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	var got tunnel.Request
	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(req tunnel.Request) tunnel.Response {
		got = req
		return tunnel.Response{
			Status:  418,
			Headers: []tunnel.Header{{Name: "X-Brewed-By", Value: "local-app"}},
			BodyB64: base64.StdEncoding.EncodeToString([]byte(`{"ok":false}`)),
		}
	})
	waitTunnel(t, s, ep.ID)

	resp := post(t, srv, ep.Slug, "/hooks/stripe?x=1", `{"hello":"world"}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	// What the provider sees is the local app's answer, not ours.
	if resp.StatusCode != 418 {
		t.Errorf("status = %d, want the local app's 418", resp.StatusCode)
	}
	if h := resp.Header.Get("X-Brewed-By"); h != "local-app" {
		t.Errorf("X-Brewed-By = %q, want the local app's header", h)
	}
	if string(body) != `{"ok":false}` {
		t.Errorf("body = %q, want the local app's body", body)
	}
	if h := resp.Header.Get("X-Hooklens-Forward"); h != "delivered" {
		t.Errorf("X-Hooklens-Forward = %q, want delivered", h)
	}

	// And what the local app saw is the provider's request, intact.
	if got.Method != "POST" {
		t.Errorf("method = %q", got.Method)
	}
	if got.Path != "/hooks/stripe" {
		t.Errorf("path = %q, want the path AFTER the inbox prefix is stripped", got.Path)
	}
	if got.Query != "x=1" {
		t.Errorf("query = %q", got.Query)
	}
	decoded, err := base64.StdEncoding.DecodeString(got.BodyB64)
	if err != nil || string(decoded) != `{"hello":"world"}` {
		t.Errorf("body = %q (err %v)", decoded, err)
	}
}

// TestForwardWithNoTunnel is failure mode 1: never lose a message. The
// provider must get a 2xx or it will retry an event we already hold.
func TestForwardWithNoTunnel(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	resp := post(t, srv, ep.Slug, "/anything", `{"a":1}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 -- a non-2xx makes the provider retry a stored event",
			resp.StatusCode)
	}
	if h := resp.Header.Get("X-Hooklens-Forward"); h != "no_tunnel" {
		t.Errorf("X-Hooklens-Forward = %q, want no_tunnel", h)
	}

	// The capture is stored, and the reason is recorded on it.
	id := capturedID(t, resp)
	req := fetchRequest(t, s, id)
	if req.ForwardError == nil || *req.ForwardError != "no_tunnel" {
		t.Errorf("forward_error = %v, want no_tunnel", req.ForwardError)
	}
	if req.ForwardStatus != nil {
		t.Errorf("forward_status = %v, want nil -- the app was never reached", *req.ForwardStatus)
	}
}

// TestForwardUnreachableLocalApp is failure mode 2. The CLI is connected but
// cannot reach localhost:3000. The provider must NOT get a 502: that would
// make it retry and eventually disable the endpoint, because someone's local
// server is not running.
func TestForwardUnreachableLocalApp(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(tunnel.Request) tunnel.Response {
		return tunnel.Response{Error: "dial tcp 127.0.0.1:3000: connection refused"}
	})
	waitTunnel(t, s, ep.ID)

	resp := post(t, srv, ep.Slug, "/hook", `{}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 -- a local app being down is not the provider's problem",
			resp.StatusCode)
	}
	if h := resp.Header.Get("X-Hooklens-Forward"); h != "unreachable" {
		t.Errorf("X-Hooklens-Forward = %q, want unreachable", h)
	}

	req := fetchRequest(t, s, capturedID(t, resp))
	if req.ForwardError == nil || *req.ForwardError != "unreachable" {
		t.Errorf("forward_error = %v, want unreachable", req.ForwardError)
	}
}

// TestForwardRelaysApplicationErrors is the other half of failure mode 2, and
// the distinction that makes it useful. A 500 FROM the local app means the app
// was reached and answered badly -- the developer is testing exactly that, so
// it is relayed unchanged rather than swallowed.
func TestForwardRelaysApplicationErrors(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(tunnel.Request) tunnel.Response {
		return tunnel.Response{
			Status:  500,
			BodyB64: base64.StdEncoding.EncodeToString([]byte("panic: nil map")),
		}
	})
	waitTunnel(t, s, ep.ID)

	resp := post(t, srv, ep.Slug, "/hook", `{}`)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 500 {
		t.Errorf("status = %d, want the local app's 500 relayed unchanged", resp.StatusCode)
	}
	if string(body) != "panic: nil map" {
		t.Errorf("body = %q, want the local app's error text", body)
	}
}

// TestForwardTimeoutStillCapturesAndRecords is failure mode 3 at the ingest
// level: a hung local app must not hang the provider forever, and must not
// produce a non-2xx.
func TestForwardTimeoutStillCapturesAndRecords(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	// Shrink the deadline for this test; 30s is the production value.
	s.ingest.SetForwardTimeout(250 * time.Millisecond)

	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(tunnel.Request) tunnel.Response {
		<-blocked // never answers in time
		return tunnel.Response{Status: 200}
	})
	waitTunnel(t, s, ep.ID)

	start := time.Now()
	resp := post(t, srv, ep.Slug, "/slow", `{}`)
	defer resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if elapsed > 10*time.Second {
		t.Errorf("provider waited %v; the forward deadline did not fire", elapsed)
	}
	if h := resp.Header.Get("X-Hooklens-Forward"); h != "timeout" {
		t.Errorf("X-Hooklens-Forward = %q, want timeout", h)
	}

	req := fetchRequest(t, s, capturedID(t, resp))
	if req.ForwardError == nil || *req.ForwardError != "timeout" {
		t.Errorf("forward_error = %v, want timeout", req.ForwardError)
	}
}

// capturedID reads the capture id from the response HEADER, not the body.
//
// On a successful forward the body belongs to the developer's app, so there
// is no hooklens JSON to parse -- which is exactly why X-Hooklens-Id exists.
// Reading the body worked for every test written before forwarding landed
// and broke silently the moment one asserted on a delivered capture.
func capturedID(t *testing.T, resp *http.Response) string {
	t.Helper()
	if id := resp.Header.Get("X-Hooklens-Id"); id != "" {
		return id
	}
	t.Fatal("no X-Hooklens-Id on the capture response")
	return ""
}

func fetchRequest(t *testing.T, s *Server, id string) *store.StoredRequest {
	t.Helper()
	req, err := s.store.GetRequest(t.Context(), id)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	return req
}

// TestTruncatedCaptureIsNotForwarded is failure mode 8 at the tunnel
// boundary. The capture is stored with what fits, but handing a local app
// bytes that are corrupt yet look complete is worse than handing it nothing:
// the resulting parse or signature failure blames the sender, not the
// truncation.
func TestTruncatedCaptureIsNotForwarded(t *testing.T) {
	s, ep := storeServer(t, 0)
	// A tiny cap, so a modest body is over it.
	s.ingest.SetMaxBody(64)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	var reached atomic.Bool
	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(tunnel.Request) tunnel.Response {
		reached.Store(true)
		return tunnel.Response{Status: 200}
	})
	waitTunnel(t, s, ep.ID)

	resp := post(t, srv, ep.Slug, "/big", strings.Repeat("x", 4096))
	defer resp.Body.Close()

	// The provider still gets a success: the capture IS stored.
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 -- the capture was stored", resp.StatusCode)
	}
	if h := resp.Header.Get("X-Hooklens-Forward"); h != "too_large" {
		t.Errorf("X-Hooklens-Forward = %q, want too_large", h)
	}
	if reached.Load() {
		t.Error("a truncated body was forwarded to the local app")
	}

	req := fetchRequest(t, s, capturedID(t, resp))
	if !req.BodyTruncated {
		t.Error("the capture is not marked truncated")
	}
	if req.ForwardError == nil || *req.ForwardError != "too_large" {
		t.Errorf("forward_error = %v, want too_large", req.ForwardError)
	}
	// And what was kept is inspectable, which is why refusing to forward
	// costs nothing for debugging.
	if req.BodySize != 64 {
		t.Errorf("stored %d bytes, want the 64 that fit", req.BodySize)
	}
}
