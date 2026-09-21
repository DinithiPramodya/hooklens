package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/DinithiPramodya/hooklens/internal/replay"
	"github.com/DinithiPramodya/hooklens/internal/tunnel"
)

func postReplay(t *testing.T, s *Server, id, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/requests/"+id+"/replay", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	return resp.StatusCode, out
}

// storeOne captures a request and returns its id.
func storeOne(t *testing.T, s *Server, slug, path, body string) string {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	resp := post(t, srv, slug, path, body)
	defer resp.Body.Close()
	return capturedID(t, resp)
}

// TestReplayToTunnel is the core feature: catch an event once, then iterate
// against it without re-triggering it.
func TestReplayToTunnel(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	var seen atomic.Int64
	var sawReplayHeader atomic.Bool
	var lastBody atomic.Value
	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(req tunnel.Request) tunnel.Response {
		seen.Add(1)
		for _, h := range req.Headers {
			if strings.EqualFold(h.Name, replay.ReplayHeader) {
				sawReplayHeader.Store(true)
			}
		}
		decoded, _ := base64.StdEncoding.DecodeString(req.BodyB64)
		lastBody.Store(string(decoded))
		return tunnel.Response{Status: 202}
	})
	waitTunnel(t, s, ep.ID)

	resp := post(t, srv, ep.Slug, "/hooks/stripe", `{"id":"evt_1"}`)
	id := capturedID(t, resp)
	resp.Body.Close()
	if seen.Load() != 1 {
		t.Fatalf("setup: the app saw %d requests, want 1", seen.Load())
	}
	sawReplayHeader.Store(false) // the original was not a replay

	code, out := postReplay(t, s, id, ep.Token, `{}`)

	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out["replayed"] != true {
		t.Errorf("replayed = %v: %v", out["replayed"], out["error"])
	}
	if out["status"] != float64(202) {
		t.Errorf("status = %v, want the app's 202", out["status"])
	}
	if seen.Load() != 2 {
		t.Errorf("the app saw %d requests, want 2", seen.Load())
	}
	// Marked, or a developer's own logs cannot tell a replay from a
	// duplicate delivery -- the exact confusion this tool removes.
	if !sawReplayHeader.Load() {
		t.Error("the replayed request carried no X-Hooklens-Replay header")
	}
	if got := lastBody.Load(); got != `{"id":"evt_1"}` {
		t.Errorf("replayed body = %v, want the original", got)
	}
	if out["edited"] != false {
		t.Errorf("edited = %v, want false", out["edited"])
	}
}

// TestReplayEdited: the feature for events you cannot trigger at all.
func TestReplayEdited(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	var lastBody atomic.Value
	fakeTunnel(t, srv.URL, ep.Slug, ep.Token, func(req tunnel.Request) tunnel.Response {
		decoded, _ := base64.StdEncoding.DecodeString(req.BodyB64)
		lastBody.Store(string(decoded))
		return tunnel.Response{Status: 200}
	})
	waitTunnel(t, s, ep.ID)

	resp := post(t, srv, ep.Slug, "/hook", `{"amount":100}`)
	id := capturedID(t, resp)
	resp.Body.Close()

	edited := `{"amount":999999999999}`
	_, out := postReplay(t, s, id, ep.Token,
		`{"body_b64":"`+base64.StdEncoding.EncodeToString([]byte(edited))+`"}`)

	if got := lastBody.Load(); got != edited {
		t.Errorf("app saw %v, want the edited body", got)
	}
	if out["edited"] != true {
		t.Error("edited = false for an edited replay")
	}
	// An edited body can never match the original MAC. Saying so stops a
	// developer concluding their verification code broke.
	if out["signature_note"] == nil {
		t.Error("no note that the signature will no longer match")
	}
}

func TestReplayToTunnelWithNoTunnel(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := storeOne(t, s, ep.Slug, "/hook", `{}`)

	code, out := postReplay(t, s, id, ep.Token, `{"target":"tunnel"}`)

	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 -- no tunnel is an answer, not an error", code)
	}
	if out["replayed"] != false {
		t.Error("replayed = true with no tunnel")
	}
	if out["hint"] == nil || !strings.Contains(out["hint"].(string), "forward") {
		t.Errorf("hint = %v, want the command to run", out["hint"])
	}
}

// TestReplayToURLRejectsPrivateTargets is the SSRF guard at the HTTP layer.
func TestReplayToURLRejectsPrivateTargets(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := storeOne(t, s, ep.Slug, "/hook", `{}`)

	for _, target := range []string{
		"http://127.0.0.1:8080/x",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/",
		"http://[::1]/",
		"file:///etc/passwd",
	} {
		t.Run(target, func(t *testing.T) {
			code, out := postReplay(t, s, id, ep.Token, `{"target":"`+target+`"}`)
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for %s", code, target)
			}
			if out["error"] == nil {
				t.Error("no error explaining the refusal")
			}
		})
	}
}

// TestReplayToURLRejectsRebinding: a HOSTNAME that resolves to loopback
// passes the pre-check and must be caught at the dial. That ordering is the
// whole reason the guard lives in Dialer.Control.
func TestReplayToURLRejectsRebinding(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := storeOne(t, s, ep.Slug, "/hook", `{}`)

	var reached atomic.Bool
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Store(true)
	}))
	defer victim.Close()

	target := strings.Replace(victim.URL, "127.0.0.1", "localhost", 1)
	code, out := postReplay(t, s, id, ep.Token, `{"target":"`+target+`"}`)

	if reached.Load() {
		t.Fatal("a loopback server was reached via its hostname")
	}
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if out["error"] == nil || !strings.Contains(out["error"].(string), "loopback") {
		t.Errorf("error = %v, want it to name loopback", out["error"])
	}
}

func TestReplayRequiresAuth(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := storeOne(t, s, ep.Slug, "/hook", `{}`)

	req := httptest.NewRequest("POST", "/api/requests/"+id+"/replay", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestReplayRejectsBadBase64(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := storeOne(t, s, ep.Slug, "/hook", `{}`)

	code, _ := postReplay(t, s, id, ep.Token, `{"body_b64":"!!!"}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
}
