package store

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"listing-moderation/internal/testdb"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Open(context.Background(), testdb.IsolatedDSN(t), 10*time.Second, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
	migs, _ := loadMigrations()
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migs) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(migs))
	}
}

// Several instances starting at once must all succeed and end with one schema.
func TestMigrateConcurrentInstances(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Migrate(ctx)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	migs, _ := loadMigrations()
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != len(migs) {
		t.Fatalf("schema_migrations has %d rows, want %d", rows, len(migs))
	}
}

func TestPing(t *testing.T) {
	s := newStore(t)
	if err := s.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenGivesUpWhenDatabaseUnreachable(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	start := time.Now()
	// Port 1 on localhost is closed: the dial fails immediately.
	_, err := Open(context.Background(), "postgres://u:p@127.0.0.1:1/db?sslmode=disable", 1500*time.Millisecond, log)
	if err == nil {
		t.Fatal("expected an error")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("Open ignored its wait budget, took %s", d)
	}
}
