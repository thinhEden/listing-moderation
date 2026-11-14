package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"listing-moderation/internal/listing"
)

var (
	// ErrNotFound: no listing with that id.
	ErrNotFound = errors.New("listing not found")
	// ErrKeyConflict: the idempotency key was already used with a different request.
	ErrKeyConflict = errors.New("idempotency key reused with a different request body")
	// ErrBusy: another request holds the same idempotency key and did not finish
	// in time. The client should retry.
	ErrBusy = errors.New("another request with the same idempotency key is still in progress")
)

// pgLockNotAvailable is SQLSTATE 55P03, raised when lock_timeout expires.
const pgLockNotAvailable = "55P03"

// CreateParams is a validated submission ready to store.
type CreateParams struct {
	Fields listing.Fields
	// Key is the Idempotency-Key, or nil when the client sent none. Keys are
	// scoped to the seller.
	Key            *string
	RequestHash    []byte
	DupFingerprint []byte
}

// CreateResult says which listing the request resolved to.
type CreateResult struct {
	ID     string
	Status listing.Status
	// Created is true when this call inserted the listing (HTTP 202) and false
	// when it found an existing one for the same key and body (HTTP 200).
	Created bool
}

const insertListingSQL = `
INSERT INTO listings
    (id, seller_id, category_id, title, description, price_vnd,
     created_at, idempotency_key, request_hash, dup_fingerprint)
VALUES ($1, $2, $3, $4, $5, $6, clock_timestamp(), $7, $8, $9)
ON CONFLICT (seller_id, idempotency_key) DO NOTHING
RETURNING id`

// CreateListing stores a new pending listing, or finds the one an earlier
// request with the same (seller, Idempotency-Key) already created.
//
// The whole thing is one short READ COMMITTED transaction:
//
//  1. INSERT ... ON CONFLICT DO NOTHING on UNIQUE (seller_id, idempotency_key).
//     If another transaction is inserting the same key right now, this
//     statement waits for it to finish. If that transaction commits, DO NOTHING
//     applies and no row comes back; if it rolls back, our INSERT proceeds. So
//     for a given key exactly one transaction ever inserts, however many race.
//  2. If a row came back, this call created the listing: commit and report it.
//     The caller must answer 202 only after this function returns, i.e. after
//     the commit succeeded.
//  3. If no row came back, a listing with this key already exists and is
//     committed. A separate SELECT (a fresh snapshot at READ COMMITTED) reads it
//     and compares request hashes: equal means a replay, different means the key
//     was reused for another request (ErrKeyConflict).
//
// A pending row is also the work queue entry, so creating the listing and
// enqueueing it for the worker are the same write. A request without a key has
// a NULL key, and NULLs never conflict, so it always creates a listing.
//
// lock_timeout bounds how long a request waits behind another one with the
// same key; past it the caller gets ErrBusy and should answer 503.
func (s *Store) CreateListing(ctx context.Context, p CreateParams) (CreateResult, error) {
	id, err := newListingID()
	if err != nil {
		return CreateResult{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CreateResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SET LOCAL does not take bind parameters; the value is an integer we format.
	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = "+strconv.FormatInt(s.lockTimeout.Milliseconds(), 10)); err != nil {
		return CreateResult{}, fmt.Errorf("set lock_timeout: %w", err)
	}

	f := p.Fields
	var inserted string
	err = tx.QueryRow(ctx, insertListingSQL,
		id, f.SellerID, f.CategoryID, f.Title, f.Description, f.PriceVND,
		p.Key, p.RequestHash, p.DupFingerprint,
	).Scan(&inserted)

	switch {
	case err == nil:
		// This request created the listing.
		if s.hooks.afterInsert != nil {
			if err := s.hooks.afterInsert(tx.Conn().PgConn().PID()); err != nil {
				return CreateResult{}, err // deferred Rollback undoes the insert
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return CreateResult{}, fmt.Errorf("commit: %w", err)
		}
		return CreateResult{ID: inserted, Status: listing.StatusPending, Created: true}, nil

	case errors.Is(err, pgx.ErrNoRows):
		// The key exists. Fall through to read the winner.

	case isLockTimeout(err):
		return CreateResult{}, ErrBusy

	default:
		return CreateResult{}, fmt.Errorf("insert listing: %w", err)
	}

	if p.Key == nil {
		// Unreachable: a NULL key never conflicts.
		return CreateResult{}, errors.New("insert conflicted without an idempotency key")
	}
	var (
		existing listing.Status
		hash     []byte
		existID  string
	)
	err = tx.QueryRow(ctx,
		`SELECT id, status, request_hash FROM listings WHERE seller_id = $1 AND idempotency_key = $2`,
		f.SellerID, *p.Key).Scan(&existID, &existing, &hash)
	if err != nil {
		// Keys are never deleted, so the row must exist. If it somehow does
		// not, let the client retry rather than guess.
		return CreateResult{}, fmt.Errorf("read listing for idempotency key: %w", err)
	}
	if !bytes.Equal(hash, p.RequestHash) {
		return CreateResult{}, ErrKeyConflict
	}
	return CreateResult{ID: existID, Status: existing, Created: false}, nil
}

func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgLockNotAvailable
}

func newListingID() (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate id: %w", err)
	}
	return "l-" + hex.EncodeToString(b[:]), nil
}

const listingColumns = `id, seller_id, title, description, price_vnd, category_id,
    status, created_at, decided_at, decision`

func scanListing(row pgx.Row) (listing.Listing, error) {
	var l listing.Listing
	var decision []byte
	err := row.Scan(&l.ID, &l.SellerID, &l.Title, &l.Description, &l.PriceVND, &l.CategoryID,
		&l.Status, &l.CreatedAt, &l.DecidedAt, &decision)
	if err != nil {
		return listing.Listing{}, err
	}
	l.CreatedAt = l.CreatedAt.UTC()
	if l.DecidedAt != nil {
		t := l.DecidedAt.UTC()
		l.DecidedAt = &t
	}
	if len(decision) > 0 {
		l.Decision = decision
	}
	return l, nil
}

// GetListing returns one listing, or ErrNotFound.
func (s *Store) GetListing(ctx context.Context, id string) (listing.Listing, error) {
	l, err := scanListing(s.pool.QueryRow(ctx, `SELECT `+listingColumns+` FROM listings WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return listing.Listing{}, ErrNotFound
	}
	if err != nil {
		return listing.Listing{}, fmt.Errorf("get listing: %w", err)
	}
	return l, nil
}

// ListFilter selects listings for GET /v1/listings. Limit must already be
// validated (1..100).
type ListFilter struct {
	SellerID *string
	Status   *listing.Status
	Limit    int
}

// ListListings returns matching listings, newest first. Ties on created_at are
// broken by id so the order is stable.
func (s *Store) ListListings(ctx context.Context, f ListFilter) ([]listing.Listing, error) {
	var (
		where []string
		args  []any
	)
	if f.SellerID != nil {
		args = append(args, *f.SellerID)
		where = append(where, "seller_id = $"+strconv.Itoa(len(args)))
	}
	if f.Status != nil {
		args = append(args, string(*f.Status))
		where = append(where, "status = $"+strconv.Itoa(len(args)))
	}
	args = append(args, f.Limit)

	q := `SELECT ` + listingColumns + ` FROM listings`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT $` + strconv.Itoa(len(args))

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list listings: %w", err)
	}
	defer rows.Close()

	out := []listing.Listing{}
	for rows.Next() {
		l, err := scanListing(rows)
		if err != nil {
			return nil, fmt.Errorf("list listings: %w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list listings: %w", err)
	}
	return out, nil
}
