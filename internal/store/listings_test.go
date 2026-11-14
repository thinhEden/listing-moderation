package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"listing-moderation/internal/listing"
)

// ---- helpers ---------------------------------------------------------------

func fields(seller, title string) listing.Fields {
	return listing.Fields{SellerID: seller, Title: title, Description: "d", PriceVND: 1000, CategoryID: "c-phones"}
}

func params(key *string, f listing.Fields) CreateParams {
	return CreateParams{
		Fields:         f,
		Key:            key,
		RequestHash:    listing.RequestHash(f),
		DupFingerprint: listing.DupFingerprint(f),
	}
}

func keyPtr(s string) *string { return &s }

func countListings(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM listings`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// hold stops the first transaction to reach CreateListing's afterInsert hook
// (INSERT done, COMMIT not yet) until release is called, and makes it fail with
// failWith if that is non-nil. Later calls pass straight through, so a request
// that inserts after the first one rolled back is not held as well.
type hold struct {
	pidCh    chan uint32
	gate     chan struct{}
	failWith error
	calls    int
	mu       sync.Mutex
	once     sync.Once
}

// newHold also releases the hold when the test ends, so a failing test cannot
// leave a transaction open that would make schema cleanup hang.
func newHold(t *testing.T, failWith error) *hold {
	h := &hold{pidCh: make(chan uint32, 1), gate: make(chan struct{}), failWith: failWith}
	t.Cleanup(h.release)
	return h
}

func (h *hold) hook(pid uint32) error {
	h.mu.Lock()
	h.calls++
	first := h.calls == 1
	h.mu.Unlock()
	if !first {
		return nil
	}
	h.pidCh <- pid
	<-h.gate
	return h.failWith
}

// holderPID blocks until the held transaction has inserted, and returns its backend pid.
func (h *hold) holderPID(t *testing.T) uint32 {
	t.Helper()
	select {
	case pid := <-h.pidCh:
		return pid
	case <-time.After(10 * time.Second):
		t.Fatal("first transaction never reached the hook")
		return 0
	}
}

func (h *hold) release() { h.once.Do(func() { close(h.gate) }) }

// waitBlockedBy waits until exactly n backends are blocked by holderPID, as
// reported by Postgres itself (pg_blocking_pids). It identifies the waiting
// connections by what they wait for, not by timing: when this returns, every
// waiter is provably queued behind the holder.
func waitBlockedBy(t *testing.T, s *Store, holderPID uint32, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got int
	for time.Now().Before(deadline) {
		err := s.pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`,
			int32(holderPID)).Scan(&got)
		if err != nil {
			t.Fatal(err)
		}
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected %d backends blocked by pid %d, saw %d", n, holderPID, got)
}

type outcome struct {
	res CreateResult
	err error
}

func goCreate(s *Store, p CreateParams) <-chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		res, err := s.CreateListing(context.Background(), p)
		ch <- outcome{res, err}
	}()
	return ch
}

func await(t *testing.T, ch <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(15 * time.Second):
		t.Fatal("CreateListing did not return")
		return outcome{}
	}
}

func notDone(t *testing.T, ch <-chan outcome, why string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("returned too early: %s", why)
	default:
	}
}

// ---- same key, same body ---------------------------------------------------

