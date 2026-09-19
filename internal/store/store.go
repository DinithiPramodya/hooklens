// Package store is the Postgres layer: a connection pool and the queries that
// use it. No HTTP types cross this boundary in either direction.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup finds nothing. Callers compare with
// errors.Is rather than inspecting pgx's own sentinel, so the pgx dependency
// stops at this package boundary.
var ErrNotFound = errors.New("not found")

type Store struct {
	pool *pgxpool.Pool
}

// Open creates the pool and verifies it can reach the database.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	// A ceiling well under Postgres's default max_connections of 100, leaving
	// room for migrations, psql, and a second instance. Connections are not
	// free on the server side -- each is a backend process -- so more is not
	// better. The right number is "enough to keep the CPU busy", which for a
	// workload this small is single digits.
	cfg.MaxConns = 10

	// Keep a couple warm so the first capture after an idle period does not pay
	// for a TCP handshake plus authentication.
	cfg.MinConns = 2

	// Recycle connections periodically. Guards against a connection that has
	// silently died behind a NAT or load balancer idle timeout, which otherwise
	// surfaces as one mysterious failed request.
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	// NewWithConfig is lazy -- it does not connect. Ping now so a wrong host or
	// password fails at startup with a clear message, rather than on the first
	// captured webhook with a confusing one.
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// isCode reports whether err is a Postgres error with the given SQLSTATE.
//
// errors.As rather than a type assertion: pgx wraps its errors, so a direct
// assertion on the top-level error misses anything that has been annotated with
// %w on the way up.
func isCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
