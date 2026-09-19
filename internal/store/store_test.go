package store

import (
	"context"
	"encoding/base64"
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

	page, err := st.ListRequests(ctx, mine.ID, 10, nil)
	if err != nil {
		t.Fatalf("ListRequests: %v", err)
	}
	if len(page.Requests) != 5 {
		t.Fatalf("got %d requests, want 5 (other inboxes must not leak in)", len(page.Requests))
	}
	for i, want := range []string{"/4", "/3", "/2", "/1", "/0"} {
		if page.Requests[i].Path != want {
			t.Errorf("position %d = %s, want %s (newest first)", i, page.Requests[i].Path, want)
		}
	}
	if page.NextCursor != "" {
		t.Errorf("NextCursor = %q on a complete result, want empty", page.NextCursor)
	}

	// And the limit is respected, with a cursor offered because more remain.
	page, err = st.ListRequests(ctx, mine.ID, 2, nil)
	if err != nil || len(page.Requests) != 2 {
		t.Fatalf("limit 2: got %d rows, %v", len(page.Requests), err)
	}
	if page.NextCursor == "" {
		t.Error("NextCursor empty although 3 more rows exist")
	}
}

func TestCursorRoundTrip(t *testing.T) {
	want := Cursor{
		// Nanosecond precision matters: truncating it in the encoding would
		// make a cursor land between rows and skip one.
		ReceivedAt: time.Date(2026, 9, 19, 12, 34, 56, 123456789, time.UTC),
		ID:         "01a0b975-189f-7f9d-bf66-c7d94fab95b4",
	}

	got, err := ParseCursor(want.String())
	if err != nil {
		t.Fatalf("ParseCursor: %v", err)
	}
	if !got.ReceivedAt.Equal(want.ReceivedAt) {
		t.Errorf("ReceivedAt = %v, want %v", got.ReceivedAt, want.ReceivedAt)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}

	if strings.ContainsAny(want.String(), "+/=") {
		t.Errorf("cursor %q is not URL-safe", want.String())
	}

	for _, bad := range []string{"not base64!", "", "AAAA", base64Raw("no-pipe-here"), base64Raw("|missing-time")} {
		if _, err := ParseCursor(bad); !errors.Is(err, ErrBadCursor) {
			t.Errorf("ParseCursor(%q) = %v, want ErrBadCursor", bad, err)
		}
	}
}

