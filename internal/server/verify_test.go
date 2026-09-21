package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/signature"
)

// postVerify sends a capture id and a secret, and decodes the panel.
func postVerify(t *testing.T, s *Server, id, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/requests/"+id+"/verify", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
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

// captureSigned stores a request carrying a real Stripe signature.
func captureSigned(t *testing.T, s *Server, slug, secret, payload string) string {
	t.Helper()
	unix := strconv.FormatInt(time.Now().Unix(), 10)
	sig := signature.Compute(signature.SHA256, signature.Hex,
		[]byte(secret), []byte(unix+"."+payload))

	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(t.Context(), "POST",
		srv.URL+"/e/"+slug+"/stripe", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Stripe-Signature", "t="+unix+",v1="+sig)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return capturedID(t, resp)
}

func TestVerifyEndpointValid(t *testing.T) {
	s, ep := storeServer(t, 0)
	const secret = "whsec_test_abc"
	id := captureSigned(t, s, ep.Slug, secret, `{"id":"evt_1","amount":2500}`)

	code, out := postVerify(t, s, id, ep.Token, `{"secret":"`+secret+`"}`)

	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if out["valid"] != true {
		t.Errorf("valid = %v, want true (%v)", out["valid"], out["problem"])
	}
	if out["provider"] != "stripe" {
		t.Errorf("provider = %v, want stripe (detection failed)", out["provider"])
	}
	// The field the whole endpoint exists for.
	signed, _ := out["signed_string"].(string)
	if !strings.Contains(signed, `"amount":2500`) {
		t.Errorf("signed_string = %q, want it to contain the body", signed)
	}
	if !strings.Contains(signed, ".") {
		t.Errorf("signed_string = %q, want the `t.body` form", signed)
	}
}

// TestVerifyEndpointWrongSecret: PLAN.md's acceptance criterion --
// "corrupting the secret reads INVALID and shows the string it compared".
func TestVerifyEndpointWrongSecret(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := captureSigned(t, s, ep.Slug, "whsec_right", `{"id":"evt_2"}`)

	_, out := postVerify(t, s, id, ep.Token, `{"secret":"whsec_wrong"}`)

	if out["valid"] != false {
		t.Fatal("a wrong secret verified")
	}
	if out["signed_string"] == "" || out["signed_string"] == nil {
		t.Error("no canonical string on a failure; that is the whole criterion")
	}
	if out["expected"] == out["provided"] {
		t.Error("expected and provided are identical on a failed check")
	}
	if out["hint"] == nil {
		t.Error("no hint")
	}
}

func TestVerifyEndpointRequiresSecret(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := captureSigned(t, s, ep.Slug, "whsec_x", `{}`)

	code, out := postVerify(t, s, id, ep.Token, `{}`)
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", code)
	}
	if out["hint"] == nil {
		t.Error("the error should say what to send")
	}
}

func TestVerifyEndpointRequiresAuth(t *testing.T) {
	s, ep := storeServer(t, 0)
	id := captureSigned(t, s, ep.Slug, "whsec_x", `{}`)

	req := httptest.NewRequest("POST", "/api/requests/"+id+"/verify",
		strings.NewReader(`{"secret":"whsec_x"}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 -- a capture's bytes are as sensitive as the list", rec.Code)
	}
}

// TestVerifyEndpointUnsignedCapture: most captures carry no signature at
// all, and that is a normal answer rather than an error.
func TestVerifyEndpointUnsignedCapture(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	resp := post(t, srv, ep.Slug, "/plain", `{"a":1}`)
	defer resp.Body.Close()
	id := capturedID(t, resp)

	code, out := postVerify(t, s, id, ep.Token, `{"secret":"anything"}`)
	if code != http.StatusOK {
		t.Errorf("status = %d, want 200 -- no signature is not an error", code)
	}
	if out["detected"] != false {
		t.Errorf("detected = %v, want false", out["detected"])
	}
	if out["hint"] == nil {
		t.Error("the answer should list what IS supported")
	}
}

// TestVerifyEndpointDoesNotEchoTheSecret. The secret is used and discarded;
// it must not come back in the response, where it would land in a browser
// cache or a screenshot.
func TestVerifyEndpointDoesNotEchoTheSecret(t *testing.T) {
	s, ep := storeServer(t, 0)
	const secret = "whsec_do_not_leak_me"
	id := captureSigned(t, s, ep.Slug, secret, `{"id":"evt_3"}`)

	req := httptest.NewRequest("POST", "/api/requests/"+id+"/verify",
		strings.NewReader(`{"secret":"`+secret+`"}`))
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), secret) {
		t.Error("the response echoed the secret back")
	}
}
