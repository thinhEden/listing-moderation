package worker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/rules"
	"listing-moderation/internal/seed"
	"listing-moderation/internal/store"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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

var testCfg = Config{
	Workers:      2,
	PollInterval: 5 * time.Millisecond,
	Lease:        time.Second,
	MaxAttempts:  3,
	BackoffBase:  time.Second,
	BackoffMax:   30 * time.Second,
}

// fakeStore stands in for the database so the worker's own logic can be tested
// precisely. The real transactions are tested against Postgres in the store package.
type fakeStore struct {
	mu          sync.Mutex
	queue       []store.Claim
	dupOf       string // when set, the duplicate lookup finds this listing
	commitErr   error
	recordErr   error
	block       chan struct{} // when set, CommitAutomated waits for it (or for ctx)
	commitCalls int
	decisions   map[string]rules.Decision
	failures    []failure
	hardCtxDone bool
}

type failure struct {
	id, token, message string
	backoff            time.Duration
}

func (f *fakeStore) ClaimPending(_ context.Context, _ int, _ time.Duration) ([]store.Claim, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queue) == 0 {
		return nil, nil
	}
	c := f.queue[0]
	f.queue = f.queue[1:]
	return []store.Claim{c}, nil
}

func (f *fakeStore) CommitAutomated(ctx context.Context, c store.Claim, decide store.Decide) (rules.Decision, error) {
	f.mu.Lock()
	f.commitCalls++
	block, err := f.block, f.commitErr
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			f.mu.Lock()
			f.hardCtxDone = true
			f.mu.Unlock()
			return rules.Decision{}, ctx.Err()
		}
	}
	if err != nil {
		return rules.Decision{}, err
	}
	d, err := decide(func() (string, bool, error) { return f.dupOf, f.dupOf != "", nil })
	if err != nil {
		return rules.Decision{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.decisions == nil {
		f.decisions = map[string]rules.Decision{}
	}
	f.decisions[c.ID] = d
	return d, nil
}

func (f *fakeStore) RecordFailure(_ context.Context, id, token, message string, backoff time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures = append(f.failures, failure{id, token, message, backoff})
	return f.recordErr
}

func claim(seller, title string, attempts int, lastErr string) store.Claim {
	return store.Claim{
		ID: "l-1", Attempts: attempts, LeaseToken: "tok-" + title, LastError: lastErr,
		Fields: listing.Fields{SellerID: seller, Title: title, Description: "d", PriceVND: 6_000_000, CategoryID: "c-phones"},
	}
}

func newPool(t *testing.T, fs *fakeStore, opts ...Option) *Pool {
	t.Helper()
	return New(testCfg, fs, engine(t), quiet(), opts...)
}

// ---- backoff ---------------------------------------------------------------

func TestBackoffDoublesFromTheBaseAndIsCapped(t *testing.T) {
	c := Config{BackoffBase: time.Second, BackoffMax: 30 * time.Second}
	want := []time.Duration{1, 2, 4, 8, 16, 30, 30, 30}
	for i, w := range want {
		if got := c.Backoff(i + 1); got != w*time.Second {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, w*time.Second)
		}
	}
	// A cap below the base is respected rather than exceeded.
	if got := (Config{BackoffBase: time.Minute, BackoffMax: time.Second}).Backoff(1); got != time.Second {
		t.Errorf("capped base: %s", got)
	}
	if got := c.Backoff(1000); got != 30*time.Second {
		t.Errorf("huge attempt count: %s", got)
	}
}

// ---- the normal path -------------------------------------------------------

func TestNormalAttemptUsesTheFullRuleOrder(t *testing.T) {
	cases := []struct {
		name   string
		claim  store.Claim
		status listing.Status
		rule   string
	}{
		{"clean listing", claim("s-1002", "iPhone", 1, ""), listing.StatusPublished, "AUTO_PASS"},
		{"trusted seller", claim("s-1001", "iPhone", 1, ""), listing.StatusPublished, "R4"},
		{"unverified seller", claim("s-1004", "iPhone", 1, ""), listing.StatusNeedsHuman, "R5"},
		{"banned term", claim("s-1001", "iPhone replica", 1, ""), listing.StatusAutoRejected, "R1"},
		// The last normal attempt is still normal.
		{"attempt == MaxAttempts is not yet exhausted", claim("s-1001", "iPhone", testCfg.MaxAttempts, ""), listing.StatusPublished, "R4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			newPool(t, fs).handle(tc.claim)
			d := fs.decisions[tc.claim.ID]
			if d.Status != tc.status || d.Reasons[0].RuleID != tc.rule {
				t.Fatalf("%+v", d)
			}
			if len(fs.failures) != 0 {
				t.Fatalf("unexpected failures: %+v", fs.failures)
			}
		})
	}
}

func TestNormalAttemptDuplicateIsRejectedForEverySeller(t *testing.T) {
	for _, seller := range []string{"s-1001", "s-1002", "s-1004"} {
		fs := &fakeStore{dupOf: "l-other"}
		newPool(t, fs).handle(claim(seller, "iPhone", 1, ""))
		d := fs.decisions["l-1"]
		if d.Status != listing.StatusAutoRejected || d.Reasons[0].RuleID != "R3" {
			t.Fatalf("%s: %+v", seller, d)
		}
	}
}

