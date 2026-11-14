package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/rules"
	"listing-moderation/internal/store"
	"listing-moderation/internal/testdb"
	"listing-moderation/internal/worker"
)

// These tests run the whole service as a client sees it: real HTTP, real
// handlers, real worker pool, real Postgres. They poll for results the way a
// seller's client would.

// ---- harness ---------------------------------------------------------------

// flowCfg is the production retry policy with the clocks sped up.
var flowCfg = worker.Config{
	Workers:      4,
	PollInterval: 10 * time.Millisecond,
	Lease:        10 * time.Second,
	MaxAttempts:  3,
	BackoffBase:  10 * time.Millisecond,
	BackoffMax:   40 * time.Millisecond,
}

type flowOpts struct {
	noWorkers  bool
	failCommit func(c store.Claim) error // injected before the real commit
	screener   worker.Screener           // replaces R1 screening
}

type flow struct {
	url  string // .../v1/listings
	base string
	dsn  string
	pool *worker.Pool
}

// faultyStore is the real store with a fault injected in front of the commit.
type faultyStore struct {
	*store.Store
	failCommit func(c store.Claim) error
}

func (f faultyStore) CommitAutomated(ctx context.Context, c store.Claim, decide store.Decide) (rules.Decision, error) {
	if f.failCommit != nil {
		if err := f.failCommit(c); err != nil {
			return rules.Decision{}, err
		}
	}
	return f.Store.CommitAutomated(ctx, c, decide)
}

