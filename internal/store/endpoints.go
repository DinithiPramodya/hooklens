package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type Endpoint struct {
	ID         string
	Slug       string
	Name       string
	CreatedAt  time.Time
	LastSeenAt *time.Time
}

// ErrSlugTaken is returned when the unique constraint on endpoints.slug fires.
var ErrSlugTaken = errors.New("slug already in use")

// uniqueViolation is Postgres's SQLSTATE for a unique constraint breach.
const uniqueViolation = "23505"

// CreateEndpoint makes a new inbox.
//
// NOTE: there is no authentication on this yet. Anyone who can reach the API can
// create an inbox with any slug they like, and anyone who guesses a slug can read
// it. Unit 08 replaces caller-chosen slugs with generated unguessable ones and
// adds a capability token. Until then this is a development-only surface.
func (s *Store) CreateEndpoint(ctx context.Context, slug, name string) (*Endpoint, error) {
	const q = `
		insert into endpoints (slug, name)
		values ($1, nullif($2, ''))
		returning id::text, slug, coalesce(name, ''), created_at, last_seen_at`

	var e Endpoint
	err := s.pool.QueryRow(ctx, q, slug, name).
		Scan(&e.ID, &e.Slug, &e.Name, &e.CreatedAt, &e.LastSeenAt)
	if err != nil {
		// Translate the driver's error into one this package owns, so callers
		// never import pgx to tell "already exists" from "database is on fire".
		if isCode(err, uniqueViolation) {
			return nil, ErrSlugTaken
		}
		return nil, fmt.Errorf("create endpoint: %w", err)
	}
	return &e, nil
}

// EndpointBySlug looks up an inbox by the name in its URL.
//
// This runs on every captured request, which is why slug carries a unique index
// -- without it this is a sequential scan on the hot path.
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
