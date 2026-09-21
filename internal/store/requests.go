package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DinithiPramodya/hooklens/internal/capture"
)

// StoredRequest is a captured request as it came back out of the database.
type StoredRequest struct {
	ID            string
	EndpointID    string
	Method        string
	Path          string
	Query         string
	Headers       []capture.Header
	Body          []byte
	BodySize      int
	BodyTruncated bool
	DeclaredSize  *int64
	SourceIP      *netip.Addr
	ReceivedAt    time.Time

	// The forwarding outcome, from migration 00006. All three are nil when
	// forwarding has not been attempted -- which is every row for an inbox
	// with no tunnel attached, and is a normal state rather than a failure.
	ForwardStatus *int
	ForwardError  *string
	ForwardMS     *int
}

// headerJSON is the on-disk shape of one header inside the jsonb array.
//
// Lowercase field names because this is a wire format that a UI and psql both
// read; Go's exported-field capitalisation should not leak into the database.
type headerJSON struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// InsertRequest stores one captured request and returns its id.
func (s *Store) InsertRequest(ctx context.Context, endpointID string, req *capture.Request) (string, error) {
	headers, err := encodeHeaders(req.Headers)
	if err != nil {
		return "", err
	}

	// Content-Length is -1 when the sender did not send one (chunked encoding).
	// Store that as SQL NULL rather than -1: "unknown" and "negative one" are
	// different statements, and a NULL cannot be accidentally arithmetic'd.
	var declared *int64
	if req.DeclaredSize >= 0 {
		d := req.DeclaredSize
		declared = &d
	}

	// Likewise an unparseable RemoteAddr becomes NULL rather than an empty
	// string -- inet has no empty value, and "we could not tell" is real
	// information.
	var ip *netip.Addr
	if req.SourceIP.IsValid() {
		a := req.SourceIP
		ip = &a
	}

	const q = `
		insert into requests (
			endpoint_id, method, path, query, headers,
			body, body_size, body_truncated, declared_size, source_ip, received_at
		) values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		returning id::text`

	var id string
	err = s.pool.QueryRow(ctx, q,
		endpointID, req.Method, req.Path, req.Query, headers,
		req.Body, len(req.Body), req.Truncated, declared, ip, req.ReceivedAt,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert request: %w", err)
	}
	return id, nil
}

// Page is one page of results plus the token for the next one.
type Page struct {
	Requests []StoredRequest
	// NextCursor is empty when this is the last page.
	NextCursor string
}

const (
	defaultLimit = 50
	maxLimit     = 200
)

// listFirstPage and listAfterCursor differ only in the WHERE clause.
//
// Two constants rather than one query with `($2::timestamptz is null or ...)`:
// a null-guarded predicate is harder to read, and it gives the planner a
// condition it must evaluate per row instead of a plain index bound.
const (
	selectRequestCols = `
		select id::text, endpoint_id::text, method, path, query, headers,
		       body, body_size, body_truncated, declared_size, source_ip, received_at,
		       forward_status, forward_error, forward_ms
		from requests
		where endpoint_id = $1`

	orderAndLimit = `
		order by received_at desc, id desc
		limit $2`
)