// N requests with the same key and body race while the first one has inserted
// but not committed. Exactly one listing must come out of it, the first request
// must not return before its commit, and every other request must replay it.
func TestCreateSameKeySameBodyConcurrent(t *testing.T) {
	s := migrated(t)
	h := newHold(t, nil)
	s.hooks.afterInsert = h.hook
	p := params(keyPtr("k1"), fields("s-1", "iPhone"))

	first := goCreate(s, p)
	pid := h.holderPID(t)

	// The insert has run but is uncommitted: invisible to others, and the
	// request has not been answered.
	if n := countListings(t, s); n != 0 {
		t.Fatalf("uncommitted listing is visible: %d rows", n)
	}
	notDone(t, first, "CreateListing must not return before COMMIT")

	const waiters = 8
	var rest []<-chan outcome
	for i := 0; i < waiters; i++ {
		rest = append(rest, goCreate(s, p))
	}
	waitBlockedBy(t, s, pid, waiters)
	for i, ch := range rest {
		notDone(t, ch, fmt.Sprintf("waiter %d must wait for the first transaction", i))
	}

	h.release()

	o := await(t, first)
	if o.err != nil || !o.res.Created || o.res.Status != listing.StatusPending {
		t.Fatalf("first request: %+v", o)
	}
	for i, ch := range rest {
		w := await(t, ch)
		if w.err != nil {
			t.Fatalf("waiter %d: %v", i, w.err)
		}
		if w.res.Created {
			t.Fatalf("waiter %d created a second listing", i)
		}
		if w.res.ID != o.res.ID {
			t.Fatalf("waiter %d got id %s, want %s", i, w.res.ID, o.res.ID)
		}
		if w.res.Status != listing.StatusPending {
			t.Fatalf("waiter %d status %s", i, w.res.Status)
		}
	}
	if n := countListings(t, s); n != 1 {
		t.Fatalf("%d listings, want 1", n)
	}
}

// ---- same key, different body ----------------------------------------------

func TestCreateSameKeyDifferentBodyConcurrent(t *testing.T) {
	s := migrated(t)
	h := newHold(t, nil)
	s.hooks.afterInsert = h.hook
	key := keyPtr("k1")

	first := goCreate(s, params(key, fields("s-1", "iPhone")))
	pid := h.holderPID(t)

	const waiters = 5
	var rest []<-chan outcome
	for i := 0; i < waiters; i++ {
		rest = append(rest, goCreate(s, params(key, fields("s-1", fmt.Sprintf("Different %d", i)))))
	}
	waitBlockedBy(t, s, pid, waiters)
	h.release()

	o := await(t, first)
	if o.err != nil || !o.res.Created {
		t.Fatalf("first request: %+v", o)
	}
	for i, ch := range rest {
		if w := await(t, ch); !errors.Is(w.err, ErrKeyConflict) {
			t.Fatalf("waiter %d: want ErrKeyConflict, got %+v", i, w)
		}
	}
	// Only the first body was stored.
	var title string
	if err := s.pool.QueryRow(context.Background(), `SELECT title FROM listings`).Scan(&title); err != nil {
		t.Fatal(err)
	}
	if title != "iPhone" || countListings(t, s) != 1 {
		t.Fatalf("stored %q, %d rows", title, countListings(t, s))
	}
}

// A different body is a conflict even after the first request has long finished.
func TestCreateKeyReuseAfterCommit(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	key := keyPtr("k1")
	a, err := s.CreateListing(ctx, params(key, fields("s-1", "iPhone")))
	if err != nil || !a.Created {
		t.Fatal(a, err)
	}
	// Same body: replay.
	b, err := s.CreateListing(ctx, params(key, fields("s-1", "iPhone")))
	if err != nil || b.Created || b.ID != a.ID {
		t.Fatalf("replay: %+v %v", b, err)
	}
	// One byte different: conflict. Case, price and category all count.
	for name, f := range map[string]listing.Fields{
		"title case": fields("s-1", "iphone"),
		"price":      {SellerID: "s-1", Title: "iPhone", Description: "d", PriceVND: 1001, CategoryID: "c-phones"},
		"category":   {SellerID: "s-1", Title: "iPhone", Description: "d", PriceVND: 1000, CategoryID: "c-laptops"},
	} {
		if _, err := s.CreateListing(ctx, params(key, f)); !errors.Is(err, ErrKeyConflict) {
			t.Fatalf("%s: want ErrKeyConflict, got %v", name, err)
		}
	}
	if n := countListings(t, s); n != 1 {
		t.Fatalf("%d listings, want 1", n)
	}
}

