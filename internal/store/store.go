// Package store owns the Postgres connection pool, schema migrations and, in
// later steps, every query the service runs.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// Open connects to Postgres and waits up to wait for it to accept
// connections, so the service tolerates starting before its database.
func Open(ctx context.Context, dsn string, wait time.Duration, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// NewWithConfig does not dial; connections are opened lazily.
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	deadline := time.Now().Add(wait)
	for attempt := 1; ; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err == nil {
			return &Store{pool: pool, log: log}, nil
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("database not reachable (waited %s): %w", wait, err)
		}
		log.Warn("database not ready, retrying", "attempt", attempt, "error", err.Error())
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}

// Ping reports whether the database currently answers. Used by /readyz.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

func (s *Store) Close() { s.pool.Close() }