// ListRequests returns one page of an inbox's requests, newest first.
//
// Cursor pagination, not OFFSET. This list is append-only and read live while
// new captures arrive, so every new row shifts every offset -- a user paging
// through would see rows repeat. See docs/learn/09-pagination.md.
//
// Pass a nil cursor for the first page, then feed back Page.NextCursor.
func (s *Store) ListRequests(ctx context.Context, endpointID string, limit int, after *Cursor) (*Page, error) {
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}

	// Ask for one more than requested. If it comes back there is another page,
	// and we discard it. The same trick as the +1 probe on the body read in
	// unit 06: the cheapest way to distinguish "exactly this many" from "this
	// many and more" is to ask for one extra.
	//
	// The alternative -- a separate COUNT(*) -- is a second query that scans
	// every matching row to answer a question we only need one bit of.
	probe := limit + 1

	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		rows, err = s.pool.Query(ctx, selectRequestCols+orderAndLimit, endpointID, probe)
	} else {
		// Row-value comparison. Postgres compares the tuple lexicographically:
		// received_at first, and id ONLY where timestamps tie. Writing it as
		// `received_at <= $3 and (received_at < $3 or id < $4)` would be
		// equivalent and much easier to get subtly wrong.
		const cursorClause = ` and (received_at, id) < ($3, $4)`
		rows, err = s.pool.Query(ctx,
			selectRequestCols+cursorClause+orderAndLimit,
			endpointID, probe, after.ReceivedAt, after.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	// Rows must be closed or the connection never returns to the pool. Exhaust
	// the iterator and the driver closes it for you, but an early return inside
	// the loop would not -- so the defer is not optional.
	defer rows.Close()

	out := make([]StoredRequest, 0, limit)
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	// rows.Err() reports a failure that happened mid-iteration, which Next()
	// signals only by returning false -- indistinguishable from "no more rows"
	// without this check.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}

	page := &Page{Requests: out}
	if len(out) > limit {
		// The probe row came back: trim it, and hand out a cursor pointing at
		// the last row the caller actually receives.
		page.Requests = out[:limit]
		last := page.Requests[limit-1]
		page.NextCursor = Cursor{ReceivedAt: last.ReceivedAt, ID: last.ID}.String()
	}
	return page, nil
}

// GetRequest fetches one captured request by id.
func (s *Store) GetRequest(ctx context.Context, id string) (*StoredRequest, error) {
	const q = `
		select id::text, endpoint_id::text, method, path, query, headers,
		       body, body_size, body_truncated, declared_size, source_ip, received_at,
		       forward_status, forward_error, forward_ms
		from requests
		where id = $1`

	r, err := scanRequest(s.pool.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// scanner is satisfied by both pgx.Row and pgx.Rows, so one scan function
// serves the single-row and multi-row queries.
type scanner interface {
	Scan(dest ...any) error
}

func scanRequest(sc scanner) (*StoredRequest, error) {
	var (
		r       StoredRequest
		headers []byte
	)
	err := sc.Scan(
		&r.ID, &r.EndpointID, &r.Method, &r.Path, &r.Query, &headers,
		&r.Body, &r.BodySize, &r.BodyTruncated, &r.DeclaredSize, &r.SourceIP, &r.ReceivedAt,
		&r.ForwardStatus, &r.ForwardError, &r.ForwardMS,
	)
	if err != nil {
		return nil, err
	}
	if r.Headers, err = decodeHeaders(headers); err != nil {
		return nil, err
	}
	return &r, nil
}

func encodeHeaders(hs []capture.Header) ([]byte, error) {
	out := make([]headerJSON, len(hs))
	for i, h := range hs {
		out[i] = headerJSON{Name: h.Name, Value: h.Value}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode headers: %w", err)
	}
	return b, nil
}

func decodeHeaders(b []byte) ([]capture.Header, error) {
	if len(b) == 0 {
		return nil, nil
	}
	var in []headerJSON
	if err := json.Unmarshal(b, &in); err != nil {
		return nil, fmt.Errorf("decode headers: %w", err)
	}
	out := make([]capture.Header, len(in))
	for i, h := range in {
		out[i] = capture.Header{Name: h.Name, Value: h.Value}
	}
	return out, nil
}

// scanRequestInto scans a request row that also carries the owning endpoint's
// token hash, for the authenticated single-request query.
//
// Separate from scanRequest rather than adding an optional parameter: the two
// queries select different column lists, and a shared function taking a "do we
// have a hash column?" flag is the kind of thing that silently mis-scans the
// day someone adds a column to one query and not the other.
func scanRequestInto(sc scanner, r *StoredRequest, hash *[]byte) error {
	var headers []byte
	err := sc.Scan(
		&r.ID, &r.EndpointID, &r.Method, &r.Path, &r.Query, &headers,
		&r.Body, &r.BodySize, &r.BodyTruncated, &r.DeclaredSize, &r.SourceIP,
		&r.ReceivedAt, &r.ForwardStatus, &r.ForwardError, &r.ForwardMS, hash,
	)
	if err != nil {
		return err
	}
	r.Headers, err = decodeHeaders(headers)
	return err
}

// DeleteExpiredRequests removes up to batch requests that are past their
// inbox's retention window. It returns how many it deleted.
//
// A returned count below batch means the backlog is drained; equal to batch
// means call again.
func (s *Store) DeleteExpiredRequests(ctx context.Context, batch int) (int64, error) {
	if batch <= 0 {
		batch = 1000
	}

	// The subquery picks the ids first, then the outer statement deletes
	// exactly those. DELETE ... LIMIT is not valid SQL in Postgres, and even
	// where it exists a bare `delete ... where received_at < ...` would have
	// no bound at all -- one statement holding locks across an entire backlog.
	//
	// make_interval(hours => ...) rather than string concatenation into an
	// interval literal: it takes the value as a parameter instead of building
	// SQL text from a column.
	const q = `
		delete from requests
		where id in (
			select r.id
			from requests r
			join endpoints e on e.id = r.endpoint_id
			where r.received_at < now() - make_interval(hours => e.retention_hours)
			limit $1
		)`

	tag, err := s.pool.Exec(ctx, q, batch)
	if err != nil {
		return 0, fmt.Errorf("delete expired requests: %w", err)
	}
	return tag.RowsAffected(), nil
}

// SetRetention changes how long an inbox keeps captures. Used by tests and,
// later, by the settings UI.
func (s *Store) SetRetention(ctx context.Context, endpointID string, hours int) error {
	const q = `update endpoints set retention_hours = $2 where id = $1`
	_, err := s.pool.Exec(ctx, q, endpointID, hours)
	if err != nil {
		return fmt.Errorf("set retention: %w", err)
	}
	return nil
}

// ForwardOutcome is what happened when a capture was handed to a tunnel.
//
// Exactly one of Status and Error is meaningful, mirroring the CHECK
// constraint in migration 00006. The type makes that pairing explicit so
// callers cannot set both without noticing.
type ForwardOutcome struct {
	// Status is the local app's HTTP status, when it was reached.
	Status int
	// Error is a short machine code: no_tunnel, timeout, disconnected,
	// unreachable, protocol. Set only when the app was NOT reached.
	Error string
	// Elapsed is the round trip, in milliseconds.
	Elapsed int
}

// RecordForward stores the result of a forwarding attempt.
//
// This is the one place the append-only rule from 00002 is bent: the row is
// inserted before forwarding is attempted -- the durability line requires it
// -- so the outcome can only be written afterwards. A single UPDATE touching
// three columns on a row we just inserted, which is still in cache.
func (s *Store) RecordForward(ctx context.Context, requestID string, out ForwardOutcome) error {
	var status *int
	if out.Status != 0 {
		status = &out.Status
	}
	var errCode *string
	if out.Error != "" {
		errCode = &out.Error
	}

	const q = `
		update requests
		set forward_status = $2, forward_error = $3, forward_ms = $4
		where id = $1`
	if _, err := s.pool.Exec(ctx, q, requestID, status, errCode, out.Elapsed); err != nil {
		return fmt.Errorf("record forward: %w", err)
	}
	return nil
}