// ---- failures --------------------------------------------------------------

func TestCommitFailureRecordsTheErrorAndBackoffWithTheLeaseToken(t *testing.T) {
	fs := &fakeStore{commitErr: errors.New("connection reset by peer")}
	c := claim("s-1002", "iPhone", 3, "")
	newPool(t, fs).handle(c)

	if len(fs.failures) != 1 {
		t.Fatalf("failures: %+v", fs.failures)
	}
	f := fs.failures[0]
	if f.id != c.ID || f.token != c.LeaseToken || !strings.Contains(f.message, "connection reset") {
		t.Fatalf("%+v", f)
	}
	if f.backoff != testCfg.Backoff(3) || f.backoff != 4*time.Second {
		t.Fatalf("backoff %s, want %s", f.backoff, testCfg.Backoff(3))
	}
	if len(fs.decisions) != 0 {
		t.Fatal("a failed commit produced a decision")
	}
}

// If someone else already committed, there is nothing to retry or to report.
func TestAlreadyDecidedIsNotAFailure(t *testing.T) {
	fs := &fakeStore{commitErr: store.ErrNotPending}
	newPool(t, fs).handle(claim("s-1002", "iPhone", 1, ""))
	if len(fs.failures) != 0 {
		t.Fatalf("ErrNotPending was recorded as a failure: %+v", fs.failures)
	}
}

func TestLostLeaseWhileRecordingAFailureIsTolerated(t *testing.T) {
	fs := &fakeStore{commitErr: errors.New("boom"), recordErr: store.ErrLeaseLost}
	newPool(t, fs).handle(claim("s-1002", "iPhone", 1, "")) // must not panic or loop
	if len(fs.failures) != 1 {
		t.Fatalf("%+v", fs.failures)
	}
}

func TestAPanicInsideAnAttemptIsRecordedAsAFailure(t *testing.T) {
	fs := &fakeStore{}
	p := newPool(t, fs, WithScreener(func(listing.Fields) ([]rules.Match, error) { panic("rule exploded") }))
	p.handle(claim("s-1002", "iPhone", 1, ""))
	if len(fs.failures) != 1 || !strings.Contains(fs.failures[0].message, "rule exploded") {
		t.Fatalf("%+v", fs.failures)
	}
}

// ---- R1 failing ------------------------------------------------------------

// If R1 cannot run, nothing may be committed, at any attempt number. In
// particular the retries-exhausted path must not escalate or approve unscreened.
func TestScreeningFailureNeverCommitsAnything(t *testing.T) {
	broken := WithScreener(func(listing.Fields) ([]rules.Match, error) { return nil, errors.New("matcher unavailable") })
	for _, attempts := range []int{1, 2, testCfg.MaxAttempts, testCfg.MaxAttempts + 1, testCfg.MaxAttempts + 10} {
		fs := &fakeStore{}
		newPool(t, fs, broken).handle(claim("s-1001", "iPhone", attempts, ""))
		if fs.commitCalls != 0 {
			t.Fatalf("attempt %d: committed without a successful R1 screening", attempts)
		}
		if len(fs.failures) != 1 || !strings.Contains(fs.failures[0].message, "R1 screening failed") ||
			!strings.Contains(fs.failures[0].message, "matcher unavailable") {
			t.Fatalf("attempt %d: %+v", attempts, fs.failures)
		}
		if fs.failures[0].backoff != testCfg.Backoff(attempts) {
			t.Fatalf("attempt %d: backoff %s", attempts, fs.failures[0].backoff)
		}
	}
}

// ---- retries exhausted -----------------------------------------------------

func TestExhaustedAttemptStillRejectsBannedContent(t *testing.T) {
	for _, seller := range []string{"s-1001", "s-1002", "s-1004"} {
		fs := &fakeStore{}
		c := claim(seller, "iPhone replica", testCfg.MaxAttempts+1, "commit: connection reset")
		newPool(t, fs).handle(c)
		d := fs.decisions[c.ID]
		if d.Status != listing.StatusAutoRejected || len(d.Reasons) != 1 || d.Reasons[0].RuleID != "R1" {
			t.Fatalf("%s: banned content after retries must still be rejected by R1: %+v", seller, d)
		}
	}
}

func TestExhaustedAttemptEscalatesCleanListingsWithTheStoredError(t *testing.T) {
	// Every seller, including a trusted one: after retries nobody gets a shortcut.
	for _, seller := range []string{"s-1001", "s-1002", "s-1004"} {
		fs := &fakeStore{}
		c := claim(seller, "iPhone", testCfg.MaxAttempts+1, "commit: connection reset by peer")
		newPool(t, fs).handle(c)
		d := fs.decisions[c.ID]
		if d.Status != listing.StatusNeedsHuman || d.Outcome != rules.OutcomeEscalated || len(d.Reasons) != 1 || d.Reasons[0].RuleID != "SYS_EVAL_FAILED" {
			t.Fatalf("%s: %+v", seller, d)
		}
		r := d.Reasons[0]
		if !strings.Contains(r.Message, "connection reset by peer") || !strings.Contains(r.Message, "3 failed attempts") {
			t.Fatalf("message should carry the stored error and the count: %q", r.Message)
		}
		if r.Detail["last_error"] != "commit: connection reset by peer" || r.Detail["failed_attempts"] != testCfg.MaxAttempts {
			t.Fatalf("detail %v", r.Detail)
		}
	}
}