func startFlow(t *testing.T, o flowOpts) *flow {
	t.Helper()
	dsn := testdb.IsolatedDSN(t)
	st, err := store.Open(context.Background(), dsn, 10*time.Second, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	data := realSeed(t)
	engine, err := rules.NewEngine(data)
	if err != nil {
		t.Fatal(err)
	}
	var opts []worker.Option
	if o.screener != nil {
		opts = append(opts, worker.WithScreener(o.screener))
	}
	pool := worker.New(flowCfg, faultyStore{Store: st, failCommit: o.failCommit}, engine, quiet(), opts...)

	srv := httptest.NewServer(NewHandler(Deps{
		Log: quiet(), Seed: data, Store: st,
		Readiness: []Check{{Name: "database", Fn: st.Ping}, {Name: "worker", Fn: pool.Check}},
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = pool.Shutdown(ctx)
	})
	if !o.noWorkers {
		pool.Start()
	}
	return &flow{url: srv.URL + "/v1/listings", base: srv.URL, dsn: dsn, pool: pool}
}

func bodyIn(seller, title, category string, price int) string {
	return fmt.Sprintf(`{"seller_id":%q,"title":%q,"description":"same description","price_vnd":%d,"category_id":%q}`,
		seller, title, price, category)
}

// submit posts a listing and returns its id; it must be accepted as pending.
func (f *flow) submit(t *testing.T, seller, title, category string, price int) string {
	t.Helper()
	r := send(t, "POST", f.url, bodyIn(seller, title, category, price))
	if r.code != 202 || r.body["status"] != "pending" {
		t.Fatalf("submit: %d %s", r.code, r.raw)
	}
	return r.body["id"].(string)
}

func (f *flow) get(t *testing.T, id string) map[string]any {
	t.Helper()
	r := send(t, "GET", f.url+"/"+id, "")
	if r.code != 200 {
		t.Fatalf("get %s: %d %s", id, r.code, r.raw)
	}
	return r.body
}

// await polls until the listing has left pending, as a client would.
func (f *flow) await(t *testing.T, id string) map[string]any {
	t.Helper()
	return f.awaitStatus(t, id, func(s string) bool { return s != "pending" })
}

func (f *flow) awaitStatus(t *testing.T, id string, ok func(string) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		l := f.get(t, id)
		if ok(l["status"].(string)) {
			return l
		}
		if time.Now().After(deadline) {
			t.Fatalf("listing %s still %v after 20s", id, l["status"])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *flow) decide(t *testing.T, id, decision, moderator, note string) reply {
	t.Helper()
	b := fmt.Sprintf(`{"decision":%q,"moderator_id":%q}`, decision, moderator)
	if note != "" {
		b = fmt.Sprintf(`{"decision":%q,"moderator_id":%q,"note":%q}`, decision, moderator, note)
	}
	return send(t, "POST", f.url+"/"+id+"/decision", b)
}

func reasonsOf(l map[string]any) []map[string]any {
	dec, ok := l["decision"].(map[string]any)
	if !ok {
		return nil
	}
	var out []map[string]any
	for _, r := range dec["reasons"].([]any) {
		out = append(out, r.(map[string]any))
	}
	return out
}

func ruleIDsOf(l map[string]any) []string {
	var ids []string
	for _, r := range reasonsOf(l) {
		ids = append(ids, r["rule_id"].(string))
	}
	return ids
}

func outcomeOf(l map[string]any) string {
	if dec, ok := l["decision"].(map[string]any); ok {
		return dec["outcome"].(string)
	}
	return ""
}

// dbScan reads straight from the database, for facts the API does not expose.
func (f *flow) dbScan(t *testing.T, query string, args []any, dest ...any) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	if err := c.QueryRow(ctx, query, args...).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

func (f *flow) attemptsAndError(t *testing.T, id string) (int, string) {
	var attempts int
	var lastErr *string
	f.dbScan(t, `SELECT attempts, last_error FROM listings WHERE id=$1`, []any{id}, &attempts, &lastErr)
	if lastErr == nil {
		return attempts, ""
	}
	return attempts, *lastErr
}

func eqStrings(got, want []string) bool { return strings.Join(got, ",") == strings.Join(want, ",") }

// ---- POST -> poll -> result ------------------------------------------------

func TestFlowSubmitThenPollForTheResult(t *testing.T) {
	f := startFlow(t, flowOpts{})
	cases := []struct {
		name            string
		seller, title   string
		category        string
		price           int
		status, outcome string
		rules           []string
		messageHas      string
	}{
		{"ordinary seller, clean", "s-1002", "iPhone 13", "c-phones", 6_000_000, "published", "approved", []string{"AUTO_PASS"}, ""},
		{"trusted seller", "s-1001", "iPhone 13", "c-phones", 6_000_000, "published", "approved", []string{"R4"}, "not applied"},
		{"trusted but unverified skips R5", "s-1005", "iPhone 13", "c-phones", 6_000_000, "published", "approved", []string{"R4"}, ""},
		{"trusted seller, absurd price, skips R2", "s-1006", "iPhone 13", "c-phones", 90_000_000, "published", "approved", []string{"R4"}, ""},
		{"banned term", "s-1002", "iPhone REPLICA", "c-phones", 6_000_000, "auto_rejected", "rejected", []string{"R1"}, `"replica"`},
		{"trusted seller cannot bypass R1", "s-1001", "iPhone replica", "c-phones", 6_000_000, "auto_rejected", "rejected", []string{"R1"}, `"replica"`},
		{"R1 hides R5", "s-1008", "hang gia", "c-fashion", 450_000, "auto_rejected", "rejected", []string{"R1"}, `"hang gia"`},
		{"unverified seller", "s-1004", "iPhone 13", "c-phones", 6_000_000, "needs_human", "escalated", []string{"R5"}, "not verified"},
		{"price anomaly and unverified", "s-1004", "iPhone cheap", "c-phones", 1_900_000, "needs_human", "escalated", []string{"R2", "R5"}, ""},
		{"exactly 3x median", "s-1003", "Wave", "c-motorbikes", 96_000_000, "published", "approved", []string{"AUTO_PASS"}, ""},
		{"one over 3x median", "s-1003", "Wave 2", "c-motorbikes", 96_000_001, "needs_human", "escalated", []string{"R2"}, "more than 3x"},
		{"below one third", "s-1003", "Wave 3", "c-motorbikes", 10_666_666, "needs_human", "escalated", []string{"R2"}, "one third"},
		{"free", "s-1002", "Gift", "c-fashion", 0, "needs_human", "escalated", []string{"R2"}, ""},
	}
	ids := make([]string, len(cases))
	for i, tc := range cases {
		ids[i] = f.submit(t, tc.seller, tc.title, tc.category, tc.price)
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := f.await(t, ids[i])
			if l["status"] != tc.status || outcomeOf(l) != tc.outcome || !eqStrings(ruleIDsOf(l), tc.rules) {
				t.Fatalf("status %v outcome %s rules %v, want %s %s %v", l["status"], outcomeOf(l), ruleIDsOf(l), tc.status, tc.outcome, tc.rules)
			}
			if l["decided_at"] == nil || l["created_at"] == nil {
				t.Fatalf("timestamps: %v", l)
			}
			for _, r := range reasonsOf(l) {
				if r["message"].(string) == "" {
					t.Fatalf("empty message: %v", r)
				}
			}
			if tc.messageHas != "" && !strings.Contains(reasonsOf(l)[0]["message"].(string), tc.messageHas) {
				t.Fatalf("message %q should contain %q", reasonsOf(l)[0]["message"], tc.messageHas)
			}
		})
	}
	// The API never shows auto_approved: approval and publication are one step.
	if r := send(t, "GET", f.url+"?status=auto_approved", ""); len(r.body["items"].([]any)) != 0 {
		t.Fatalf("auto_approved should never be stored: %s", r.raw)
	}
}

func TestFlowReplayAfterTheResultReportsTheCurrentStatus(t *testing.T) {
	f := startFlow(t, flowOpts{})
	first := send(t, "POST", f.url, bodyIn("s-1002", "iPhone", "c-phones", 6_000_000), "Idempotency-Key", "k1")
	if first.code != 202 {
		t.Fatal(first.raw)
	}
	id := first.body["id"].(string)
	f.await(t, id)

	replay := send(t, "POST", f.url, bodyIn("s-1002", "iPhone", "c-phones", 6_000_000), "Idempotency-Key", "k1")
	if replay.code != 200 || replay.body["id"] != id || replay.body["status"] != "published" {
		t.Fatalf("%d %s", replay.code, replay.raw)
	}
}

func TestFlowListingsSubmittedBeforeTheWorkersStartAreProcessedWhenTheyDo(t *testing.T) {
	f := startFlow(t, flowOpts{noWorkers: true})
	ids := []string{
		f.submit(t, "s-1002", "iPhone", "c-phones", 6_000_000),
		f.submit(t, "s-1004", "iPhone", "c-phones", 6_000_000),
	}
	time.Sleep(100 * time.Millisecond)
	for _, id := range ids {
		if f.get(t, id)["status"] != "pending" {
			t.Fatal("listing was processed without workers")
		}
	}
	f.pool.Start()
	if f.await(t, ids[0])["status"] != "published" || f.await(t, ids[1])["status"] != "needs_human" {
		t.Fatal("pending listings were not picked up after the workers started")
	}
}

// ---- duplicates ------------------------------------------------------------

// Identical listings are submitted together and decided by competing workers.
// For every item exactly one survives and the others point at it.
func TestFlowIdenticalListingsLeaveExactlyOneActive(t *testing.T) {
	f := startFlow(t, flowOpts{})
	type group struct {
		seller, title string
		n             int
		active        string // status of the one that survives
	}
	groups := []group{
		{"s-1002", "dup ordinary", 4, "published"},
		{"s-1001", "dup trusted", 3, "published"},      // R4 does not exempt from R3
		{"s-1004", "dup unverified", 3, "needs_human"}, // needs_human counts as active
		{"s-1003", "dup verified", 5, "published"},
	}
	var wg sync.WaitGroup
	ids := make([][]string, len(groups))
	for gi, g := range groups {
		ids[gi] = make([]string, g.n)
		for i := 0; i < g.n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Different prices: the price is not part of the duplicate check.
				r := send(t, "POST", f.url, bodyIn(g.seller, g.title, "c-phones", 6_000_000+i))
				ids[gi][i] = r.body["id"].(string)
			}()
		}
	}
	wg.Wait()

	for gi, g := range groups {
		var survivors []string
		for _, id := range ids[gi] {
			l := f.await(t, id)
			if l["status"] == g.active {
				survivors = append(survivors, id)
			}
		}
		if len(survivors) != 1 {
			t.Fatalf("%s: %d listings are active, want exactly 1", g.title, len(survivors))
		}
		for _, id := range ids[gi] {
			l := f.get(t, id)
			if id == survivors[0] {
				continue
			}
			rs := reasonsOf(l)
			if l["status"] != "auto_rejected" || len(rs) != 1 || rs[0]["rule_id"] != "R3" ||
				rs[0]["detail"].(map[string]any)["duplicate_of"] != survivors[0] {
				t.Fatalf("%s: loser %s should be an R3 duplicate of %s, got %v %v", g.title, id, survivors[0], l["status"], rs)
			}
		}
	}
}

