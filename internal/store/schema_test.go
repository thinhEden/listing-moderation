package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests pin the schema invariants the rest of the design relies on.

func migrated(t *testing.T) *Store {
	t.Helper()
	s := newStore(t)
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// insert adds a listing; extra overrides columns by name.
func insert(t *testing.T, s *Store, id string, extra map[string]any) error {
	t.Helper()
	cols := map[string]any{
		"id": id, "seller_id": "s-1", "category_id": "c-1", "title": "t", "description": "d",
		"price_vnd": int64(1), "request_hash": []byte{1}, "dup_fingerprint": []byte{1},
	}
	for k, v := range extra {
		cols[k] = v
	}
	var names, ph []string
	var args []any
	for k, v := range cols {
		names = append(names, k)
		args = append(args, v)
		ph = append(ph, "$"+strconv.Itoa(len(args)))
	}
	_, err := s.pool.Exec(context.Background(),
		"INSERT INTO listings ("+strings.Join(names, ",")+") VALUES ("+strings.Join(ph, ",")+")", args...)
	return err
}

func decided(status string) map[string]any {
	return map[string]any{
		"status": status, "decision": `{"outcome":"approved","reasons":[]}`, "decided_at": time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestSchemaStatusEnum(t *testing.T) {
	s := migrated(t)
	if err := insert(t, s, "l-1", map[string]any{"status": "bogus"}); err == nil {
		t.Fatal("unknown status was accepted")
	}
	// draft appears in the lifecycle diagram but is not an API status.
	if err := insert(t, s, "l-2", map[string]any{"status": "draft"}); err == nil {
		t.Fatal("draft was accepted")
	}
}

func TestSchemaPendingIffNoDecision(t *testing.T) {
	s := migrated(t)
	if err := insert(t, s, "l-1", nil); err != nil {
		t.Fatalf("plain pending insert: %v", err)
	}
	if err := insert(t, s, "l-2", decided("published")); err != nil {
		t.Fatalf("decided insert: %v", err)
	}
	// pending with a decision, and decided without one, are both inconsistent.
	if err := insert(t, s, "l-3", map[string]any{"decision": `{}`, "decided_at": time.Now()}); err == nil {
		t.Fatal("pending listing with a decision was accepted")
	}
	if err := insert(t, s, "l-4", map[string]any{"status": "published"}); err == nil {
		t.Fatal("published listing without a decision was accepted")
	}
}

func TestSchemaIdempotencyKeyScopedToSeller(t *testing.T) {
	s := migrated(t)
	key := map[string]any{"idempotency_key": "k"}
	if err := insert(t, s, "l-1", key); err != nil {
		t.Fatal(err)
	}
	if err := insert(t, s, "l-2", key); err == nil {
		t.Fatal("same seller + same key was accepted twice")
	}
	if err := insert(t, s, "l-3", map[string]any{"idempotency_key": "k", "seller_id": "s-2"}); err != nil {
		t.Fatalf("same key for another seller must be allowed: %v", err)
	}
}

func TestSchemaNullKeysNeverCollide(t *testing.T) {
	s := migrated(t)
	for _, id := range []string{"l-1", "l-2", "l-3"} {
		if err := insert(t, s, id, nil); err != nil {
			t.Fatalf("listing without a key rejected: %v", err)
		}
	}
}

func TestSchemaOneActiveListingPerFingerprint(t *testing.T) {
	s := migrated(t)
	fp := func(status string, extra map[string]any) map[string]any {
		m := decided(status)
		m["dup_fingerprint"] = []byte{9}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	if err := insert(t, s, "l-1", fp("published", nil)); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"published", "needs_human", "auto_approved"} {
		if err := insert(t, s, "dup-"+status, fp(status, nil)); err == nil {
			t.Fatalf("second active listing (%s) with the same fingerprint was accepted", status)
		}
	}
	// Non-active duplicates, pending ones, other sellers and other fingerprints are fine.
	ok := map[string]map[string]any{
		"l-rejected": fp("auto_rejected", nil),
		"l-human":    fp("rejected", nil),
		"l-pending":  {"dup_fingerprint": []byte{9}},
		"l-seller":   fp("published", map[string]any{"seller_id": "s-2"}),
		"l-other-fp": fp("published", map[string]any{"dup_fingerprint": []byte{8}}),
	}
	for id, cols := range ok {
		if err := insert(t, s, id, cols); err != nil {
			t.Fatalf("%s should be allowed: %v", id, err)
		}
	}
}

func TestSchemaRejectsNegativePrice(t *testing.T) {
	s := migrated(t)
	if err := insert(t, s, "l-1", map[string]any{"price_vnd": int64(-1)}); err == nil {
		t.Fatal("negative price accepted")
	}
	if err := insert(t, s, "l-2", map[string]any{"price_vnd": int64(0)}); err != nil {
		t.Fatalf("price 0 is valid: %v", err)
	}
}
