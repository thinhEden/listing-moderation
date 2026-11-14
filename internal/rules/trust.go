package rules

import (
	"fmt"

	"listing-moderation/internal/seed"
)

// TrustThreshold is the R4 cut-off: trust_score >= 80 is trusted.
const TrustThreshold = 80

// IsTrusted implements R4's condition.
func IsTrusted(s seed.Seller) bool { return s.TrustScore >= TrustThreshold }

// IsUnverified implements R5's condition.
func IsUnverified(s seed.Seller) bool { return !s.Verified }

// TrustedReason builds the R4 reason. R4 stops evaluation before R2 and R5, so
// the message says which checks were not applied; a moderator reading the
// listing later can tell the price and verification checks never ran.
func TrustedReason(s seed.Seller) Reason {
	return Reason{
		RuleID: RuleTrusted,
		Message: fmt.Sprintf("seller trust_score %d >= %d: price and verification checks not applied",
			s.TrustScore, TrustThreshold),
		Detail: map[string]any{
			"seller_id":   s.ID,
			"trust_score": s.TrustScore,
			"threshold":   TrustThreshold,
			"skipped":     []string{RulePrice, RuleUnverified},
		},
	}
}

// UnverifiedReason builds the R5 reason.
func UnverifiedReason(s seed.Seller) Reason {
	return Reason{
		RuleID:  RuleUnverified,
		Message: fmt.Sprintf("seller %s is not verified", s.ID),
		Detail:  map[string]any{"seller_id": s.ID},
	}
}
