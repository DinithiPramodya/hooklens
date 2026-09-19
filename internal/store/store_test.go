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

// uniqueSlug keeps parallel and repeated runs from colliding on the unique
// index, without needing to truncate tables between tests.
func uniqueSlug(t *testing.T) string {
	t.Helper()
	s := strings.ToLower(t.Name())
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	s = fmt.Sprintf("%s-%d", s, time.Now().UnixNano()%1e9)
	if len(s) > 32 {
		s = s[len(s)-32:]
	}
	return strings.Trim(s, "-")
}

func TestEndpointLifecycle(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()
	slug := uniqueSlug(t)

	ep, err := st.CreateEndpoint(ctx, slug, "my inbox")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if ep.ID == "" || ep.Slug != slug || ep.Name != "my inbox" {
		t.Fatalf("unexpected endpoint %+v", ep)
	}
	if ep.LastSeenAt != nil {
		t.Errorf("LastSeenAt = %v, want nil on a fresh inbox", ep.LastSeenAt)
	}

	// The unique index must surface as our own error, not a raw pgx one.
	if _, err := st.CreateEndpoint(ctx, slug, ""); !errors.Is(err, ErrSlugTaken) {
		t.Errorf("duplicate slug: got %v, want ErrSlugTaken", err)
	}

	got, err := st.EndpointBySlug(ctx, slug)
	if err != nil || got.ID != ep.ID {
		t.Fatalf("EndpointBySlug: %+v, %v", got, err)
	}

	if _, err := st.EndpointBySlug(ctx, "definitely-not-a-real-inbox"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing slug: got %v, want ErrNotFound", err)
	}
}

// TestInsertRequestRoundTrip is the test that matters: whatever bytes went in
// must come back identical. If this fails, Phase 4's signature verification
// cannot work.
func TestInsertRequestRoundTrip(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, uniqueSlug(t), "")
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

	ep, err := st.CreateEndpoint(ctx, uniqueSlug(t), "")
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

	mine, err := st.CreateEndpoint(ctx, uniqueSlug(t), "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	// A second call, not uniqueSlug(t)+"x" -- that is 33 chars and the CHECK
	// constraint from migration 00001 rejects it. (It did, on the first run.)
	theirs, err := st.CreateEndpoint(ctx, uniqueSlug(t), "")
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

	ep, err := st.CreateEndpoint(ctx, uniqueSlug(t), "")
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
