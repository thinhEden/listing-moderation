package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"listing-moderation/internal/listing"
)

func TestGetListingNotFound(t *testing.T) {
	s := migrated(t)
	if _, err := s.GetListing(context.Background(), "l-nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestGetListingReturnsStoredFieldsAndDecision(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	f := listing.Fields{SellerID: "s-1", Title: "  Hàng ĐẸP ", Description: "line1\nline2", PriceVND: 1<<63 - 1, CategoryID: "c-phones"}
	o, err := s.CreateListing(ctx, params(nil, f))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetListing(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SellerID != f.SellerID || got.Title != f.Title || got.Description != f.Description ||
		got.PriceVND != f.PriceVND || got.CategoryID != f.CategoryID {
		t.Fatalf("stored values changed: %+v", got)
	}
	if got.CreatedAt.Location() != time.UTC || time.Since(got.CreatedAt) > time.Minute {
		t.Fatalf("created_at %v", got.CreatedAt)
	}

	// Pending: the API shows decision and decided_at as null.
	b, _ := json.Marshal(got)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if v, ok := m["decision"]; !ok || v != nil {
		t.Fatalf("pending decision should marshal as null, got %v (present=%v)", v, ok)
	}
	if v, ok := m["decided_at"]; !ok || v != nil {
		t.Fatalf("pending decided_at should marshal as null, got %v (present=%v)", v, ok)
	}

	// Decided: the stored decision comes back unchanged.
	const decision = `{"outcome":"escalated","reasons":[{"rule_id":"R5","message":"seller s-1 is not verified","detail":{"seller_id":"s-1"}}]}`
	if _, err := s.pool.Exec(ctx,
		`UPDATE listings SET status='needs_human', decision=$2::jsonb, decided_at=now() WHERE id=$1`, o.ID, decision); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetListing(ctx, o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != listing.StatusNeedsHuman || got.DecidedAt == nil {
		t.Fatalf("%+v", got)
	}
	var want, have any
	_ = json.Unmarshal([]byte(decision), &want)
	if err := json.Unmarshal(got.Decision, &have); err != nil {
		t.Fatal(err)
	}
	wb, _ := json.Marshal(want)
	hb, _ := json.Marshal(have)
	if string(wb) != string(hb) {
		t.Fatalf("decision changed:\n got %s\nwant %s", hb, wb)
	}
}

func TestListListings(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	mk := func(seller, title string) string {
		o, err := s.CreateListing(ctx, params(nil, fields(seller, title)))
		if err != nil {
			t.Fatal(err)
		}
		return o.ID
	}
	a := mk("s-1", "a")
	b := mk("s-2", "b")
	c := mk("s-1", "c")
	d := mk("s-1", "d")
	if _, err := s.pool.Exec(ctx,
		`UPDATE listings SET status='published', decision='{"outcome":"approved","reasons":[]}', decided_at=now() WHERE id = ANY($1)`,
		[]string{a, b}); err != nil {
		t.Fatal(err)
	}

	ids := func(f ListFilter) []string {
		items, err := s.ListListings(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, l := range items {
			out = append(out, l.ID)
		}
		return out
	}
	eq := func(got, want []string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	seller1, seller3 := "s-1", "s-3"
	published, pending := listing.StatusPublished, listing.StatusPending

	eq(ids(ListFilter{Limit: 100}), []string{d, c, b, a}) // newest first
	eq(ids(ListFilter{Limit: 2}), []string{d, c})
	eq(ids(ListFilter{Limit: 100, SellerID: &seller1}), []string{d, c, a})
	eq(ids(ListFilter{Limit: 100, Status: &published}), []string{b, a})
	eq(ids(ListFilter{Limit: 100, Status: &pending}), []string{d, c})
	eq(ids(ListFilter{Limit: 100, SellerID: &seller1, Status: &published}), []string{a})
	eq(ids(ListFilter{Limit: 1, SellerID: &seller1, Status: &pending}), []string{d})
	eq(ids(ListFilter{Limit: 100, SellerID: &seller3}), []string{}) // unknown seller: empty, not an error
}