// ---- needs_human -> moderator ----------------------------------------------

func TestFlowModeratorApprovesAndRejects(t *testing.T) {
	f := startFlow(t, flowOpts{})
	a := f.submit(t, "s-1004", "to approve", "c-phones", 6_000_000)
	b := f.submit(t, "s-1004", "to reject", "c-phones", 6_000_000)
	before := f.await(t, a)
	f.await(t, b)
	if before["status"] != "needs_human" {
		t.Fatalf("setup: %v", before["status"])
	}

	// Approve.
	r := f.decide(t, a, "approve", "m-7", "photos check out")
	if r.code != 200 || r.body["status"] != "published" || outcomeOf(r.body) != "approved" {
		t.Fatalf("approve: %d %s", r.code, r.raw)
	}
	rs := reasonsOf(r.body)
	if len(rs) != 1 || rs[0]["rule_id"] != "HUMAN" ||
		!strings.Contains(rs[0]["message"].(string), "m-7") || !strings.Contains(rs[0]["message"].(string), "photos check out") ||
		rs[0]["detail"].(map[string]any)["moderator_id"] != "m-7" {
		t.Fatalf("reasons: %v", rs)
	}
	if r.body["decided_at"] == nil || r.body["decided_at"].(string) < before["decided_at"].(string) {
		t.Fatalf("decided_at: %v then %v", before["decided_at"], r.body["decided_at"])
	}
	// What GET shows is the same: the human result replaced the escalation.
	got := f.get(t, a)
	if got["status"] != "published" || outcomeOf(got) != "approved" || !eqStrings(ruleIDsOf(got), []string{"HUMAN"}) {
		t.Fatalf("get after approve: %v", got)
	}

	// Reject, without a note.
	r = f.decide(t, b, "reject", "m-8", "")
	if r.code != 200 || r.body["status"] != "rejected" || outcomeOf(r.body) != "rejected" {
		t.Fatalf("reject: %d %s", r.code, r.raw)
	}

	// Resolved once: any further decision, same or opposite, is a conflict.
	for _, d := range []string{"approve", "reject"} {
		for _, id := range []string{a, b} {
			if r := f.decide(t, id, d, "m-9", ""); r.code != 409 {
				t.Fatalf("second decision (%s) on %s: %d %s", d, id, r.code, r.raw)
			}
		}
	}
	if got := f.get(t, a); got["status"] != "published" || ruleIDsOf(got)[0] != "HUMAN" {
		t.Fatal("a rejected second decision changed the listing")
	}
}

