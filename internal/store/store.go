// Package store owns the Postgres connection pool, schema migrations and, in
// later steps, every query the service runs.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
	log  *slog.Logger

	// lockTimeout bounds how long a request waits behind another transaction
	// holding the same idempotency key.
	lockTimeout time.Duration

	// hooks let tests stop a transaction at a precise point to build a race
	// deterministically. They are nil in production.
	hooks testHooks
}

// testHooks are injected by tests in this package only.
type testHooks struct {
	// afterInsert runs inside CreateListing after the INSERT succeeded and
	// before COMMIT, with the backend pid of the transaction. Returning an
	// error rolls the transaction back.
	afterInsert func(pid uint32) error

	// afterDecide runs inside CommitAutomated after the decision was computed
	// (seller lock held) and before the status is written; attempt starts at 1.
	afterDecide func(pid uint32, attempt int) error

	// beforeCommit runs inside CommitAutomated after the status was written
	// and before COMMIT, still holding the seller lock.
	beforeCommit func(pid uint32) error
}

const defaultLockTimeout = 3 * time.Second

// Open connects to Postgres and waits up to wait for it to accept
// connections, so the service tolerates starting before its database.
func Open(ctx context.Context, dsn string, wait time.Duration, log *slog.Logger) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// Requests that wait on a shared idempotency key each hold a connection, so
	// the default (max(4, CPUs)) is too small. An explicit pool_max_conns in the
	// URL still wins.
	if !strings.Contains(dsn, "pool_max_conns") {
		cfg.MaxConns = 20
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
			return &Store{pool: pool, log: log, lockTimeout: defaultLockTimeout}, nil
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
