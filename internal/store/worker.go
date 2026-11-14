package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/rules"
)

var (
	// ErrLeaseLost: the lease token does not match. Another attempt has claimed
	// the listing since, or it is no longer pending. The caller must stop
	// touching the lease.
	ErrLeaseLost = errors.New("lease lost: the listing was claimed again or is no longer pending")

	// ErrNotPending: the listing already has an automated result, so this
	// attempt's result was not applied. It is not a failure of the system: it
	// means another attempt got there first.
	ErrNotPending = errors.New("listing is no longer pending; its result was already committed")
)

const (
	pgUniqueViolation = "23505"

	// maxCommitAttempts bounds how often a commit is redone after the unique
	// index on active fingerprints fires. It only fires if something wrote an
	// active listing without taking the seller lock, so one retry normally
	// suffices; the second is slack.
	maxCommitAttempts = 3

	maxLastErrorRunes = 1000

	// seller lock namespace: pg_advisory_xact_lock(int4, int4) keeps these keys
	// apart from the bigint key the migrations use.
	sellerLockNamespace = 1
)

// Claim is a listing handed to one worker attempt.
type Claim struct {
	ID         string
	Fields     listing.Fields
	Attempts   int    // including this one
	LeaseToken string // identifies this claim; see RecordFailure
}