func TestFlowDecisionOnAListingThatIsNotAwaitingAHuman(t *testing.T) {
	f := startFlow(t, flowOpts{})
	published := f.submit(t, "s-1002", "fine", "c-phones", 6_000_000)
	rejected := f.submit(t, "s-1002", "fake", "c-phones", 6_000_000)
	f.await(t, published)
	f.await(t, rejected)

	for name, id := range map[string]string{"published": published, "auto_rejected": rejected} {
		r := f.decide(t, id, "approve", "m-1", "")
		if r.code != 409 {
			t.Fatalf("%s: %d %s", name, r.code, r.raw)
		}
		if e, _ := r.body["error"].(map[string]any); e == nil || e["code"] == "" || e["message"] == "" {
			t.Fatalf("409 should carry the Error body: %s", r.raw)
		}
	}
	if f.get(t, rejected)["status"] != "auto_rejected" {
		t.Fatal("a moderator changed an automatically rejected listing")
	}

	if r := f.decide(t, "l-doesnotexist", "approve", "m-1", ""); r.code != 404 {
		t.Fatalf("unknown id: %d %s", r.code, r.raw)
	}
}

func TestFlowDecisionOnAPendingListingIsAConflict(t *testing.T) {
	f := startFlow(t, flowOpts{noWorkers: true})
	id := f.submit(t, "s-1004", "x", "c-phones", 6_000_000)
	if r := f.decide(t, id, "approve", "m-1", ""); r.code != 409 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if f.get(t, id)["status"] != "pending" {
		t.Fatal("a moderator decided a listing the automated phase has not finished")
	}
}

