package store

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
)

// explainPlan runs EXPLAIN and returns the plan as text.
func explainPlan(t *testing.T, st *Store, q string, args ...any) string {
	t.Helper()
	rows, err := st.pool.Query(t.Context(), "explain (format text) "+q, args...)
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
		t.Fatalf("rows: %v", err)
	}
	return plan.String()
}

// TestSweepUsesTheExpiryIndex is the regression test for migration 00009.
//
// The old predicate compared received_at against a retention window living on
// the JOINED endpoints row, so Postgres could not evaluate it until after the
// join and no index on requests applied. Worse, the LIMIT did not help: a
// filter that runs after a join keeps scanning until it finds enough matches,
// and when nothing is expired -- the normal state -- that is every row.
// Measured at 576,639 rows: 276ms per tick to return nothing.
//
// Asserting on the PLAN rather than on a duration, for the reason unit 09's
// plan test gives: a timing assertion on a machine under unknown load is a
// flake, and "it was fast" is not the property. The property is "it uses the
// index", and a sequential scan here is the bug coming back.
func TestSweepUsesTheExpiryIndex(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	// Enough rows that the index is genuinely the cheaper plan. On a small
	// table a sequential scan IS cheaper and the planner is right to pick it,
	// which would make this test assert the opposite of what it means.
	_, err = st.pool.Exec(ctx, `
		insert into requests (endpoint_id, method, path, declared_size, received_at, expires_at)
		select $1, 'POST', '/p' || g, null, now() - (g || ' seconds')::interval,
		       now() - (g || ' seconds')::interval
		         + make_interval(hours => (select retention_hours from endpoints where id = $1))
		from generate_series(1, 5000) g`, ep.ID)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := st.pool.Exec(ctx, `analyze requests`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	plan := explainPlan(t, st, `
		select id from requests
		where expires_at < now()
		order by expires_at
		limit 1000`)

	if !strings.Contains(plan, "requests_expires_at_idx") {
		t.Errorf("the sweep does not use the expiry index -- plan:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on requests") {
		t.Errorf("the sweep is back to a sequential scan -- plan:\n%s", plan)
	}
}

// TestSetRetentionReprojectsExistingExpiries is the cost of the
// denormalisation, made into a test.
//
// expires_at is a DERIVED COPY of the inbox's retention setting. Changing the
// setting has to rewrite it, or the copy silently disagrees with the source:
// shortening retention would leave old captures alive past the new window
// while the settings page claims otherwise, and lengthening it would have the
// next sweep delete rows the user just asked to keep.
func TestSetRetentionReprojectsExistingExpiries(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}

	// A capture from three hours ago, under a generous default retention.
	req := newCaptureFixture()
	req.ReceivedAt = time.Now().Add(-3 * time.Hour)
	if _, err := st.InsertRequest(ctx, ep.ID, req); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	// Nothing should be expired yet.
	if n, err := st.DeleteExpiredRequests(ctx, 100); err != nil || n != 0 {
		t.Fatalf("deleted %d (err %v) before the retention change, want 0", n, err)
	}

	// Shorten retention to one hour. The existing row is three hours old, so
	// it is now past its window -- but only if the stored expiry was rewritten.
	if err := st.SetRetention(ctx, ep.ID, 1); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}

	n, err := st.DeleteExpiredRequests(ctx, 100)
	if err != nil {
		t.Fatalf("DeleteExpiredRequests: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d after shortening retention, want 1 -- "+
			"the stored expiry was not reprojected, so the copy disagrees with the setting", n)
	}
}

// TestSetRetentionLengtheningRescues the other direction, which is the one
// that loses data rather than merely keeping it too long.
func TestSetRetentionLengtheningRescues(t *testing.T) {
	st := testStore(t)
	ctx := t.Context()

	ep, err := st.CreateEndpoint(ctx, "")
	if err != nil {
		t.Fatalf("CreateEndpoint: %v", err)
	}
	if err := st.SetRetention(ctx, ep.ID, 1); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}

	req := newCaptureFixture()
	req.ReceivedAt = time.Now().Add(-3 * time.Hour)
	if _, err := st.InsertRequest(ctx, ep.ID, req); err != nil {
		t.Fatalf("InsertRequest: %v", err)
	}

	// The row is now expired under the one-hour window, and the user notices
	// and raises retention before the sweeper's next tick.
	if err := st.SetRetention(ctx, ep.ID, 168); err != nil {
		t.Fatalf("SetRetention: %v", err)
	}

	if n, err := st.DeleteExpiredRequests(ctx, 100); err != nil || n != 0 {
		t.Errorf("deleted %d (err %v) after raising retention, want 0 -- "+
			"a capture the user just asked to keep was swept", n, err)
	}
}

// newCaptureRequest builds a throwaway capture. Untagged so both the ordinary
// tests here and the loadtest-tagged diagnostics can use one definition.
func newCaptureRequest(body []byte) *capture.Request {
	return &capture.Request{
		Method:       "POST",
		Path:         "/load",
		Body:         body,
		DeclaredSize: int64(len(body)),
		SourceIP:     netip.MustParseAddr("127.0.0.1"),
		Headers:      []capture.Header{{Name: "Content-Type", Value: "application/json"}},
		ReceivedAt:   time.Now(),
	}
}

func newCaptureFixture() *capture.Request {
	return newCaptureRequest([]byte(`{"event":"test"}`))
}
