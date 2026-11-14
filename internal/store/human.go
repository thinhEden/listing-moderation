package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/rules"
)

// ErrNotAwaitingHuman: the listing exists but is not in needs_human, either
// because it was never escalated or because someone already resolved it.
var ErrNotAwaitingHuman = errors.New("listing is not awaiting human review")

// ResolveHuman commits a moderator's decision, the one result of the human
// phase. d must be what rules.HumanDecision returns (published or rejected).
//
// It is a single compare-and-set statement:
//
//	UPDATE listings SET status = ..., decision = ..., decided_at = now()
//	WHERE id = $1 AND status = 'needs_human'
//
// When two moderators decide at once, the second UPDATE waits for the first
// row lock, then re-reads the row, finds it no longer needs_human, and updates
// nothing: exactly one wins and the others get ErrNotAwaitingHuman. The new
// decision replaces the automated escalation; no history is kept.
//
// No seller lock is needed. needs_human and published are both "active" for
// R3, so approving moves between active states, and rejecting only leaves the
// active set; neither can create a second active duplicate.
//
// Zero rows is either "no such listing" (ErrNotFound) or "wrong status"
// (ErrNotAwaitingHuman); a second query tells them apart. A listing never goes
// back to needs_human, so that query cannot misreport.
func (s *Store) ResolveHuman(ctx context.Context, id string, d rules.Decision) (listing.Listing, error) {
	if d.Status != listing.StatusPublished && d.Status != listing.StatusRejected {
		return listing.Listing{}, fmt.Errorf("a human decision must publish or reject, got %q", d.Status)
	}
	body, err := d.JSON()
	if err != nil {
		return listing.Listing{}, fmt.Errorf("encode decision: %w", err)
	}

	l, err := scanListing(s.pool.QueryRow(ctx, `
UPDATE listings
SET status = $2, decision = $3::jsonb, decided_at = now()
WHERE id = $1 AND status = 'needs_human'
RETURNING `+listingColumns, id, string(d.Status), body))
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return listing.Listing{}, fmt.Errorf("resolve listing: %w", err)
	}

	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM listings WHERE id = $1)`, id).Scan(&exists); err != nil {
		return listing.Listing{}, fmt.Errorf("resolve listing: %w", err)
	}
	if !exists {
		return listing.Listing{}, ErrNotFound
	}
	return listing.Listing{}, ErrNotAwaitingHuman
}
