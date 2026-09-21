package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func getDiff(t *testing.T, s *Server, id, with, token, extra string) (int, map[string]any) {
	t.Helper()
	url := "/api/requests/" + id + "/diff?with=" + with + extra
	req := httptest.NewRequest("GET", url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
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

func changePaths(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		m, _ := it.(map[string]any)
		out = append(out, m["path"].(string))
	}
	return out
}

// TestDiffEndpointHighlightsOnlyTheAmount is PLAN.md's acceptance
// criterion, end to end through the API.
func TestDiffEndpointHighlightsOnlyTheAmount(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	base := `{"id":"evt","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","amount":%d,"currency":"usd"}}}`
	r1 := post(t, srv, ep.Slug, "/hooks/stripe", strings.Replace(base, "%d", "2500", 1))
	id1 := capturedID(t, r1)
	r1.Body.Close()
	r2 := post(t, srv, ep.Slug, "/hooks/stripe", strings.Replace(base, "%d", "9900", 1))
	id2 := capturedID(t, r2)
	r2.Body.Close()

	code, out := getDiff(t, s, id1, id2, ep.Token, "")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}

	got := changePaths(out["body"])
	if len(got) != 1 || got[0] != "data.object.amount" {
		t.Errorf("body changes = %v, want exactly data.object.amount", got)
	}
	// And nothing spurious elsewhere. Both requests went to the same path
	// with the same method, so the request line is identical.
	if n := len(changePaths(out["request"])); n != 0 {
		t.Errorf("request changes = %v, want none", changePaths(out["request"]))
	}
	if out["identical"] == true {
		t.Error("identical = true for two different payloads")
	}
}

// TestDiffEndpointHidesVolatileHeadersByDefault. Date and signatures differ
// on every request; leaving them in drowns the two fields somebody wants.
func TestDiffEndpointHidesVolatileHeadersByDefault(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	send := func(sig string) string {
		req, err := http.NewRequestWithContext(t.Context(), "POST",
			srv.URL+"/e/"+ep.Slug+"/hook", strings.NewReader(`{"a":1}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Stripe-Signature", sig)
		req.Header.Set("X-Stable", "same")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return capturedID(t, resp)
	}

	id1 := send("t=1,v1=aaaa")
	id2 := send("t=2,v1=bbbb")

	_, out := getDiff(t, s, id1, id2, ep.Token, "")
	for _, p := range changePaths(out["headers"]) {
		if p == "header:stripe-signature" {
			t.Error("the signature header was shown by default; it differs every time")
		}
	}
	if out["volatile_hidden"] != true {
		t.Error("volatile_hidden = false; the response must say the filter is on")
	}

	// And opting in brings it back, because hiding data without a way to
	// see it is a diff that lies.
	_, out2 := getDiff(t, s, id1, id2, ep.Token, "&volatile=1")
	var found bool
	for _, p := range changePaths(out2["headers"]) {
		if p == "header:stripe-signature" {
			found = true
		}
	}
	if !found {
		t.Error("volatile=1 did not reveal the signature header")
	}
	if out2["volatile_hidden"] != false {
		t.Error("volatile_hidden should be false when the filter is off")
	}
}

// TestDiffEndpointReportsRequestLineChanges: the same payload sent to a
// different path is a routing bug, not a payload bug, and it is neither a
// header nor a body difference.
func TestDiffEndpointReportsRequestLineChanges(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	r1 := post(t, srv, ep.Slug, "/hooks/a", `{"same":1}`)
	id1 := capturedID(t, r1)
	r1.Body.Close()
	r2 := post(t, srv, ep.Slug, "/hooks/b", `{"same":1}`)
	id2 := capturedID(t, r2)
	r2.Body.Close()

	_, out := getDiff(t, s, id1, id2, ep.Token, "")

	got := changePaths(out["request"])
	if len(got) != 1 || got[0] != "path" {
		t.Errorf("request changes = %v, want exactly [path]", got)
	}
	if n := len(changePaths(out["body"])); n != 0 {
		t.Errorf("body changes = %v, want none", changePaths(out["body"]))
	}
}

func TestDiffEndpointIdentical(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	r1 := post(t, srv, ep.Slug, "/hook", `{"a":1}`)
	id1 := capturedID(t, r1)
	r1.Body.Close()
	r2 := post(t, srv, ep.Slug, "/hook", `{"a":1}`)
	id2 := capturedID(t, r2)
	r2.Body.Close()

	_, out := getDiff(t, s, id1, id2, ep.Token, "")
	if out["identical"] != true {
		t.Errorf("identical = %v; body=%v headers=%v request=%v",
			out["identical"], changePaths(out["body"]),
			changePaths(out["headers"]), changePaths(out["request"]))
	}
}

func TestDiffEndpointRequiresWith(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	r := post(t, srv, ep.Slug, "/hook", `{}`)
	id := capturedID(t, r)
	r.Body.Close()

	req := httptest.NewRequest("GET", "/api/requests/"+id+"/diff", nil)
	req.Header.Set("Authorization", "Bearer "+ep.Token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestDiffEndpointAuthenticatesBothIDs is the one that matters for access
// control. Checking only the first id would turn this endpoint into a way
// to read ANY capture by diffing it against one you own.
func TestDiffEndpointAuthenticatesBothIDs(t *testing.T) {
	s, mine := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)

	// A second inbox, belonging to someone else.
	theirs, err := s.store.CreateEndpoint(t.Context(), "someone else")
	if err != nil {
		t.Fatal(err)
	}

	r1 := post(t, srv, mine.Slug, "/hook", `{"mine":1}`)
	mineID := capturedID(t, r1)
	r1.Body.Close()
	r2 := post(t, srv, theirs.Slug, "/hook", `{"secret":"theirs"}`)
	theirID := capturedID(t, r2)
	r2.Body.Close()

	code, _ := getDiff(t, s, mineID, theirID, mine.Token, "")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 -- my token must not read their capture", code)
	}

	// And the other way round.
	code2, _ := getDiff(t, s, theirID, mineID, mine.Token, "")
	if code2 != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for their capture as the left side", code2)
	}
}

func TestDiffEndpointRequiresAuth(t *testing.T) {
	s, ep := storeServer(t, 0)
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	r := post(t, srv, ep.Slug, "/hook", `{}`)
	id := capturedID(t, r)
	r.Body.Close()

	code, _ := getDiff(t, s, id, id, "", "")
	if code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", code)
	}
}