func TestFlowDecisionBodyValidation(t *testing.T) {
	f := startFlow(t, flowOpts{})
	id := f.submit(t, "s-1004", "x", "c-phones", 6_000_000)
	f.await(t, id)
	url := f.url + "/" + id + "/decision"

	for name, b := range map[string]string{
		"empty":             ``,
		"malformed":         `{`,
		"unknown decision":  `{"decision":"maybe","moderator_id":"m"}`,
		"missing moderator": `{"decision":"approve"}`,
		"duplicate key":     `{"decision":"approve","decision":"reject","moderator_id":"m"}`,
		"note not string":   `{"decision":"approve","moderator_id":"m","note":1}`,
		"trailing data":     `{"decision":"approve","moderator_id":"m"}x`,
	} {
		if r := send(t, "POST", url, b); r.code != 400 {
			t.Fatalf("%s: %d %s", name, r.code, r.raw)
		}
	}
	if f.get(t, id)["status"] != "needs_human" {
		t.Fatal("an invalid request changed the listing")
	}
	// Unknown fields are ignored.
	if r := send(t, "POST", url, `{"decision":"reject","moderator_id":"m","extra":{"a":[1]}}`); r.code != 200 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}

// The schema requires moderator_id to be a string but does not forbid an empty
// one, so an empty id is accepted and recorded as given.
func TestFlowDecisionAcceptsAnEmptyModeratorID(t *testing.T) {
	f := startFlow(t, flowOpts{})
	id := f.submit(t, "s-1004", "x", "c-phones", 6_000_000)
	f.await(t, id)
	r := f.decide(t, id, "approve", "", "fine")
	if r.code != 200 || r.body["status"] != "published" {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	rs := reasonsOf(r.body)
	if rs[0]["rule_id"] != "HUMAN" || rs[0]["detail"].(map[string]any)["moderator_id"] != "" {
		t.Fatalf("%v", rs)
	}
	// A missing moderator_id is still an error.
	id2 := f.submit(t, "s-1004", "y", "c-phones", 6_000_000)
	f.await(t, id2)
	if r := send(t, "POST", f.url+"/"+id2+"/decision", `{"decision":"approve"}`); r.code != 400 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}

// Many moderators decide the same listing at once. One request wins; the final
// state is exactly what the winner asked for.
func TestFlowTwoModeratorsRaceExactlyOneWins(t *testing.T) {
	f := startFlow(t, flowOpts{})
	for round := 0; round < 5; round++ {
		id := f.submit(t, "s-1004", fmt.Sprintf("contested %d", round), "c-phones", 6_000_000)
		f.await(t, id)

		const n = 12
		replies := make([]reply, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				action := []string{"approve", "reject"}[i%2]
				replies[i] = f.decide(t, id, action, fmt.Sprintf("m-%d", i), "")
			}()
		}
		close(start)
		wg.Wait()

		winner := -1
		for i, r := range replies {
			switch r.code {
			case 200:
				if winner != -1 {
					t.Fatalf("round %d: two moderators both got 200", round)
				}
				winner = i
			case 409:
			default:
				t.Fatalf("round %d: unexpected %d %s", round, r.code, r.raw)
			}
		}
		if winner == -1 {
			t.Fatalf("round %d: nobody won", round)
		}

		l := f.get(t, id)
		wantStatus, wantOutcome := "published", "approved"
		if winner%2 == 1 {
			wantStatus, wantOutcome = "rejected", "rejected"
		}
		rs := reasonsOf(l)
		if l["status"] != wantStatus || outcomeOf(l) != wantOutcome || len(rs) != 1 ||
			rs[0]["detail"].(map[string]any)["moderator_id"] != fmt.Sprintf("m-%d", winner) {
			t.Fatalf("round %d: final state %v %s %v does not match winner m-%d (%s)", round, l["status"], outcomeOf(l), rs, winner, wantStatus)
		}
		// The 200 response described the same result.
		if replies[winner].body["status"] != wantStatus {
			t.Fatalf("round %d: the winning response said %v", round, replies[winner].body["status"])
		}
	}
}

// ---- retries exhausted: R1 is never skipped --------------------------------

