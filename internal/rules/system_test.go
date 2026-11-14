package rules

import (
	"errors"
	"strings"
	"testing"

	"listing-moderation/internal/listing"
)

func TestDecideAfterRetries(t *testing.T) {
	e := newEngine(t)
	spy := &dupSpy{}
	none := spy.none

	// R1 found something: rejected, and R3 is not even consulted.
	d, err := e.DecideAfterRetries([]Match{{"title", "fake"}}, none, 5, "boom")
	if err != nil || d.Status != listing.StatusAutoRejected || ruleIDs(d)[0] != "R1" || len(d.Reasons) != 1 {
		t.Fatalf("banned: %+v %v", d, err)
	}
	if spy.calls != 0 {
		t.Fatal("R3 was consulted after R1 had already rejected")
	}

	// Clean and not a duplicate: a human decides, and is told why.
	d, err = e.DecideAfterRetries(nil, none, 5, "commit: connection reset")
	if err != nil || d.Status != listing.StatusNeedsHuman || d.Outcome != OutcomeEscalated || ruleIDs(d)[0] != "SYS_EVAL_FAILED" {
		t.Fatalf("clean: %+v %v", d, err)
	}
	r := d.Reasons[0]
	if !strings.Contains(r.Message, "5 failed attempts") || !strings.Contains(r.Message, "connection reset") {
		t.Fatalf("message should explain the failure: %q", r.Message)
	}
	if r.Detail["last_error"] != "commit: connection reset" || r.Detail["failed_attempts"] != 5 {
		t.Fatalf("detail: %v", r.Detail)
	}

	// An active duplicate: rejected, not escalated.
	d, err = e.DecideAfterRetries(nil, duplicateOf("l-1"), 5, "x")
	if err != nil || d.Status != listing.StatusAutoRejected || ruleIDs(d)[0] != "R3" || d.Reasons[0].Detail["duplicate_of"] != "l-1" {
		t.Fatalf("duplicate: %+v %v", d, err)
	}

	// A failing lookup is an error, never an escalation.
	boom := errors.New("db down")
	if _, err := e.DecideAfterRetries(nil, func() (string, bool, error) { return "", false, boom }, 5, ""); !errors.Is(err, boom) {
		t.Fatalf("lookup error: %v", err)
	}
}

// Even a trusted seller, an unverified one, or an absurd price get no shortcut:
// after retries only R1 and R3 can reject and everything else goes to a human.
func TestDecideAfterRetriesIgnoresSellerAndPrice(t *testing.T) {
	e := newEngine(t)
	for _, seller := range []string{"s-1001", "s-1002", "s-1004", "s-1008"} {
		d, err := e.DecideAfterRetries(nil, noDuplicate, 3, "")
		if err != nil || d.Status != listing.StatusNeedsHuman {
			t.Fatalf("%s: %+v %v", seller, d, err)
		}
	}
}

func TestSysEvalFailedReasonWithoutAnError(t *testing.T) {
	r := SysEvalFailedReason(2, "")
	if strings.Contains(r.Message, "last error") {
		t.Fatalf("%q", r.Message)
	}
	if _, ok := r.Detail["last_error"]; ok {
		t.Fatal("no last_error expected")
	}
}

func TestSysEvalFailedReasonTruncatesLongErrorsInTheMessageOnly(t *testing.T) {
	long := strings.Repeat("e", 2000)
	r := SysEvalFailedReason(5, long)
	if len(r.Message) > 600 || !strings.HasSuffix(r.Message, "...)") {
		t.Fatalf("message length %d", len(r.Message))
	}
	if r.Detail["last_error"] != long {
		t.Fatal("detail keeps the full error")
	}
}

func TestHumanDecision(t *testing.T) {
	a := HumanDecision(true, "m-1", "looks fine")
	if a.Status != listing.StatusPublished || a.Outcome != OutcomeApproved || len(a.Reasons) != 1 {
		t.Fatalf("%+v", a)
	}
	if r := a.Reasons[0]; r.RuleID != "HUMAN" || r.Message != "approved by moderator m-1: looks fine" ||
		r.Detail["moderator_id"] != "m-1" || r.Detail["note"] != "looks fine" {
		t.Fatalf("%+v", r)
	}
	r := HumanDecision(false, "m-2", "")
	if r.Status != listing.StatusRejected || r.Outcome != OutcomeRejected {
		t.Fatalf("%+v", r)
	}
	if r.Reasons[0].Message != "rejected by moderator m-2" {
		t.Fatalf("%q", r.Reasons[0].Message)
	}
	if _, ok := r.Reasons[0].Detail["note"]; ok {
		t.Fatal("no note expected")
	}
}

func TestHumanReasonWithAnEmptyModeratorID(t *testing.T) {
	r := HumanReason(true, "", "ok")
	if r.Message != "approved by moderator: ok" || r.Detail["moderator_id"] != "" {
		t.Fatalf("%+v", r)
	}
	if r := HumanReason(false, "", ""); r.Message != "rejected by moderator" {
		t.Fatalf("%q", r.Message)
	}
}