func base64Raw(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

// TestCursorPaginationWalksEverything checks the boring but essential property:
// paging all the way through returns every row exactly once, in order.
func TestCursorPaginationWalksEverything(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	const total = 23
	base := time.Now().UTC().Truncate(time.Microsecond)
	for i := range total {
		if _, err := st.InsertRequest(ctx, ep.ID, &capture.Request{
			Method: "POST", Path: fmt.Sprintf("/%02d", i), DeclaredSize: -1,
			ReceivedAt: base.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatalf("InsertRequest: %v", err)
		}
	}

	var (
		seen   []string
		cursor *Cursor
		pages  int
	)
	for {
		page, err := st.ListRequests(ctx, ep.ID, 5, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, r := range page.Requests {
			seen = append(seen, r.Path)
		}
		if page.NextCursor == "" {
			break
		}
		c, err := ParseCursor(page.NextCursor)
		if err != nil {
			t.Fatalf("own cursor did not parse: %v", err)
		}
		cursor = &c

		if pages > 20 {
			t.Fatal("pagination did not terminate -- the cursor is not advancing")
		}
	}

	if len(seen) != total {
		t.Fatalf("saw %d rows over %d pages, want %d", len(seen), pages, total)
	}
	if pages != 5 { // 23 rows at 5 per page
		t.Errorf("took %d pages, want 5", pages)
	}
	for i, path := range seen {
		want := fmt.Sprintf("/%02d", total-1-i)
		if path != want {
			t.Fatalf("position %d = %s, want %s", i, path, want)
		}
	}
	// No duplicates.
	uniq := make(map[string]bool, len(seen))
	for _, p := range seen {
		if uniq[p] {
			t.Errorf("row %s returned twice", p)
		}
		uniq[p] = true
	}
}

// TestCursorSurvivesInserts is the whole argument for cursor pagination.
//
// It reads page 1, inserts new rows above it -- exactly what a live inbox does
// -- and then reads page 2 both ways. The cursor is unaffected; OFFSET returns
// rows the caller has already seen.
func TestCursorSurvivesInserts(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	base := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(label string, offset time.Duration) {
		t.Helper()
		if _, err := st.InsertRequest(ctx, ep.ID, &capture.Request{
			Method: "POST", Path: "/" + label, DeclaredSize: -1,
			ReceivedAt: base.Add(offset),
		}); err != nil {
			t.Fatalf("InsertRequest: %v", err)
		}
	}

	// Ten original rows, old-00 .. old-09, newest last.
	for i := range 10 {
		insert(fmt.Sprintf("old-%02d", i), time.Duration(i)*time.Millisecond)
	}

	// Page 1: the five newest, old-09 .. old-05.
	p1, err := st.ListRequests(ctx, ep.ID, 5, nil)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if got := p1.Requests[0].Path; got != "/old-09" {
		t.Fatalf("page 1 starts at %s, want /old-09", got)
	}
	page1 := pathsOf(p1.Requests)

	// Three new webhooks arrive while the user is reading page 1. In a
	// newest-first list they land at positions 0, 1, 2 and push everything down.
	for i := range 3 {
		insert(fmt.Sprintf("new-%02d", i), time.Duration(100+i)*time.Millisecond)
	}

	// Page 2 by cursor.
	c, err := ParseCursor(p1.NextCursor)
	if err != nil {
		t.Fatalf("ParseCursor: %v", err)
	}
	p2, err := st.ListRequests(ctx, ep.ID, 5, &c)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	cursorPage2 := pathsOf(p2.Requests)

	// Page 2 by OFFSET, the naive equivalent, run directly against the database.
	offsetPage2, err := listByOffset(ctx, st, ep.ID, 5, 5)
	if err != nil {
		t.Fatalf("offset query: %v", err)
	}

	t.Logf("page 1          : %v", page1)
	t.Logf("page 2 (cursor) : %v", cursorPage2)
	t.Logf("page 2 (offset) : %v", offsetPage2)

	// The cursor continues exactly where page 1 stopped.
	want := []string{"/old-04", "/old-03", "/old-02", "/old-01", "/old-00"}
	if !slicesEqual(cursorPage2, want) {
		t.Errorf("cursor page 2 = %v, want %v", cursorPage2, want)
	}
	if overlap := intersect(page1, cursorPage2); len(overlap) != 0 {
		t.Errorf("cursor repeated rows from page 1: %v", overlap)
	}

	// OFFSET does not, and this is the point of the test.
	if overlap := intersect(page1, offsetPage2); len(overlap) == 0 {
		t.Errorf("expected OFFSET to repeat rows from page 1 after 3 inserts, but it did not "+
			"-- page1=%v offsetPage2=%v", page1, offsetPage2)
	} else {
		t.Logf("OFFSET repeated %d rows already shown on page 1: %v", len(overlap), overlap)
	}
}

// listByOffset is the naive pagination this unit exists to avoid. It lives only
// in the test, as the thing being compared against.
func listByOffset(ctx context.Context, st *Store, endpointID string, limit, offset int) ([]string, error) {
	rows, err := st.pool.Query(ctx, `
		select path from requests
		where endpoint_id = $1
		order by received_at desc, id desc
		limit $2 offset $3`, endpointID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func pathsOf(rs []StoredRequest) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Path
	}
	return out
}

func intersect(a, b []string) []string {
	in := make(map[string]bool, len(a))
	for _, s := range a {
		in[s] = true
	}
	var out []string
	for _, s := range b {
		if in[s] {
			out = append(out, s)
		}
	}
	return out
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIndexIsUsed asks Postgres what it actually plans to do. A test asserting
// rows come back in the right order would pass just as happily with a
// sequential scan and a sort -- which is the thing the index exists to avoid.
// TestIndexIsUsed asks Postgres what it plans to do. A test asserting that rows
// come back in the right order would pass just as happily with a sequential
// scan and a sort -- which is the thing the index exists to avoid.
//
// The first version of this test ran against an almost-empty table and asserted
// there was no Sort node. It passed by luck and then failed, because the planner
// is COST-BASED: on a tiny table, reading every row genuinely is cheaper than
// descending an index, and choosing the seq scan is the optimiser being right.
// An index assertion on an empty table tests nothing at all.
//
// So this seeds enough rows for the index to actually be the cheaper plan, and
// runs ANALYZE so the planner has statistics rather than its default guesses.
func TestIndexIsUsed(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	// One statement rather than 5,000 round trips.
	_, err = st.pool.Exec(ctx, `
		insert into requests (endpoint_id, method, path, declared_size, received_at)
		select $1, 'POST', '/p' || g, null, now() - (g || ' seconds')::interval
		from generate_series(1, 5000) g`, ep.ID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Without fresh statistics the planner works from defaults and may still
	// choose wrong -- not because the index is bad, but because it does not
	// know how many rows are there.
	if _, err := st.pool.Exec(ctx, `analyze requests`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	const q = `
		explain (format text)
		select id from requests
		where endpoint_id = $1
		order by received_at desc, id desc
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

	got := plan.String()
	t.Logf("plan:\n%s", got)

	// Two assertions, and the second is the one that matters. An Index Scan
	// alone would still be a win if it were followed by a Sort; the absence of
	// the Sort is what proves the index supplies the ORDER BY, which is what
	// makes deep pages cheap.
	if !strings.Contains(got, "requests_endpoint_cursor_idx") {
		t.Errorf("plan does not use the cursor index:\n%s", got)
	}
	if strings.Contains(got, "Sort") {
		t.Errorf("plan contains a Sort -- the index is not supplying the ordering:\n%s", got)
	}
}

// TestCursorPlanIsNotOffsetPlan shows the cost difference the unit is about.
// Deep OFFSET reads and discards everything above it; the cursor seeks.
func TestCursorPlanIsNotOffsetPlan(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	_, err = st.pool.Exec(ctx, `
		insert into requests (endpoint_id, method, path, declared_size, received_at)
		select $1, 'POST', '/p' || g, null, now() - (g || ' seconds')::interval
		from generate_series(1, 5000) g`, ep.ID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `analyze requests`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// "rows" here is rows actually read, which is the number this unit is about.
	read := func(q string, args ...any) int {
		t.Helper()
		var total int
		rows, err := st.pool.Query(ctx, "explain (analyze, format text) "+q, args...)
		if err != nil {
			t.Fatalf("explain analyze: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan: %v", err)
			}
			plan.WriteString(line + "\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("explain analyze: %v", err)
		}
		t.Logf("%s\n%s", q, plan.String())
		// Pull "actual ... rows=N" from the innermost scan node.
		for _, line := range strings.Split(plan.String(), "\n") {
			// "Scan", not "Index Scan": selecting only indexed columns gives an
			// Index ONLY Scan, which does not contain that substring. Missing it
			// is how this parser silently reported 0 on the first run.
			if strings.Contains(line, "Scan") {
				if i := strings.LastIndex(line, "rows="); i >= 0 {
					_, _ = fmt.Sscanf(line[i:], "rows=%d", &total)
				}
			}
		}
		return total
	}

	deepOffset := read(`select id from requests where endpoint_id = $1
		order by received_at desc, id desc limit 50 offset 4000`, ep.ID)

	// The equivalent position by cursor: the 4000th row back.
	var c Cursor
	err = st.pool.QueryRow(ctx, `
		select received_at, id::text from requests
		where endpoint_id = $1
		order by received_at desc, id desc
		offset 3999 limit 1`, ep.ID).Scan(&c.ReceivedAt, &c.ID)
	if err != nil {
		t.Fatalf("find cursor position: %v", err)
	}

	byCursor := read(`select id from requests where endpoint_id = $1
		and (received_at, id) < ($2, $3)
		order by received_at desc, id desc limit 50`, ep.ID, c.ReceivedAt, c.ID)

	t.Logf("rows actually read -- OFFSET 4000: %d, cursor: %d", deepOffset, byCursor)

	if deepOffset <= byCursor {
		t.Errorf("expected deep OFFSET to read far more rows than the cursor; got %d vs %d",
			deepOffset, byCursor)
	}
}
