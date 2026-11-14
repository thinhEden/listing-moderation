package rules

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/seed"
)

// The engine is tested against the repository's real seed, so the cases below
// are the seed sellers the brief's authors set up as edge cases.
func newEngine(t *testing.T) *Engine {
	t.Helper()
	data, err := seed.Load(filepath.Join("..", "..", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(data)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func noDuplicate() (string, bool, error) { return "", false, nil }

func duplicateOf(id string) DuplicateFinder {
	return func() (string, bool, error) { return id, true, nil }
}

type dupSpy struct{ calls int }

func (s *dupSpy) none() (string, bool, error) { s.calls++; return "", false, nil }

func ruleIDs(d Decision) []string {
	ids := make([]string, len(d.Reasons))
	for i, r := range d.Reasons {
		ids[i] = r.RuleID
	}
	return ids
}

func fields(seller, category, title string, price int64) listing.Fields {
	return listing.Fields{SellerID: seller, CategoryID: category, Title: title, Description: "ok", PriceVND: price}
}

func TestDecideMatrix(t *testing.T) {
	e := newEngine(t)
	const (
		published = listing.StatusPublished
		rejected  = listing.StatusAutoRejected
		human     = listing.StatusNeedsHuman
	)
	cases := []struct {
		name   string
		f      listing.Fields
		dup    DuplicateFinder
		status listing.Status
		rules  []string
	}{
		// Clean listings.
		{"ordinary seller, clean, normal price", fields("s-1002", "c-phones", "iPhone 13", 6_000_000), noDuplicate, published, []string{"AUTO_PASS"}},
		{"trusted seller, clean", fields("s-1001", "c-phones", "iPhone 13", 6_000_000), noDuplicate, published, []string{"R4"}},

		// R1: applies to everyone, and stops everything else.
		{"R1 ordinary seller", fields("s-1002", "c-phones", "iPhone replica", 6_000_000), noDuplicate, rejected, []string{"R1"}},
		{"R1 beats R4: trusted seller", fields("s-1001", "c-phones", "iPhone replica", 6_000_000), noDuplicate, rejected, []string{"R1"}},
		{"R1 hides R5: unverified new seller", fields("s-1008", "c-fashion", "hang gia", 450_000), noDuplicate, rejected, []string{"R1"}},
		{"R1 hides R2 and R5", fields("s-1004", "c-phones", "fake phone", 1), noDuplicate, rejected, []string{"R1"}},
		{"R1 beats R3", fields("s-1002", "c-phones", "fake", 6_000_000), duplicateOf("l-1"), rejected, []string{"R1"}},
		{"R1 trusted + unverified (s-1005)", fields("s-1005", "c-phones", "stolen goods", 6_000_000), noDuplicate, rejected, []string{"R1"}},

		// R3: applies to everyone, beats R4/R2/R5.
		{"R3 ordinary seller", fields("s-1003", "c-phones", "iPhone 13", 6_000_000), duplicateOf("l-9"), rejected, []string{"R3"}},
		{"R3 beats R4: trusted seller", fields("s-1001", "c-phones", "iPhone 13", 6_000_000), duplicateOf("l-9"), rejected, []string{"R3"}},
		{"R3 hides R2 and R5", fields("s-1004", "c-phones", "iPhone 13", 30_000_000), duplicateOf("l-9"), rejected, []string{"R3"}},

		// R4: trusted sellers skip R2 and R5.
		{"R4 trusted but unverified (s-1005): R5 skipped", fields("s-1005", "c-phones", "iPhone 13", 6_000_000), noDuplicate, published, []string{"R4"}},
		{"R4 exactly at threshold (s-1006), anomalous price: R2 skipped", fields("s-1006", "c-phones", "iPhone 13", 30_000_000), noDuplicate, published, []string{"R4"}},
		{"one below threshold (s-1007), anomalous price", fields("s-1007", "c-phones", "iPhone 13", 30_000_000), noDuplicate, human, []string{"R2"}},
		{"one below threshold (s-1007), normal price", fields("s-1007", "c-phones", "iPhone 13", 6_000_000), noDuplicate, published, []string{"AUTO_PASS"}},

		// R2 and R5 are a group: every rule that fired is reported.
		{"R2 alone (verified seller)", fields("s-1003", "c-phones", "iPhone 13", 30_000_000), noDuplicate, human, []string{"R2"}},
		{"R5 alone (unverified, normal price)", fields("s-1004", "c-phones", "iPhone 13", 6_000_000), noDuplicate, human, []string{"R5"}},
		{"R2 and R5 together", fields("s-1004", "c-phones", "iPhone 13", 1_900_000), noDuplicate, human, []string{"R2", "R5"}},
		{"brand new seller, too cheap", fields("s-1008", "c-furniture", "Table", 100_000), noDuplicate, human, []string{"R2", "R5"}},

		// R2 boundaries through the whole pipeline.
		{"motorbike at exactly 3x", fields("s-1003", "c-motorbikes", "Wave", 96_000_000), noDuplicate, published, []string{"AUTO_PASS"}},
		{"motorbike one over 3x", fields("s-1003", "c-motorbikes", "Wave", 96_000_001), noDuplicate, human, []string{"R2"}},
		{"motorbike first price not below a third", fields("s-1003", "c-motorbikes", "Wave", 10_666_667), noDuplicate, published, []string{"AUTO_PASS"}},
		{"motorbike last price below a third", fields("s-1003", "c-motorbikes", "Wave", 10_666_666), noDuplicate, human, []string{"R2"}},
		{"free listing", fields("s-1002", "c-fashion", "Gift", 0), noDuplicate, human, []string{"R2"}},
		{"absurdly high price (no overflow)", fields("s-1002", "c-phones", "Gold", 1<<63-1), noDuplicate, human, []string{"R2"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := e.Decide(tc.f, e.Screen(tc.f), tc.dup)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != tc.status {
				t.Fatalf("status = %s, want %s (reasons %v)", d.Status, tc.status, ruleIDs(d))
			}
			if got := ruleIDs(d); !reflect.DeepEqual(got, tc.rules) {
				t.Fatalf("reasons = %v, want %v", got, tc.rules)
			}
			// Status, outcome and reasons must always agree.
			wantOutcome := map[listing.Status]Outcome{published: OutcomeApproved, rejected: OutcomeRejected, human: OutcomeEscalated}[tc.status]
			if d.Outcome != wantOutcome {
				t.Fatalf("outcome = %s, want %s", d.Outcome, wantOutcome)
			}
			for _, r := range d.Reasons {
				if r.RuleID == "" || r.Message == "" {
					t.Fatalf("reason with empty fields: %+v", r)
				}
			}
		})
	}
}

func TestDecideNeverProducesAutoApproved(t *testing.T) {
	e := newEngine(t)
	for _, seller := range []string{"s-1001", "s-1002", "s-1004", "s-1008"} {
		f := fields(seller, "c-phones", "iPhone", 6_000_000)
		d, err := e.Decide(f, e.Screen(f), noDuplicate)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status == listing.StatusAutoApproved || d.Status == listing.StatusPending {
			t.Fatalf("%s: status %s", seller, d.Status)
		}
	}
}

// R3 is a database lookup, so a listing already rejected by R1 must not cause one.
func TestDecideSkipsDuplicateLookupWhenR1Rejects(t *testing.T) {
	e := newEngine(t)
	spy := &dupSpy{}
	f := fields("s-1002", "c-phones", "fake phone", 6_000_000)
	if _, err := e.Decide(f, e.Screen(f), spy.none); err != nil {
		t.Fatal(err)
	}
	if spy.calls != 0 {
		t.Fatalf("duplicate lookup ran %d times after an R1 rejection", spy.calls)
	}

	// And it runs exactly once otherwise, even for a trusted seller (R3 comes before R4).
	for _, seller := range []string{"s-1001", "s-1002"} {
		spy := &dupSpy{}
		f := fields(seller, "c-phones", "iPhone", 6_000_000)
		if _, err := e.Decide(f, e.Screen(f), spy.none); err != nil {
			t.Fatal(err)
		}
		if spy.calls != 1 {
			t.Fatalf("%s: duplicate lookup ran %d times, want 1", seller, spy.calls)
		}
	}
}

func TestDecideReportsDuplicateOf(t *testing.T) {
	e := newEngine(t)
	f := fields("s-1002", "c-phones", "iPhone", 6_000_000)
	d, err := e.Decide(f, nil, duplicateOf("l-42"))
	if err != nil {
		t.Fatal(err)
	}
	r := d.Reasons[0]
	if r.RuleID != "R3" || r.Detail["duplicate_of"] != "l-42" || !strings.Contains(r.Message, "l-42") {
		t.Fatalf("%+v", r)
	}
}

func TestDecidePropagatesLookupAndSeedErrors(t *testing.T) {
	e := newEngine(t)
	boom := errors.New("db down")
	f := fields("s-1002", "c-phones", "iPhone", 6_000_000)
	if _, err := e.Decide(f, nil, func() (string, bool, error) { return "", false, boom }); !errors.Is(err, boom) {
		t.Fatalf("lookup error not propagated: %v", err)
	}
	if _, err := e.Decide(fields("s-nope", "c-phones", "x", 1), nil, noDuplicate); err == nil {
		t.Fatal("unknown seller must be an error, never an approval")
	}
	if _, err := e.Decide(fields("s-1002", "c-nope", "x", 1), nil, noDuplicate); err == nil {
		t.Fatal("unknown category must be an error, never an approval")
	}
}

// R1 is judged only on the banned list passed in: an empty Screen result means
// "R1 ran and found nothing". Decide never re-reads the title itself, which is
// what lets the worker run R1 outside its lock.
func TestDecideTrustsTheScreenResult(t *testing.T) {
	e := newEngine(t)
	f := fields("s-1002", "c-phones", "fake phone", 6_000_000)
	d, err := e.Decide(f, []Match{{"title", "fake"}}, noDuplicate)
	if err != nil || d.Status != listing.StatusAutoRejected {
		t.Fatalf("%v %+v", err, d)
	}
}

func TestDecisionReasonsMarshalToTheAPISchema(t *testing.T) {
	e := newEngine(t)
	f := fields("s-1004", "c-phones", "iPhone", 1_900_000)
	d, _ := e.Decide(f, nil, noDuplicate)
	b, err := json.Marshal(d.Reasons)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	for _, r := range back {
		if _, ok := r["rule_id"].(string); !ok {
			t.Fatalf("missing rule_id: %v", r)
		}
		if _, ok := r["message"].(string); !ok {
			t.Fatalf("missing message: %v", r)
		}
	}
}

func TestNewEngineRejectsBadKeywords(t *testing.T) {
	if _, err := NewEngine(&seed.Data{Keywords: []string{" "}}); err == nil {
		t.Fatal("expected an error")
	}
}