// ClaimPending takes up to limit pending listings whose lease is free or has
// expired, oldest first, and gives each a new lease for the given duration.
//
// One statement does all of it:
//
//   - FOR UPDATE SKIP LOCKED means concurrent claimers never wait for or
//     return the same row: a row another claimer has locked is simply skipped.
//   - attempts is incremented and a fresh lease_token generated for every
//     claim, including a reclaim after an expired lease. The token is what
//     lets RecordFailure tell the current attempt from an older one.
//
// The lease clock is the database's own now(), so workers on machines with
// different clocks agree on when a lease expires. The claim commits at once;
// if the worker dies afterwards the lease runs out and the listing is claimed
// again.
func (s *Store) ClaimPending(ctx context.Context, limit int, lease time.Duration) ([]Claim, error) {
	rows, err := s.pool.Query(ctx, `
UPDATE listings
SET attempts         = attempts + 1,
    lease_token      = gen_random_uuid(),
    lease_expires_at = now() + make_interval(secs => $2)
WHERE id IN (
    SELECT id FROM listings
    WHERE status = 'pending'
      AND (lease_expires_at IS NULL OR lease_expires_at < now())
    ORDER BY created_at, id
    LIMIT $1
    FOR UPDATE SKIP LOCKED)
RETURNING id, seller_id, category_id, title, description, price_vnd, attempts, lease_token::text`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("claim listings: %w", err)
	}
	defer rows.Close()

	var out []Claim
	for rows.Next() {
		var c Claim
		f := &c.Fields
		if err := rows.Scan(&c.ID, &f.SellerID, &f.CategoryID, &f.Title, &f.Description, &f.PriceVND,
			&c.Attempts, &c.LeaseToken); err != nil {
			return nil, fmt.Errorf("claim listings: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("claim listings: %w", err)
	}
	return out, nil
}

// RecordFailure notes that attempt c failed and keeps the listing unavailable
// for backoff, by pushing its lease expiry that far into the future. The next
// claim after that is a retry.
//
// The update only applies if token is still the listing's current lease token
// and the listing is still pending. A worker that was too slow, whose lease
// expired and was claimed by another, matches neither and gets ErrLeaseLost:
// it cannot overwrite the newer attempt's lease or error.
func (s *Store) RecordFailure(ctx context.Context, id, token, message string, backoff time.Duration) error {
	if utf8.RuneCountInString(message) > maxLastErrorRunes {
		message = string([]rune(message)[:maxLastErrorRunes])
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE listings
SET last_error       = $3,
    lease_expires_at = now() + make_interval(secs => $4)
WHERE id = $1
  AND lease_token = $2::text::uuid
  AND status = 'pending'`,
		id, token, message, backoff.Seconds())
	if err != nil {
		return fmt.Errorf("record failure: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// Decide computes the automated decision for one listing. CommitAutomated
// calls it with the duplicate lookup that runs inside the seller lock; the
// caller binds the listing and its R1 screening result, so R1 has already run,
// outside any transaction, when this is called.
type Decide func(findDuplicate rules.DuplicateFinder) (rules.Decision, error)

const findActiveDuplicateSQL = `
SELECT other.id
FROM listings me
JOIN listings other
  ON other.seller_id       = me.seller_id
 AND other.dup_fingerprint = me.dup_fingerprint
 AND other.id             <> me.id
WHERE me.id = $1
  AND other.status IN ('auto_approved', 'needs_human', 'published')
ORDER BY other.id
LIMIT 1`

// CommitAutomated decides the listing and commits the one automated result
// for it.
//
// Within one READ COMMITTED transaction:
//
//  1. Take the seller's advisory lock (pg_advisory_xact_lock). Every commit for
//     the same seller runs one after another from here to COMMIT; the lock is
//     released by the commit or rollback itself, never separately.
//  2. Run decide. Its R3 lookup (findDuplicate) looks for another listing of
//     this seller with the same fingerprint and an active status. Because of
//     the lock, whatever any other commit for this seller wrote is already
//     committed and visible to this statement, and nothing else can change it
//     until we finish. Listings still pending, or whose transaction has not
//     committed, are not active and are not seen. So of two duplicates, the one
//     whose commit succeeds first wins, whichever was submitted first.
//  3. Write the result with a compare-and-set on the status:
//     UPDATE ... WHERE id = $1 AND status = 'pending'. Zero rows means another
//     attempt already committed a result (or a human has resolved it), and this
//     attempt's result is dropped: ErrNotPending. That check, not the lease, is
//     what guarantees one automated result per listing.
//  4. COMMIT.
//
// The partial unique index on (seller_id, dup_fingerprint) for active statuses
// backs this up: if some other code path ever skips the lock and makes an
// active duplicate, the UPDATE fails with a unique violation, this attempt is
// rolled back and redone from the start, and the redo finds the duplicate.
//
// The transaction holds the lock while it waits only for the database, never
// for the network or a rule that can be slow (R1 ran before). lock_timeout
// bounds the wait for the lock; past it the caller gets ErrBusy and should
// treat the attempt as failed.
func (s *Store) CommitAutomated(ctx context.Context, c Claim, decide Decide) (rules.Decision, error) {
	for attempt := 1; ; attempt++ {
		d, err := s.commitAutomatedOnce(ctx, c, attempt, decide)
		if isUniqueViolation(err) && attempt < maxCommitAttempts {
			s.log.Warn("active-duplicate index fired; redoing commit", "listing", c.ID, "attempt", attempt)
			continue
		}
		return d, err
	}
}

func (s *Store) commitAutomatedOnce(ctx context.Context, c Claim, attempt int, decide Decide) (rules.Decision, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return rules.Decision{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = "+strconv.FormatInt(s.lockTimeout.Milliseconds(), 10)); err != nil {
		return rules.Decision{}, fmt.Errorf("set lock_timeout: %w", err)
	}

	// 1. Seller lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`,
		int32(sellerLockNamespace), c.Fields.SellerID); err != nil {
		if isLockTimeout(err) {
			return rules.Decision{}, ErrBusy
		}
		return rules.Decision{}, fmt.Errorf("lock seller: %w", err)
	}

	// 2. Decide, with the duplicate lookup running inside the lock.
	findDuplicate := func() (string, bool, error) {
		var id string
		err := tx.QueryRow(ctx, findActiveDuplicateSQL, c.ID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return id, true, nil
	}
	d, err := decide(findDuplicate)
	if err != nil {
		return rules.Decision{}, err
	}
	if d.Status == listing.StatusPending || !d.Status.Valid() {
		return rules.Decision{}, fmt.Errorf("decision has invalid status %q", d.Status)
	}
	body, err := d.JSON()
	if err != nil {
		return rules.Decision{}, fmt.Errorf("encode decision: %w", err)
	}

	if s.hooks.afterDecide != nil {
		if err := s.hooks.afterDecide(tx.Conn().PgConn().PID(), attempt); err != nil {
			return rules.Decision{}, err
		}
	}

	// 3. Compare-and-set on status.
	tag, err := tx.Exec(ctx, `
UPDATE listings
SET status           = $2,
    decision         = $3::jsonb,
    decided_at       = now(),
    lease_token      = NULL,
    lease_expires_at = NULL
WHERE id = $1 AND status = 'pending'`,
		c.ID, string(d.Status), body)
	if err != nil {
		return rules.Decision{}, err // a unique violation is retried by the caller
	}
	if tag.RowsAffected() == 0 {
		return rules.Decision{}, ErrNotPending
	}

	if s.hooks.beforeCommit != nil {
		if err := s.hooks.beforeCommit(tx.Conn().PgConn().PID()); err != nil {
			return rules.Decision{}, err
		}
	}

	// 4. Commit.
	if err := tx.Commit(ctx); err != nil {
		return rules.Decision{}, fmt.Errorf("commit: %w", err)
	}
	return d, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}
