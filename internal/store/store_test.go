package store

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
	"github.com/DinithiPramodya/hooklens/internal/secret"
)

// testStore connects to the database named by DATABASE_URL, or skips.
//
// Deliberately NOT testcontainers, which is Phase 5 material and a heavier
// dependency than this unit needs. The trade-off is honest: these tests skip
// silently when no database is reachable, so a green `go test ./...` on a
// machine without Docker does not mean the store layer passed. CI always has
// Postgres, so CI always runs them.
func testStore(t *testing.T) *Store {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	st, err := Open(ctx, dsn)
	if err != nil {
		t.Skipf("no database reachable (%v) -- run `docker compose up -d`", err)
	}
	t.Cleanup(st.Close)
	return st
}

func TestEndpointLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "my inbox")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.ID == "" || ep.Name != "my inbox" {
		t.Fatalf("unexpected endpoint %+v", ep)
	}
	if len(ep.Slug) != 26 {
		t.Errorf("generated slug %q is %d chars, want 26 (128 bits of base32)", ep.Slug, len(ep.Slug))
	}
	if ep.Token == "" {
		t.Fatal("CreateEndpoint returned no token")
	}
	if ep.LastSeenAt != nil {
		t.Errorf("LastSeenAt = %v, want nil on a fresh inbox", ep.LastSeenAt)
	}

	got, err := st.EndpointBySlug(ctx, ep.Slug)
	if err != nil || got.ID != ep.ID {
		t.Fatalf("EndpointBySlug: %+v, %v", got, err)
	}

	if _, err := st.EndpointBySlug(ctx, "definitely-not-a-real-inbox"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing slug: got %v, want ErrNotFound", err)
	}
}

// TestTokenNotStoredInPlaintext is the point of the whole unit. Anyone who can
// read this table must hold nothing usable.
func TestTokenNotStoredInPlaintext(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	// Ask the database directly, the way a leaked dump or a curious DBA would.
	var hash []byte
	err = st.pool.QueryRow(ctx,
		`select owner_token_hash from endpoints where id = $1`, ep.ID).Scan(&hash)
	if err != nil {
		t.Fatalf("read hash: %v", err)
	}

	if string(hash) == ep.Token {
		t.Fatal("the plaintext token is in the database")
	}
	if strings.Contains(string(hash), ep.Token) {
		t.Fatal("the stored value contains the plaintext token")
	}
	if len(hash) != 32 {
		t.Errorf("stored hash is %d bytes, want 32 (SHA-256)", len(hash))
	}
	if !secret.Equal(hash, secret.Hash(ep.Token)) {
		t.Error("stored hash does not match the hash of the issued token")
	}
}

func TestAuthenticateEndpoint(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	other, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	got, err := st.AuthenticateEndpoint(ctx, ep.Slug, ep.Token)
	if err != nil {
		t.Fatalf("correct token rejected: %v", err)
	}
	if got.ID != ep.ID {
		t.Errorf("authenticated the wrong inbox")
	}

	cases := []struct {
		name, slug, token string
	}{
		{"wrong token", ep.Slug, other.Token},
		{"empty token", ep.Slug, ""},
		{"garbage token", ep.Slug, "not-a-token"},
		{"another inbox's slug with this token", other.Slug, ep.Token},
		// The important one: a slug that does not exist must fail the SAME way
		// as a bad token, or the error becomes an oracle confirming which
		// inboxes are real.
		{"nonexistent slug", "aaaaaaaaaaaaaaaaaaaaaaaaaa", ep.Token},
	}
	for _, c := range cases {
		if _, err := st.AuthenticateEndpoint(ctx, c.slug, c.token); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: got %v, want ErrUnauthorized", c.name, err)
		}
	}
}