// A replay reports the listing's current status, not "pending".
func TestCreateReplayReportsCurrentStatus(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	p := params(keyPtr("k1"), fields("s-1", "iPhone"))
	a, err := s.CreateListing(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE listings SET status='published', decision='{"outcome":"approved","reasons":[]}', decided_at=now() WHERE id=$1`, a.ID); err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateListing(ctx, p)
	if err != nil || b.Created || b.ID != a.ID || b.Status != listing.StatusPublished {
		t.Fatalf("replay: %+v %v", b, err)
	}
}

// ---- first transaction rolls back while others wait ------------------------

// The first request dies after its INSERT (the process could crash, the commit
// could fail). Its key must be released: a waiting request with the same key
// then creates the listing instead of replaying one that never existed.
func TestCreateFirstRollsBackWhileOtherWaits(t *testing.T) {
	s := migrated(t)
	boom := errors.New("injected failure before commit")
	h := newHold(t, boom)
	s.hooks.afterInsert = h.hook
	p := params(keyPtr("k1"), fields("s-1", "iPhone"))

	first := goCreate(s, p)
	pid := h.holderPID(t)
	second := goCreate(s, p)
	waitBlockedBy(t, s, pid, 1)
	notDone(t, second, "the waiter must wait for the first transaction")

	h.release()

	if o := await(t, first); !errors.Is(o.err, boom) || o.res.Created {
		t.Fatalf("first request should fail with the injected error, got %+v", o)
	}
	w := await(t, second)
	if w.err != nil || !w.res.Created {
		t.Fatalf("waiter should have created the listing after the rollback, got %+v", w)
	}
	if n := countListings(t, s); n != 1 {
		t.Fatalf("%d listings, want 1", n)
	}
	var id string
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM listings`).Scan(&id); err != nil || id != w.res.ID {
		t.Fatalf("stored id %q, want the waiter's %q (%v)", id, w.res.ID, err)
	}

	// And the client's retry of the first request now replays the waiter's listing.
	again, err := s.CreateListing(context.Background(), p)
	if err != nil || again.Created || again.ID != w.res.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
}

// After a rollback several waiters with mixed bodies race for the freed key.
// Whoever inserts first wins; the rest must agree with that winner.
func TestCreateFirstRollsBackMixedWaiters(t *testing.T) {
	s := migrated(t)
	h := newHold(t, errors.New("injected failure before commit"))
	s.hooks.afterInsert = h.hook
	key := keyPtr("k1")
	bodies := []listing.Fields{
		fields("s-1", "A"), fields("s-1", "A"), fields("s-1", "B"), fields("s-1", "B"), fields("s-1", "A"),
	}

	first := goCreate(s, params(key, fields("s-1", "A")))
	pid := h.holderPID(t)
	var rest []<-chan outcome
	for _, f := range bodies {
		rest = append(rest, goCreate(s, params(key, f)))
	}
	waitBlockedBy(t, s, pid, len(bodies))
	h.release()
	if o := await(t, first); o.err == nil {
		t.Fatal("first request should have failed")
	}

	var winnerID, winnerTitle string
	results := make([]outcome, len(rest))
	for i, ch := range rest {
		results[i] = await(t, ch)
		if results[i].err == nil && results[i].res.Created {
			if winnerID != "" {
				t.Fatal("two requests created a listing for the same key")
			}
			winnerID, winnerTitle = results[i].res.ID, bodies[i].Title
		}
	}
	if winnerID == "" {
		t.Fatal("nobody created the listing")
	}
	for i, o := range results {
		switch {
		case o.err == nil && o.res.Created: // the winner
		case bodies[i].Title == winnerTitle:
			if o.err != nil || o.res.ID != winnerID {
				t.Fatalf("waiter %d (same body as winner): %+v", i, o)
			}
		default:
			if !errors.Is(o.err, ErrKeyConflict) {
				t.Fatalf("waiter %d (other body): want ErrKeyConflict, got %+v", i, o)
			}
		}
	}
	if n := countListings(t, s); n != 1 {
		t.Fatalf("%d listings, want 1", n)
	}
}