func TestExhaustedAttemptRejectsADuplicateInsteadOfEscalating(t *testing.T) {
	fs := &fakeStore{dupOf: "l-active"}
	c := claim("s-1002", "iPhone", testCfg.MaxAttempts+1, "x")
	newPool(t, fs).handle(c)
	d := fs.decisions[c.ID]
	if d.Status != listing.StatusAutoRejected || d.Reasons[0].RuleID != "R3" || d.Reasons[0].Detail["duplicate_of"] != "l-active" {
		t.Fatalf("%+v", d)
	}
}

// If even the escalation cannot be committed, the listing stays pending and is
// retried, and the operator is told by the error log; it is never dropped.
func TestExhaustedAttemptThatFailsToCommitIsRetried(t *testing.T) {
	fs := &fakeStore{commitErr: errors.New("database is down")}
	c := claim("s-1002", "iPhone", testCfg.MaxAttempts+2, "")
	newPool(t, fs).handle(c)
	if len(fs.failures) != 1 || len(fs.decisions) != 0 {
		t.Fatalf("failures %+v decisions %+v", fs.failures, fs.decisions)
	}
	if fs.failures[0].backoff != testCfg.Backoff(c.Attempts) {
		t.Fatalf("backoff %s", fs.failures[0].backoff)
	}
}

// ---- the pool --------------------------------------------------------------

func TestPoolProcessesQueuedListingsAndShutsDown(t *testing.T) {
	fs := &fakeStore{}
	for i := 0; i < 10; i++ {
		c := claim("s-1002", "iPhone", 1, "")
		c.ID = "l-" + string(rune('a'+i))
		fs.queue = append(fs.queue, c)
	}
	p := newPool(t, fs)
	p.Start()
	deadline := time.Now().Add(5 * time.Second)
	for {
		fs.mu.Lock()
		n := len(fs.decisions)
		fs.mu.Unlock()
		if n == 10 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of 10 listings were processed", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestShutdownWaitsForTheListingInFlight(t *testing.T) {
	fs := &fakeStore{block: make(chan struct{})}
	fs.queue = []store.Claim{claim("s-1002", "iPhone", 1, "")}
	p := newPool(t, fs)
	p.Start()
	waitFor(t, func() bool { fs.mu.Lock(); defer fs.mu.Unlock(); return fs.commitCalls == 1 })

	done := make(chan error, 1)
	go func() { done <- p.Shutdown(context.Background()) }()
	select {
	case <-done:
		t.Fatal("Shutdown returned while a listing was still being committed")
	case <-time.After(100 * time.Millisecond):
	}

	close(fs.block) // the commit finishes
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := fs.decisions["l-1"]; !ok {
		t.Fatal("the in-flight listing was not completed")
	}
}

func TestShutdownGivesUpAfterTheDeadlineAndCancelsInFlightWork(t *testing.T) {
	fs := &fakeStore{block: make(chan struct{})} // never released
	fs.queue = []store.Claim{claim("s-1002", "iPhone", 1, "")}
	p := newPool(t, fs)
	p.Start()
	waitFor(t, func() bool { fs.mu.Lock(); defer fs.mu.Unlock(); return fs.commitCalls == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Shutdown(ctx); err == nil {
		t.Fatal("expected an error when the deadline passes")
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if !fs.hardCtxDone {
		t.Fatal("in-flight database work was not cancelled")
	}
	if len(fs.decisions) != 0 {
		t.Fatal("a cancelled attempt produced a decision")
	}
}

func TestShutdownTwiceIsHarmless(t *testing.T) {
	p := newPool(t, &fakeStore{})
	p.Start()
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ---- readiness -------------------------------------------------------------

func TestCheckTracksThePoolsLifecycle(t *testing.T) {
	p := newPool(t, &fakeStore{})
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("a pool that was never started must not be ready")
	}
	p.Start()
	waitFor(t, func() bool { return p.Check(context.Background()) == nil })
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(context.Background()); err == nil {
		t.Fatal("a stopped pool must not be ready")
	}
}

func TestCheckFailsWhenAWorkerStopsMakingProgress(t *testing.T) {
	p := newPool(t, &fakeStore{})
	p.Start()
	defer p.Shutdown(context.Background())
	waitFor(t, func() bool { return p.Check(context.Background()) == nil })

	// Pretend worker 1 has been wedged for longer than two leases.
	p.beats[1].Store(time.Now().Add(-3 * testCfg.Lease).UnixNano())
	err := p.Check(context.Background())
	if err == nil || !strings.Contains(err.Error(), "worker 1") {
		t.Fatalf("got %v", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(2 * time.Millisecond)
	}
}