func TestAuthenticateRequest(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	mine, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	theirs, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	id, err := st.InsertRequest(ctx, mine.ID, &capture.Request{
		Method: "POST", Path: "/secret", DeclaredSize: -1, ReceivedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	got, err := st.AuthenticateRequest(ctx, id, mine.Token)
	if err != nil {
		t.Fatalf("owner rejected: %v", err)
	}
	if got.Path != "/secret" {
		t.Errorf("Path = %q", got.Path)
	}

	// Another inbox's owner must not be able to read this request even though
	// they hold a perfectly valid token -- for a different inbox.
	if _, err := st.AuthenticateRequest(ctx, id, theirs.Token); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("cross-inbox read: got %v, want ErrUnauthorized", err)
	}
}

// TestInsertRequestRoundTrip is the test that matters: whatever bytes went in
// must come back identical. If this fails, Phase 4's signature verification
// cannot work.
func TestInsertRequestRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	ip := netip.MustParseAddr("203.0.113.9")
	declared := int64(999)
	in := &capture.Request{
		Method: "POST",
		Path:   "/webhook",
		Query:  "a=1&b=two",
		Headers: []capture.Header{
			{Name: "Content-Type", Value: "application/json"},
			{Name: "X-Custom", Value: "first"},  // duplicate name...
			{Name: "X-Custom", Value: "second"}, // ...must survive
			{Name: "A-First", Value: "z"},       // and array order must survive
		},
		// Ugly on purpose: duplicate key, ragged whitespace, and a NUL byte
		// that a text column would reject outright.
		Body:         []byte("{\"b\":1,   \"a\":2,\n\"b\":3}\x00\xff"),
		Truncated:    true,
		DeclaredSize: declared,
		SourceIP:     ip,
		ReceivedAt:   time.Now().UTC().Truncate(time.Microsecond),
	}

	id, err := st.InsertRequest(ctx, ep.ID, in)
	if err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	out, err := st.GetRequest(ctx, id)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}

	if string(out.Body) != string(in.Body) {
		t.Errorf("body round trip failed\n got %q\nwant %q", out.Body, in.Body)
	}
	if out.BodySize != len(in.Body) {
		t.Errorf("BodySize = %d, want %d", out.BodySize, len(in.Body))
	}
	if !out.BodyTruncated {
		t.Error("BodyTruncated lost")
	}
	if out.DeclaredSize == nil || *out.DeclaredSize != declared {
		t.Errorf("DeclaredSize = %v, want %d", out.DeclaredSize, declared)
	}
	if out.SourceIP == nil || *out.SourceIP != ip {
		t.Errorf("SourceIP = %v, want %v", out.SourceIP, ip)
	}
	if !out.ReceivedAt.Equal(in.ReceivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", out.ReceivedAt, in.ReceivedAt)
	}

	// The jsonb-array-not-object decision, verified: order preserved and the
	// duplicate name still present. Stored as an object this would come back
	// with three entries collapsed to two and sorted.
	if len(out.Headers) != len(in.Headers) {
		t.Fatalf("got %d headers, want %d: %+v", len(out.Headers), len(in.Headers), out.Headers)
	}
	for i := range in.Headers {
		if out.Headers[i] != in.Headers[i] {
			t.Errorf("header %d = %+v, want %+v", i, out.Headers[i], in.Headers[i])
		}
	}
}

// TestInsertRequestNulls covers the "unknown" cases that must be SQL NULL
// rather than a sentinel: chunked encoding has no Content-Length, and an
// unparseable RemoteAddr has no IP.
func TestInsertRequestNulls(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	id, err := st.InsertRequest(ctx, ep.ID, &capture.Request{
		Method:       "GET",
		Path:         "/",
		DeclaredSize: -1,           // chunked: no Content-Length
		SourceIP:     netip.Addr{}, // unparseable
		ReceivedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	out, err := st.GetRequest(ctx, id)
	if err != nil {
		t.Fatalf("GetRequest: %v", err)
	}
	if out.DeclaredSize != nil {
		t.Errorf("DeclaredSize = %v, want NULL for chunked", *out.DeclaredSize)
	}
	if out.SourceIP != nil {
		t.Errorf("SourceIP = %v, want NULL", *out.SourceIP)
	}
}

// TestListRequestsOrdering pins what the (endpoint_id, received_at desc) index
// exists to serve: newest first, scoped to one inbox.
func TestListRequestsOrdering(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	mine, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	// A second inbox: slugs are generated now, so no collision to engineer.
	theirs, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	for i := range 5 {
		_, err := st.InsertRequest(ctx, mine.ID, &capture.Request{
			Method: "POST", Path: fmt.Sprintf("/%d", i),
			DeclaredSize: -1,
			ReceivedAt:   base.Add(time.Duration(i) * time.Second),
		})
		if err != nil {
			t.Fatalf("InsertRequest: %v", err)
		}
	}
	// One in a different inbox, newer than all of the above. It must not appear.
	if _, err := st.InsertRequest(ctx, theirs.ID, &capture.Request{
		Method: "POST", Path: "/other", DeclaredSize: -1,
		ReceivedAt: base.Add(time.Hour),
	}); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	got, err := st.ListRequests(ctx, mine.ID, 10)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d requests, want 5 (other inboxes must not leak in)", len(got))
	}
	for i, want := range []string{"/4", "/3", "/2", "/1", "/0"} {
		if got[i].Path != want {
			t.Errorf("position %d = %s, want %s (newest first)", i, got[i].Path, want)
		}
	}

	// And the limit is respected.
	got, err = st.ListRequests(ctx, mine.ID, 2)
	if err != nil || len(got) != 2 {
		t.Errorf("limit 2: got %d rows, %v", len(got), err)
	}
}

// TestIndexIsUsed asks Postgres what it actually plans to do. A test asserting
// rows come back in the right order would pass just as happily with a
// sequential scan and a sort -- which is the thing the index exists to avoid.
func TestIndexIsUsed(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	const q = `
		explain (format text)
		select id from requests
		where endpoint_id = $1
		order by received_at desc
		limit 50`

	rows, err := st.pool.Query(ctx, q, ep.ID)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain: %v", err)
	}

	t.Logf("plan:\n%s", plan.String())

	// On an empty or tiny table Postgres may legitimately prefer a sequential
	// scan -- with no rows, reading the whole table IS cheapest, and forcing an
	// index would be wrong. So this asserts only the thing that is always true:
	// the planner must not be sorting, because the index supplies the order.
	if strings.Contains(plan.String(), "Sort") {
		t.Errorf("plan contains a Sort -- the index is not supplying the ordering:\n%s", plan.String())
	}
}