// A request that fails before committing leaves nothing behind.
func TestCreateFailureBeforeCommitLeavesNoTrace(t *testing.T) {
	s := migrated(t)
	h := newHold(t, errors.New("injected"))
	s.hooks.afterInsert = h.hook
	h.release() // do not actually hold; just fail
	p := params(keyPtr("k1"), fields("s-1", "iPhone"))

	if _, err := s.CreateListing(context.Background(), p); err == nil {
		t.Fatal("expected the injected error")
	}
	if n := countListings(t, s); n != 0 {
		t.Fatalf("%d rows after a failed create", n)
	}
	// The same key is free again.
	o, err := s.CreateListing(context.Background(), p)
	if err != nil || !o.Created {
		t.Fatalf("retry: %+v %v", o, err)
	}
}

// ---- waiting too long ------------------------------------------------------

func TestCreateWaitingTooLongReturnsBusy(t *testing.T) {
	s := migrated(t)
	s.lockTimeout = 300 * time.Millisecond
	h := newHold(t, nil)
	s.hooks.afterInsert = h.hook
	p := params(keyPtr("k1"), fields("s-1", "iPhone"))

	first := goCreate(s, p)
	h.holderPID(t)

	start := time.Now()
	waiter := await(t, goCreate(s, p))
	if !errors.Is(waiter.err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %+v", waiter)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("waited %s despite a 300ms lock timeout", d)
	}

	h.release()
	if o := await(t, first); o.err != nil || !o.res.Created {
		t.Fatalf("first request: %+v", o)
	}
	// The busy request can simply be retried and now replays.
	retry, err := s.CreateListing(context.Background(), p)
	if err != nil || retry.Created {
		t.Fatalf("retry: %+v %v", retry, err)
	}
	if n := countListings(t, s); n != 1 {
		t.Fatalf("%d listings, want 1", n)
	}
}

// ---- scope of keys ---------------------------------------------------------

func TestCreateWithoutKeyAlwaysCreates(t *testing.T) {
	s := migrated(t)
	p := params(nil, fields("s-1", "iPhone"))
	const n = 10
	var chans []<-chan outcome
	for i := 0; i < n; i++ {
		chans = append(chans, goCreate(s, p))
	}
	ids := map[string]bool{}
	for _, ch := range chans {
		o := await(t, ch)
		if o.err != nil || !o.res.Created {
			t.Fatalf("%+v", o)
		}
		ids[o.res.ID] = true
	}
	if len(ids) != n || countListings(t, s) != n {
		t.Fatalf("%d distinct ids, %d rows, want %d", len(ids), countListings(t, s), n)
	}
}

func TestCreateSameKeyDifferentSellersAreSeparate(t *testing.T) {
	s := migrated(t)
	key := keyPtr("shared")
	a, errA := s.CreateListing(context.Background(), params(key, fields("s-1", "iPhone")))
	b, errB := s.CreateListing(context.Background(), params(key, fields("s-2", "iPhone")))
	if errA != nil || errB != nil || !a.Created || !b.Created || a.ID == b.ID {
		t.Fatalf("%+v %v / %+v %v", a, errA, b, errB)
	}
	if n := countListings(t, s); n != 2 {
		t.Fatalf("%d listings, want 2", n)
	}
}

// A pending listing is what the worker queue reads: no decision, no lease.
func TestCreatedListingIsPendingAndUndecided(t *testing.T) {
	s := migrated(t)
	o, err := s.CreateListing(context.Background(), params(nil, fields("s-1", "iPhone")))
	if err != nil {
		t.Fatal(err)
	}
	l, err := s.GetListing(context.Background(), o.ID)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != listing.StatusPending || l.Decision != nil || l.DecidedAt != nil {
		t.Fatalf("%+v", l)
	}
	var attempts int
	var lease *string
	if err := s.pool.QueryRow(context.Background(),
		`SELECT attempts, lease_token::text FROM listings WHERE id=$1`, o.ID).Scan(&attempts, &lease); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || lease != nil {
		t.Fatalf("attempts=%d lease=%v", attempts, lease)
	}
}
