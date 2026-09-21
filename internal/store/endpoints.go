package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/DinithiPramodya/hooklens/internal/secret"
)

type Endpoint struct {
	ID         string
	Slug       string
	Name       string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

// ErrUnauthorized is returned when a token does not match the inbox.
//
// Deliberately indistinguishable from ErrNotFound at the HTTP layer: telling an
// attacker "that inbox exists but your token is wrong" turns a 128-bit guessing
// problem into a confirmation oracle.
var ErrUnauthorized = errors.New("unauthorized")

// NewEndpoint is what CreateEndpoint returns. The token is plaintext and this
// is the only time it exists outside the caller's head -- the database holds
// only its hash.
type NewEndpoint struct {
	Endpoint
	Token string
}

// CreateEndpoint mints a new inbox with a generated slug and owner token.
//
// The caller does not choose the slug. That was allowed in unit 07 and was a
// development-only state: a caller-chosen slug is by definition guessable, so
// anyone could have created "stripe" and anyone could have found it.
func (s *Store) CreateEndpoint(ctx context.Context, name string) (*NewEndpoint, error) {
	const q = `
		insert into endpoints (slug, name, owner_token_hash)
		values ($1, nullif($2, ''), $3)
		returning id::text, slug, coalesce(name, ''), created_at, last_seen_at`

	// Retry on the astronomically unlikely slug collision rather than failing.
	// At 128 bits this loop will never run twice; it exists because "never" and
	// "returns a confusing 409 to a user who did nothing wrong" are different
	// guarantees, and the cost of being right is four lines.
	const attempts = 3
	for i := range attempts {
		slug := secret.NewSlug()
		token := secret.NewToken()

		var e Endpoint
		err := s.pool.QueryRow(ctx, q, slug, name, secret.Hash(token)).
			Scan(&e.ID, &e.Slug, &e.Name, &e.CreatedAt, &e.LastSeenAt)
		if err == nil {
			return &NewEndpoint{Endpoint: e, Token: token}, nil
		}
		if isCode(err, uniqueViolation) && i < attempts-1 {
			continue
		}
		return nil, fmt.Errorf("create endpoint: %w", err)
	}
	return nil, errors.New("create endpoint: slug collision after retries")
}

// uniqueViolation is Postgres's SQLSTATE for a unique constraint breach.
const uniqueViolation = "23505"

// EndpointBySlug looks up an inbox by the name in its URL, without checking any
// token.
//
// This is the CAPTURE path: a provider posting a webhook has no token and never
// will. Learning a slug therefore lets someone send junk to an inbox -- which is
// the accepted cost of a URL that has to be pasted into somebody else's
// dashboard. It does not let them read anything; that needs AuthenticateEndpoint.
func (s *Store) EndpointBySlug(ctx context.Context, slug string) (*Endpoint, error) {
	const q = `
		select id::text, slug, coalesce(name, ''), created_at, last_seen_at
		from endpoints
		where slug = $1`

	var e Endpoint
	err := s.pool.QueryRow(ctx, q, slug).
		Scan(&e.ID, &e.Slug, &e.Name, &e.CreatedAt, &e.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lookup endpoint %q: %w", slug, err)
	}
	return &e, nil
}

// AuthenticateEndpoint returns the inbox only if the token matches.
//
// The lookup is by slug -- a non-secret -- and the secret is then compared in
// constant time in Go. Querying `where owner_token_hash = $1` instead would
// push the comparison into an index lookup, whose timing is emphatically not
// constant, and would leave the hash in the query log.
func (s *Store) AuthenticateEndpoint(ctx context.Context, slug, token string) (*Endpoint, error) {
	const q = `
		select id::text, slug, coalesce(name, ''), created_at, last_seen_at, owner_token_hash
		from endpoints
		where slug = $1`

	var (
		e    Endpoint
		hash []byte
	)
	err := s.pool.QueryRow(ctx, q, slug).
		Scan(&e.ID, &e.Slug, &e.Name, &e.CreatedAt, &e.LastSeenAt, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Same error as a bad token, on purpose. See ErrUnauthorized.
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("authenticate endpoint: %w", err)
	}

	if !secret.Equal(hash, secret.Hash(token)) {
		return nil, ErrUnauthorized
	}
	return &e, nil
}

// AuthenticateRequest resolves a captured request and checks the caller holds
// the token for the inbox that owns it.
//
// One query rather than fetch-then-check, so there is no window in which the
// request has been read out of the database for a caller who turns out not to
// be allowed it.
func (s *Store) AuthenticateRequest(ctx context.Context, requestID, token string) (*StoredRequest, error) {
	const q = `
		select r.id::text, r.endpoint_id::text, r.method, r.path, r.query, r.headers,
		       r.body, r.body_size, r.body_truncated, r.declared_size, r.source_ip,
		       r.received_at, r.forward_status, r.forward_error, r.forward_ms,
		       e.owner_token_hash
		from requests r
		join endpoints e on e.id = r.endpoint_id
		where r.id = $1`

	var (
		req  StoredRequest
		hash []byte
	)
	err := scanRequestInto(s.pool.QueryRow(ctx, q, requestID), &req, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("authenticate request: %w", err)
	}

	if !secret.Equal(hash, secret.Hash(token)) {
		return nil, ErrUnauthorized
	}
	return &req, nil
}

// TouchEndpoint records that traffic arrived, for the "last seen" column.
//
// Deliberately separate from the insert and deliberately allowed to fail: it is
// a nicety, and a failure here must never turn a successfully captured request
// into an error the provider retries.
func (s *Store) TouchEndpoint(ctx context.Context, id string, at time.Time) error {
	const q = `update endpoints set last_seen_at = $2 where id = $1`
	_, err := s.pool.Exec(ctx, q, id, at)
	return err
}
