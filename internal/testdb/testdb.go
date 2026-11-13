// Package testdb gives integration tests an isolated Postgres schema.
//
// Tests run against a real database (see DATABASE_URL); nothing is mocked.
// Each test gets its own schema so tests can run in parallel and never see
// each other's rows.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// IsolatedDSN creates a fresh schema and returns a DSN whose search_path
// points at it. The schema is dropped when the test ends.
//
// If DATABASE_URL is unset the test is skipped, unless REQUIRE_DB=1 (as set by
// `make test`), in which case it fails: a misconfigured test environment must
// not look like a pass.
func IsolatedDSN(t testing.TB) string {
	t.Helper()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatal("DATABASE_URL is not set but REQUIRE_DB=1")
		}
		t.Skip("DATABASE_URL not set; skipping database test (run `make test`)")
	}

	var rnd [6]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		t.Fatal(err)
	}
	schema := "t_" + hex.EncodeToString(rnd[:])

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := pgx.Connect(ctx, base)
		if err != nil {
			t.Logf("cleanup connect: %v", err)
			return
		}
		defer c.Close(ctx)
		if _, err := c.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Logf("drop schema %s: %v", schema, err)
		}
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}