// failCommitWhileRetrying makes the commit of matching listings fail on every
// ordinary attempt, so they reach the retries-exhausted path.
func failCommitWhileRetrying(match func(store.Claim) bool) func(store.Claim) error {
	return func(c store.Claim) error {
		if match(c) && c.Attempts <= flowCfg.MaxAttempts {
			return fmt.Errorf("injected commit failure on attempt %d", c.Attempts)
		}
		return nil
	}
}

func TestFlowRetriesExhaustedBannedContentIsStillRejectedByR1(t *testing.T) {
	// Every listing's commit fails until it has used up its normal attempts.
	f := startFlow(t, flowOpts{failCommit: failCommitWhileRetrying(func(store.Claim) bool { return true })})

	// A trusted seller, who would otherwise skip every other check.
	id := f.submit(t, "s-1001", "iPhone replica", "c-phones", 6_000_000)
	l := f.await(t, id)

	if l["status"] != "auto_rejected" || outcomeOf(l) != "rejected" || !eqStrings(ruleIDsOf(l), []string{"R1"}) {
		t.Fatalf("banned content after retries must end rejected by R1, got %v %s %v", l["status"], outcomeOf(l), ruleIDsOf(l))
	}
	if !strings.Contains(reasonsOf(l)[0]["message"].(string), `"replica"`) {
		t.Fatalf("message must quote the term: %v", reasonsOf(l)[0]["message"])
	}
	// It really did go through the exhausted path: MaxAttempts failures, then the fallback.
	if attempts, lastErr := f.attemptsAndError(t, id); attempts != flowCfg.MaxAttempts+1 || !strings.Contains(lastErr, "injected commit failure") {
		t.Fatalf("attempts=%d last_error=%q", attempts, lastErr)
	}
	// And a moderator cannot override it.
	if r := f.decide(t, id, "approve", "m-1", ""); r.code != 409 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
}

func TestFlowRetriesExhaustedCleanListingGoesToAModeratorWithTheReason(t *testing.T) {
	f := startFlow(t, flowOpts{failCommit: failCommitWhileRetrying(func(store.Claim) bool { return true })})

	// Even a trusted seller is escalated, not published: R4 was never evaluated.
	for _, seller := range []string{"s-1002", "s-1001"} {
		id := f.submit(t, seller, "iPhone "+seller, "c-phones", 6_000_000)
		l := f.await(t, id)
		rs := reasonsOf(l)
		if l["status"] != "needs_human" || outcomeOf(l) != "escalated" || len(rs) != 1 || rs[0]["rule_id"] != "SYS_EVAL_FAILED" {
			t.Fatalf("%s: %v %s %v", seller, l["status"], outcomeOf(l), rs)
		}
		msg := rs[0]["message"].(string)
		if !strings.Contains(msg, "injected commit failure on attempt 3") || !strings.Contains(msg, "3 failed attempts") {
			t.Fatalf("the stored error should explain the escalation: %q", msg)
		}
		if d := rs[0]["detail"].(map[string]any); d["failed_attempts"] != float64(3) ||
			!strings.Contains(d["last_error"].(string), "injected commit failure") {
			t.Fatalf("detail: %v", d)
		}

		// From here on it is an ordinary human resolution.
		if r := f.decide(t, id, "approve", "m-1", "reviewed manually"); r.code != 200 || r.body["status"] != "published" {
			t.Fatalf("%d %s", r.code, r.raw)
		}
		if r := f.decide(t, id, "reject", "m-2", ""); r.code != 409 {
			t.Fatalf("%d %s", r.code, r.raw)
		}
	}
}

func TestFlowRetriesExhaustedDuplicateIsRejectedNotEscalated(t *testing.T) {
	// Only the second listing (recognised by its price) has a failing commit.
	f := startFlow(t, flowOpts{failCommit: failCommitWhileRetrying(func(c store.Claim) bool { return c.Fields.PriceVND == 7_777_777 })})

	first := f.submit(t, "s-1002", "same item", "c-phones", 6_000_000)
	if f.await(t, first)["status"] != "published" {
		t.Fatal("setup")
	}
	second := f.submit(t, "s-1002", "same item", "c-phones", 7_777_777)
	l := f.await(t, second)
	rs := reasonsOf(l)
	if l["status"] != "auto_rejected" || len(rs) != 1 || rs[0]["rule_id"] != "R3" ||
		rs[0]["detail"].(map[string]any)["duplicate_of"] != first {
		t.Fatalf("a duplicate after retries must be rejected by R3, got %v %v", l["status"], rs)
	}
	if attempts, _ := f.attemptsAndError(t, second); attempts != flowCfg.MaxAttempts+1 {
		t.Fatalf("attempts = %d", attempts)
	}
}

