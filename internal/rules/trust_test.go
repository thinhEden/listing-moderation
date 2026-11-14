package rules

import (
	"strings"
	"testing"

	"listing-moderation/internal/seed"
)

func TestIsTrustedThreshold(t *testing.T) {
	for _, tc := range []struct {
		score int
		want  bool
	}{{100, true}, {92, true}, {81, true}, {80, true}, {79, false}, {64, false}, {0, false}} {
		if got := IsTrusted(seed.Seller{TrustScore: tc.score}); got != tc.want {
			t.Errorf("IsTrusted(%d) = %v, want %v", tc.score, got, tc.want)
		}
	}
}

// Trust and verification are independent: R4 looks only at the score.
func TestIsTrustedIgnoresVerification(t *testing.T) {
	if !IsTrusted(seed.Seller{TrustScore: 88, Verified: false}) {
		t.Fatal("an unverified seller with trust 88 is still trusted under R4")
	}
}

func TestIsUnverified(t *testing.T) {
	if !IsUnverified(seed.Seller{Verified: false}) || IsUnverified(seed.Seller{Verified: true}) {
		t.Fatal("IsUnverified is wrong")
	}
}

func TestTrustedReasonSaysWhatWasSkipped(t *testing.T) {
	r := TrustedReason(seed.Seller{ID: "s-1005", TrustScore: 88})
	if r.RuleID != "R4" {
		t.Fatalf("rule id %q", r.RuleID)
	}
	for _, want := range []string{"88", "80", "not applied"} {
		if !strings.Contains(r.Message, want) {
			t.Fatalf("message %q should mention %q", r.Message, want)
		}
	}
	skipped, _ := r.Detail["skipped"].([]string)
	if len(skipped) != 2 || skipped[0] != "R2" || skipped[1] != "R5" {
		t.Fatalf("skipped = %v", r.Detail["skipped"])
	}
}

func TestUnverifiedReason(t *testing.T) {
	r := UnverifiedReason(seed.Seller{ID: "s-1004"})
	if r.RuleID != "R5" || !strings.Contains(r.Message, "s-1004") {
		t.Fatalf("%+v", r)
	}
}
