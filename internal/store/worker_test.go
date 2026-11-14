package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/rules"
	"listing-moderation/internal/seed"
)

// ---- helpers ---------------------------------------------------------------

func engine(t *testing.T) *rules.Engine {
	t.Helper()
	data, err := seed.Load(filepath.Join("..", "..", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := rules.NewEngine(data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// decideWith mirrors production: R1 runs now, outside any transaction, and its
// result is carried into the decision that runs under the seller lock.
func decideWith(e *rules.Engine, f listing.Fields) Decide {
	banned := e.Screen(f)
	return func(find rules.DuplicateFinder) (rules.Decision, error) {
		return e.Decide(f, banned, find)
	}
}

func newListing(t *testing.T, s *Store, f listing.Fields) string {
	t.Helper()
	o, err := s.CreateListing(context.Background(), params(nil, f))
	if err != nil {
		t.Fatal(err)
	}
	return o.ID
}

func content(seller, title string) listing.Fields {
	return listing.Fields{SellerID: seller, Title: title, Description: "same description", PriceVND: 6_000_000, CategoryID: "c-phones"}
}

// claimOne claims exactly one listing.
func claimOne(t *testing.T, s *Store) Claim {
	t.Helper()
	cs, err := s.ClaimPending(context.Background(), 1, time.Minute)
	if err != nil || len(cs) != 1 {
		t.Fatalf("claim: %v %v", cs, err)
	}
	return cs[0]
}

func expireLease(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE listings SET lease_expires_at = now() - interval '1 second' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
}

type row struct {
	status   listing.Status
	attempts int
	token    *string
	expires  *time.Time
	lastErr  *string
	decision *string
}

func readRow(t *testing.T, s *Store, id string) row {
	t.Helper()
	var r row
	err := s.pool.QueryRow(context.Background(),
		`SELECT status, attempts, lease_token::text, lease_expires_at, last_error, decision::text FROM listings WHERE id=$1`, id).
		Scan(&r.status, &r.attempts, &r.token, &r.expires, &r.lastErr, &r.decision)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func countByStatus(t *testing.T, s *Store, status listing.Status) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM listings WHERE status=$1`, string(status)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

type commitOutcome struct {
	d   rules.Decision
	err error
}

func goCommit(s *Store, c Claim, decide Decide) <-chan commitOutcome {
	ch := make(chan commitOutcome, 1)
	go func() {
		d, err := s.CommitAutomated(context.Background(), c, decide)
		ch <- commitOutcome{d, err}
	}()
	return ch
}

func awaitCommit(t *testing.T, ch <-chan commitOutcome) commitOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(15 * time.Second):
		t.Fatal("CommitAutomated did not return")
		return commitOutcome{}
	}
}

func claimByID(t *testing.T, s *Store, ids ...string) map[string]Claim {
	t.Helper()
	cs, err := s.ClaimPending(context.Background(), 100, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Claim{}
	for _, c := range cs {
		got[c.ID] = c
	}
	for _, id := range ids {
		if _, ok := got[id]; !ok {
			t.Fatalf("listing %s was not claimed (claimed %d)", id, len(cs))
		}
	}
	return got
}

// ---- claiming --------------------------------------------------------------

// Several claimers drain the queue at once. Every listing must be handed out
// exactly once, with attempts = 1 and the token that was returned.
func TestClaimConcurrentClaimersNeverShareAListing(t *testing.T) {
	s := migrated(t)
	const total, claimers = 60, 8
	want := map[string]bool{}
	for i := 0; i < total; i++ {
		want[newListing(t, s, content("s-1002", fmt.Sprintf("item %d", i)))] = true
	}

	var (
		mu      sync.Mutex
		claimed []Claim
		wg      sync.WaitGroup
		start   = make(chan struct{})
	)
	for w := 0; w < claimers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for {
				cs, err := s.ClaimPending(context.Background(), 4, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(cs) == 0 {
					return
				}
				mu.Lock()
				claimed = append(claimed, cs...)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(claimed) != total {
		t.Fatalf("claimed %d listings, want %d", len(claimed), total)
	}
	seen := map[string]bool{}
	tokens := map[string]bool{}
	for _, c := range claimed {
		if seen[c.ID] {
			t.Fatalf("listing %s was claimed twice", c.ID)
		}
		seen[c.ID] = true
		if !want[c.ID] {
			t.Fatalf("claimed unknown listing %s", c.ID)
		}
		if c.Attempts != 1 || c.LeaseToken == "" || tokens[c.LeaseToken] {
			t.Fatalf("bad claim: %+v", c)
		}
		tokens[c.LeaseToken] = true

		r := readRow(t, s, c.ID)
		if r.attempts != 1 || r.token == nil || *r.token != c.LeaseToken || r.expires == nil || !r.expires.After(time.Now()) {
			t.Fatalf("stored lease does not match the claim: %+v vs %+v", r, c)
		}
	}
}

// A row another transaction has locked is skipped, not waited for.
func TestClaimSkipsLockedRowsWithoutWaiting(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	locked := newListing(t, s, content("s-1002", "locked"))
	free := newListing(t, s, content("s-1002", "free"))

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT 1 FROM listings WHERE id=$1 FOR UPDATE`, locked); err != nil {
		t.Fatal(err)
	}

	done := make(chan []Claim, 1)
	go func() {
		cs, err := s.ClaimPending(ctx, 10, time.Minute)
		if err != nil {
			t.Error(err)
		}
		done <- cs
	}()
	select {
	case cs := <-done:
		if len(cs) != 1 || cs[0].ID != free {
			t.Fatalf("claimed %+v, want only %s", cs, free)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ClaimPending waited on a locked row instead of skipping it")
	}

	tx.Rollback(ctx)
	if cs, err := s.ClaimPending(ctx, 10, time.Minute); err != nil || len(cs) != 1 || cs[0].ID != locked {
		t.Fatalf("after the lock is gone: %+v %v", cs, err)
	}
}

func TestClaimIsOldestFirstLimitedAndPendingOnly(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	a := newListing(t, s, content("s-1002", "a"))
	b := newListing(t, s, content("s-1002", "b"))
	c := newListing(t, s, content("s-1002", "c"))
	d := newListing(t, s, content("s-1002", "d"))
	// c is already decided and must never be claimed.
	if _, err := s.pool.Exec(ctx, `UPDATE listings SET status='published', decision='{"outcome":"approved","reasons":[]}', decided_at=now() WHERE id=$1`, c); err != nil {
		t.Fatal(err)
	}

	first, err := s.ClaimPending(ctx, 2, time.Minute)
	if err != nil || len(first) != 2 {
		t.Fatalf("%v %v", first, err)
	}
	ids := []string{first[0].ID, first[1].ID}
	sort.Strings(ids)
	want := []string{a, b}
	sort.Strings(want)
	if ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("claimed %v, want the two oldest %v", ids, want)
	}
	rest, err := s.ClaimPending(ctx, 10, time.Minute)
	if err != nil || len(rest) != 1 || rest[0].ID != d {
		t.Fatalf("rest: %+v %v", rest, err)
	}
	if more, _ := s.ClaimPending(ctx, 10, time.Minute); len(more) != 0 {
		t.Fatalf("nothing should be claimable now, got %+v", more)
	}
	if r := readRow(t, s, c); r.attempts != 0 || r.token != nil {
		t.Fatalf("a decided listing was touched: %+v", r)
	}
}

func TestClaimReturnsTheListingContent(t *testing.T) {
	s := migrated(t)
	f := listing.Fields{SellerID: "s-1004", Title: "  Hàng ĐẸP ", Description: "d\nd", PriceVND: 1<<63 - 1, CategoryID: "c-laptops"}
	newListing(t, s, f)
	if c := claimOne(t, s); c.Fields != f || c.Attempts != 1 {
		t.Fatalf("%+v", c)
	}
}

// ---- reclaim ---------------------------------------------------------------

func TestClaimedListingIsNotClaimableUntilTheLeaseExpires(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	id := newListing(t, s, content("s-1002", "x"))

	first := claimOne(t, s)
	if more, _ := s.ClaimPending(ctx, 10, time.Minute); len(more) != 0 {
		t.Fatalf("a listing with a live lease was claimed again: %+v", more)
	}

	expireLease(t, s, id) // the worker died
	again := claimOne(t, s)
	if again.ID != id || again.Attempts != 2 {
		t.Fatalf("reclaim: %+v", again)
	}
	if again.LeaseToken == first.LeaseToken {
		t.Fatal("a reclaim must issue a new lease token")
	}
	if r := readRow(t, s, id); r.attempts != 2 || *r.token != again.LeaseToken {
		t.Fatalf("stored: %+v", r)
	}

	// Each expiry gives one more attempt, with a fresh token each time.
	expireLease(t, s, id)
	third := claimOne(t, s)
	if third.Attempts != 3 || third.LeaseToken == again.LeaseToken || third.LeaseToken == first.LeaseToken {
		t.Fatalf("third claim: %+v", third)
	}
}

// ---- recording failures ----------------------------------------------------

func TestRecordFailureBacksOffAndKeepsTheLeaseToken(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	id := newListing(t, s, content("s-1002", "x"))
	c := claimOne(t, s)

	if err := s.RecordFailure(ctx, id, c.LeaseToken, "database timeout", time.Hour); err != nil {
		t.Fatal(err)
	}
	r := readRow(t, s, id)
	if r.lastErr == nil || *r.lastErr != "database timeout" || r.token == nil || *r.token != c.LeaseToken {
		t.Fatalf("%+v", r)
	}
	if d := time.Until(*r.expires); d < 59*time.Minute || d > 61*time.Minute {
		t.Fatalf("backoff not applied: lease expires in %s", d)
	}
	if more, _ := s.ClaimPending(ctx, 10, time.Minute); len(more) != 0 {
		t.Fatal("listing is claimable during its backoff")
	}

	expireLease(t, s, id) // backoff over
	if again := claimOne(t, s); again.ID != id || again.Attempts != 2 {
		t.Fatalf("retry: %+v", again)
	}
}

func TestRecordFailureTruncatesLongErrors(t *testing.T) {
	s := migrated(t)
	id := newListing(t, s, content("s-1002", "x"))
	c := claimOne(t, s)
	long := ""
	for i := 0; i < 5000; i++ {
		long += "đ"
	}
	if err := s.RecordFailure(context.Background(), id, c.LeaseToken, long, time.Second); err != nil {
		t.Fatal(err)
	}
	if r := readRow(t, s, id); len([]rune(*r.lastErr)) != maxLastErrorRunes {
		t.Fatalf("stored %d runes", len([]rune(*r.lastErr)))
	}
}

// A worker whose lease expired and was taken over must not be able to change
// the new lease: not its backoff, not its error, not its token.
func TestStaleWorkerCannotTouchTheNewLease(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	id := newListing(t, s, content("s-1002", "x"))

	old := claimOne(t, s) // worker A
	expireLease(t, s, id)
	cur := claimOne(t, s) // worker B takes over
	if cur.LeaseToken == old.LeaseToken {
		t.Fatal("tokens must differ")
	}
	before := readRow(t, s, id)

	// A wakes up and reports its failure with its old token.
	err := s.RecordFailure(ctx, id, old.LeaseToken, "stale worker's error", 24*time.Hour)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("want ErrLeaseLost, got %v", err)
	}
	after := readRow(t, s, id)
	if after.token == nil || *after.token != cur.LeaseToken {
		t.Fatalf("the lease token was changed: %+v", after)
	}
	if after.lastErr != nil {
		t.Fatalf("stale worker's error was recorded: %q", *after.lastErr)
	}
	if !after.expires.Equal(*before.expires) {
		t.Fatalf("stale worker moved the lease expiry: %v -> %v", before.expires, after.expires)
	}

	// B, holding the current token, can.
	if err := s.RecordFailure(ctx, id, cur.LeaseToken, "current worker's error", time.Minute); err != nil {
		t.Fatal(err)
	}
	if r := readRow(t, s, id); r.lastErr == nil || *r.lastErr != "current worker's error" {
		t.Fatalf("%+v", r)
	}
}

func TestRecordFailureOnAFinishedOrUnknownListingIsLeaseLost(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	id := newListing(t, s, content("s-1002", "x"))
	c := claimOne(t, s)
	if _, err := s.CommitAutomated(ctx, c, decideWith(engine(t), c.Fields)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure(ctx, id, c.LeaseToken, "late", time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("after commit: %v", err)
	}
	if err := s.RecordFailure(ctx, "l-nope", c.LeaseToken, "x", time.Second); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := s.RecordFailure(ctx, id, "not-a-uuid", "x", time.Second); err == nil {
		t.Fatal("a malformed token must be an error, not a silent success")
	}
}

// ---- committing results: first commit wins ---------------------------------

func TestCommitStoresTheDecisionAndClearsTheLease(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	id := newListing(t, s, content("s-1004", "x")) // unverified: needs a human
	c := claimOne(t, s)

	d, err := s.CommitAutomated(ctx, c, decideWith(engine(t), c.Fields))
	if err != nil || d.Status != listing.StatusNeedsHuman {
		t.Fatalf("%+v %v", d, err)
	}
	l, err := s.GetListing(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != listing.StatusNeedsHuman || l.DecidedAt == nil || l.Decision == nil {
		t.Fatalf("%+v", l)
	}
	r := readRow(t, s, id)
	if r.token != nil || r.expires != nil {
		t.Fatalf("lease not cleared: %+v", r)
	}
	// The stored decision is the API shape.
	var stored struct {
		Outcome string `json:"outcome"`
		Reasons []struct {
			RuleID string `json:"rule_id"`
		} `json:"reasons"`
	}
	if err := jsonUnmarshal(l.Decision, &stored); err != nil || stored.Outcome != "escalated" ||
		len(stored.Reasons) != 1 || stored.Reasons[0].RuleID != "R5" {
		t.Fatalf("decision %s (%v)", l.Decision, err)
	}
}

// A stale attempt that is too late must not replace the committed result, and
// must not replace a human's resolution either.
func TestStaleResultCannotOverwriteACommittedResultOrAHumanDecision(t *testing.T) {
	s := migrated(t)
	ctx := context.Background()
	e := engine(t)
	id := newListing(t, s, content("s-1004", "x"))

	stale := claimOne(t, s) // worker A claims, then stalls
	expireLease(t, s, id)
	fresh := claimOne(t, s) // worker B takes over and commits

	if d, err := s.CommitAutomated(ctx, fresh, decideWith(e, fresh.Fields)); err != nil || d.Status != listing.StatusNeedsHuman {
		t.Fatalf("B: %+v %v", d, err)
	}
	// A moderator approves.
	if _, err := s.pool.Exec(ctx, `UPDATE listings SET status='published', decision='{"outcome":"approved","reasons":[{"rule_id":"HUMAN","message":"ok"}]}'::jsonb, decided_at=now() WHERE id=$1 AND status='needs_human'`, id); err != nil {
		t.Fatal(err)
	}
	before := readRow(t, s, id)

	// A finally commits a different result.
	_, err := s.CommitAutomated(ctx, stale, func(rules.DuplicateFinder) (rules.Decision, error) {
		return rules.Decision{Status: listing.StatusAutoRejected, Outcome: rules.OutcomeRejected,
			Reasons: []rules.Reason{{RuleID: "R1", Message: "from the stale attempt"}}}, nil
	})
	if !errors.Is(err, ErrNotPending) {
		t.Fatalf("want ErrNotPending, got %v", err)
	}
	after := readRow(t, s, id)
	if after.status != listing.StatusPublished || *after.decision != *before.decision {
		t.Fatalf("stale attempt changed the result: %+v -> %+v", before, after)
	}
}

func TestOnlyOneOfSeveralConcurrentAttemptsCommits(t *testing.T) {
	s := migrated(t)
	id := newListing(t, s, content("s-1002", "x"))
	c := claimOne(t, s)

	const n = 6
	var chans []<-chan commitOutcome
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("attempt %d", i)
		chans = append(chans, goCommit(s, c, func(rules.DuplicateFinder) (rules.Decision, error) {
			return rules.Decision{Status: listing.StatusPublished, Outcome: rules.OutcomeApproved,
				Reasons: []rules.Reason{{RuleID: "AUTO_PASS", Message: title}}}, nil
		}))
	}
	won := 0
	for _, ch := range chans {
		switch o := awaitCommit(t, ch); {
		case o.err == nil:
			won++
		case errors.Is(o.err, ErrNotPending):
		default:
			t.Fatal(o.err)
		}
	}
	if won != 1 {
		t.Fatalf("%d attempts committed a result, want exactly 1", won)
	}
	if r := readRow(t, s, id); r.status != listing.StatusPublished {
		t.Fatalf("%+v", r)
	}
}

// ---- duplicates inside the seller lock -------------------------------------

// Two identical listings are decided at the same moment. The first to commit
// is published; the second must see it and be rejected as its duplicate.
func TestDuplicatesRacingToCommitOnePublishesOneIsRejected(t *testing.T) {
	s := migrated(t)
	e := engine(t)
	f := content("s-1002", "same item")
	idA := newListing(t, s, f)
	idB := newListing(t, s, f)
	claims := claimByID(t, s, idA, idB)
	a, b := claims[idA], claims[idB]

	h := newHold(t, nil)
	s.hooks.beforeCommit = h.hook // first transaction to reach COMMIT is held

	first := goCommit(s, a, decideWith(e, a.Fields))
	holderPID := h.holderPID(t)

	// A has decided "published" but not committed. B must wait for the seller
	// lock, not decide on its own.
	second := goCommit(s, b, decideWith(e, b.Fields))
	waitBlockedBy(t, s, holderPID, 1)
	select {
	case <-second:
		t.Fatal("B decided while A held the seller lock")
	default:
	}

	h.release()
	oa, ob := awaitCommit(t, first), awaitCommit(t, second)
	if oa.err != nil || ob.err != nil {
		t.Fatalf("%v / %v", oa.err, ob.err)
	}
	if oa.d.Status != listing.StatusPublished {
		t.Fatalf("A: %+v", oa.d)
	}
	if ob.d.Status != listing.StatusAutoRejected || ob.d.Reasons[0].RuleID != "R3" ||
		ob.d.Reasons[0].Detail["duplicate_of"] != idA {
		t.Fatalf("B should be R3, duplicate of A: %+v", ob.d)
	}
	if countByStatus(t, s, listing.StatusPublished) != 1 || countByStatus(t, s, listing.StatusAutoRejected) != 1 {
		t.Fatal("expected exactly one published and one auto_rejected")
	}
}

// "First valid commit wins" means the first to commit, not the first submitted.
func TestDuplicateThatCommitsFirstWinsEvenIfSubmittedSecond(t *testing.T) {
	s := migrated(t)
	e := engine(t)
	f := content("s-1002", "same item")
	idA := newListing(t, s, f) // submitted first
	idB := newListing(t, s, f) // submitted second
	claims := claimByID(t, s, idA, idB)

	if d, err := s.CommitAutomated(context.Background(), claims[idB], decideWith(e, claims[idB].Fields)); err != nil || d.Status != listing.StatusPublished {
		t.Fatalf("B: %+v %v", d, err)
	}
	d, err := s.CommitAutomated(context.Background(), claims[idA], decideWith(e, claims[idA].Fields))
	if err != nil || d.Status != listing.StatusAutoRejected || d.Reasons[0].Detail["duplicate_of"] != idB {
		t.Fatalf("A should be rejected as a duplicate of B: %+v %v", d, err)
	}
}

// If the first transaction rolls back, nothing it did counts: the second is
// not a duplicate of an uncommitted listing.
func TestDuplicateOfAnUncommittedListingIsNotADuplicate(t *testing.T) {
	s := migrated(t)
	e := engine(t)
	f := content("s-1002", "same item")
	idA := newListing(t, s, f)
	idB := newListing(t, s, f)
	claims := claimByID(t, s, idA, idB)

	h := newHold(t, errors.New("injected failure before commit"))
	s.hooks.beforeCommit = h.hook

	first := goCommit(s, claims[idA], decideWith(e, claims[idA].Fields))
	pid := h.holderPID(t)
	second := goCommit(s, claims[idB], decideWith(e, claims[idB].Fields))
	waitBlockedBy(t, s, pid, 1)
	h.release()

	if o := awaitCommit(t, first); o.err == nil {
		t.Fatal("A should have failed")
	}
	ob := awaitCommit(t, second)
	if ob.err != nil || ob.d.Status != listing.StatusPublished {
		t.Fatalf("B should be published, got %+v %v", ob.d, ob.err)
	}
	if r := readRow(t, s, idA); r.status != listing.StatusPending {
		t.Fatalf("A must still be pending: %+v", r)
	}
	// A is retried once its lease runs out, and now it does find B.
	expireLease(t, s, idA)
	retry := claimByID(t, s, idA)[idA]
	if retry.Attempts != 2 {
		t.Fatalf("attempts = %d", retry.Attempts)
	}
	d, err := s.CommitAutomated(context.Background(), retry, decideWith(e, retry.Fields))
	if err != nil || d.Status != listing.StatusAutoRejected || d.Reasons[0].Detail["duplicate_of"] != idB {
		t.Fatalf("A's retry should be a duplicate of B: %+v %v", d, err)
	}
}

// Different sellers use different locks and never wait for each other.
func TestDifferentSellersDoNotBlockEachOther(t *testing.T) {
	s := migrated(t)
	e := engine(t)
	idA := newListing(t, s, content("s-1002", "same item"))
	idB := newListing(t, s, content("s-1003", "same item")) // same content, other seller
	claims := claimByID(t, s, idA, idB)

	h := newHold(t, nil)
	s.hooks.beforeCommit = h.hook
	first := goCommit(s, claims[idA], decideWith(e, claims[idA].Fields))
	h.holderPID(t) // s-1002's commit is held inside its lock

	done := goCommit(s, claims[idB], decideWith(e, claims[idB].Fields))
	o := awaitCommit(t, done) // must finish while s-1002 is still held
	if o.err != nil || o.d.Status != listing.StatusPublished {
		t.Fatalf("%+v %v", o.d, o.err)
	}
	h.release()
	if o := awaitCommit(t, first); o.err != nil || o.d.Status != listing.StatusPublished {
		t.Fatalf("%+v %v", o.d, o.err)
	}
	// The same content from two sellers is not a duplicate.
	if countByStatus(t, s, listing.StatusPublished) != 2 {
		t.Fatal("both sellers' listings should be published")
	}
}

// Which statuses make a listing "active" for R3.
func TestDuplicateLookupSeesOnlyActiveListings(t *testing.T) {
	cases := []struct {
		status listing.Status
		active bool
	}{
		{listing.StatusAutoApproved, true},
		{listing.StatusNeedsHuman, true},
		{listing.StatusPublished, true},
		{listing.StatusAutoRejected, false},
		{listing.StatusRejected, false},
		{listing.StatusExpired, false},
		{listing.StatusPending, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			s := migrated(t)
			f := content("s-1002", "same item")
			other := newListing(t, s, f)
			mine := newListing(t, s, f)
			if tc.status != listing.StatusPending {
				if _, err := s.pool.Exec(context.Background(),
					`UPDATE listings SET status=$2, decision='{"outcome":"approved","reasons":[]}'::jsonb, decided_at=now() WHERE id=$1`,
					other, string(tc.status)); err != nil {
					t.Fatal(err)
				}
			}
			c := claimByID(t, s, mine)[mine]

			var found string
			var ok bool
			_, err := s.CommitAutomated(context.Background(), c, func(find rules.DuplicateFinder) (rules.Decision, error) {
				var err error
				found, ok, err = find()
				return rules.Decision{Status: listing.StatusPublished, Outcome: rules.OutcomeApproved,
					Reasons: []rules.Reason{{RuleID: "AUTO_PASS", Message: "x"}}}, err
			})
			// A published decision for `mine` is itself a duplicate of an active `other`;
			// the index would stop it. Only the lookup result matters here.
			if tc.active {
				if !ok || found != other {
					t.Fatalf("duplicate not found: %q %v (commit err %v)", found, ok, err)
				}
				return
			}
			if ok {
				t.Fatalf("a %s listing was reported as a duplicate", tc.status)
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The duplicate lookup never reports the listing itself.
func TestDuplicateLookupIgnoresTheListingItself(t *testing.T) {
	s := migrated(t)
	newListing(t, s, content("s-1002", "only one"))
	c := claimOne(t, s)
	_, err := s.CommitAutomated(context.Background(), c, func(find rules.DuplicateFinder) (rules.Decision, error) {
		if id, ok, err := find(); err != nil || ok {
			t.Errorf("found %q, %v, %v", id, ok, err)
		}
		return rules.Decision{Status: listing.StatusPublished, Outcome: rules.OutcomeApproved,
			Reasons: []rules.Reason{{RuleID: "AUTO_PASS", Message: "x"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Safety net: if something writes an active duplicate without taking the
// seller lock, the unique index stops this commit and the retry sees it.
func TestUniqueIndexCatchesAWriterThatSkippedTheLock(t *testing.T) {
	s := migrated(t)
	e := engine(t)
	ctx := context.Background()
	f := content("s-1002", "same item")
	idA := newListing(t, s, f)
	newListing(t, s, f) // idB, the rogue
	c := claimByID(t, s, idA)[idA]

	var rogue string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM listings WHERE id <> $1`, idA).Scan(&rogue); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	s.hooks.afterDecide = func(_ uint32, attempt int) error {
		attempts = attempt
		if attempt == 1 {
			// Bypass the lock: make the other listing active behind this transaction's back.
			_, err := s.pool.Exec(ctx, `UPDATE listings SET status='published', decision='{"outcome":"approved","reasons":[]}'::jsonb, decided_at=now() WHERE id=$1`, rogue)
			return err
		}
		return nil
	}

	d, err := s.CommitAutomated(ctx, c, decideWith(e, c.Fields))
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected one redo, ran %d attempts", attempts)
	}
	if d.Status != listing.StatusAutoRejected || d.Reasons[0].RuleID != "R3" || d.Reasons[0].Detail["duplicate_of"] != rogue {
		t.Fatalf("the redo should have found the duplicate: %+v", d)
	}
	if countByStatus(t, s, listing.StatusPublished) != 1 {
		t.Fatal("two active duplicates exist")
	}
}

// Waiting too long for the seller lock is reported, not hung on.
func TestCommitWaitingTooLongForTheSellerLockIsBusy(t *testing.T) {
	s := migrated(t)
	s.lockTimeout = 300 * time.Millisecond
	e := engine(t)
	idA := newListing(t, s, content("s-1002", "one"))
	idB := newListing(t, s, content("s-1002", "two"))
	claims := claimByID(t, s, idA, idB)

	h := newHold(t, nil)
	s.hooks.beforeCommit = h.hook
	first := goCommit(s, claims[idA], decideWith(e, claims[idA].Fields))
	h.holderPID(t)

	o := awaitCommit(t, goCommit(s, claims[idB], decideWith(e, claims[idB].Fields)))
	if !errors.Is(o.err, ErrBusy) {
		t.Fatalf("want ErrBusy, got %+v", o)
	}
	if r := readRow(t, s, idB); r.status != listing.StatusPending {
		t.Fatalf("B must stay pending so it is retried: %+v", r)
	}
	h.release()
	awaitCommit(t, first)
}

// An error from decide (for example the duplicate lookup failing) leaves the
// listing pending, with nothing written.
func TestCommitWhenDecideFailsLeavesTheListingPending(t *testing.T) {
	s := migrated(t)
	id := newListing(t, s, content("s-1002", "x"))
	c := claimOne(t, s)
	boom := errors.New("rule engine failed")
	_, err := s.CommitAutomated(context.Background(), c, func(rules.DuplicateFinder) (rules.Decision, error) {
		return rules.Decision{}, boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if r := readRow(t, s, id); r.status != listing.StatusPending || r.decision != nil {
		t.Fatalf("%+v", r)
	}
	// And a decision that is not a final result is refused.
	_, err = s.CommitAutomated(context.Background(), c, func(rules.DuplicateFinder) (rules.Decision, error) {
		return rules.Decision{Status: listing.StatusPending, Outcome: rules.OutcomeApproved}, nil
	})
	if err == nil {
		t.Fatal("a pending decision must be refused")
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