// If R1 itself cannot run, nothing is ever decided: the listing stays pending
// and keeps being retried, and no approval path opens however many attempts
// have been used. When R1 recovers, the listing is past its retries, so it takes
// the exhausted path with R1 intact.
func TestFlowWhenR1CannotRunTheListingStaysPendingAndIsNeverApproved(t *testing.T) {
	var broken atomic.Bool
	broken.Store(true)
	f := startFlow(t, flowOpts{screener: func(l listing.Fields) ([]rules.Match, error) {
		if broken.Load() {
			return nil, errors.New("keyword matcher unavailable")
		}
		// Recovered: the real R1 behaviour for this test's one banned title.
		if strings.Contains(strings.ToLower(l.Title), "replica") {
			return []rules.Match{{Field: "title", Term: "replica"}}, nil
		}
		return nil, nil
	}})

	clean := f.submit(t, "s-1001", "clean item", "c-phones", 6_000_000)
	banned := f.submit(t, "s-1001", "replica item", "c-phones", 6_000_000)

	// Wait until both have gone well past MaxAttempts without being decided.
	deadline := time.Now().Add(20 * time.Second)
	for _, id := range []string{clean, banned} {
		for {
			attempts, _ := f.attemptsAndError(t, id)
			if attempts >= flowCfg.MaxAttempts+3 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("listing %s only reached %d attempts", id, attempts)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for _, id := range []string{clean, banned} {
		l := f.get(t, id)
		if l["status"] != "pending" || l["decision"] != nil || l["decided_at"] != nil {
			t.Fatalf("a listing whose R1 could not run was decided: %v", l)
		}
		if _, lastErr := f.attemptsAndError(t, id); !strings.Contains(lastErr, "R1 screening failed") ||
			!strings.Contains(lastErr, "keyword matcher unavailable") {
			t.Fatalf("last_error should say why: %q", lastErr)
		}
		if r := f.decide(t, id, "approve", "m-1", ""); r.code != 409 {
			t.Fatalf("a moderator can approve an unscreened listing: %d %s", r.code, r.raw)
		}
	}

	broken.Store(false) // R1 works again

	lc, lb := f.await(t, clean), f.await(t, banned)
	if lb["status"] != "auto_rejected" || !eqStrings(ruleIDsOf(lb), []string{"R1"}) {
		t.Fatalf("banned content must be rejected once R1 runs: %v %v", lb["status"], ruleIDsOf(lb))
	}
	// R1 is clean and the listing is past its retries: the exhausted path, not
	// the normal pipeline, so even this trusted seller goes to a moderator.
	rs := reasonsOf(lc)
	if lc["status"] != "needs_human" || len(rs) != 1 || rs[0]["rule_id"] != "SYS_EVAL_FAILED" ||
		!strings.Contains(rs[0]["message"].(string), "keyword matcher unavailable") {
		t.Fatalf("clean listing: %v %v", lc["status"], rs)
	}
}

// ---- readiness -------------------------------------------------------------

func TestFlowReadyzReportsTheWorkers(t *testing.T) {
	f := startFlow(t, flowOpts{})
	deadline := time.Now().Add(5 * time.Second)
	var r reply
	for {
		r = send(t, "GET", f.base+"/readyz", "")
		if r.code == http.StatusOK || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	checks, _ := r.body["checks"].(map[string]any)
	if r.code != 200 || checks["database"] != "ok" || checks["worker"] != "ok" {
		t.Fatalf("%d %s", r.code, r.raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.pool.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	r = send(t, "GET", f.base+"/readyz", "")
	checks, _ = r.body["checks"].(map[string]any)
	if r.code != 503 || checks["worker"] != "failed" || checks["database"] != "ok" {
		t.Fatalf("after the workers stopped: %d %s", r.code, r.raw)
	}
	// Liveness does not depend on the workers.
	if r := send(t, "GET", f.base+"/livez", ""); r.code != 200 {
		t.Fatalf("livez: %d", r.code)
	}
}
