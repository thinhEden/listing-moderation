package rules

import (
	"fmt"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/seed"
)

// Engine applies the rules in their agreed order. It holds only read-only seed
// data and is safe for concurrent use.
type Engine struct {
	banned     *BannedMatcher
	sellers    map[string]seed.Seller
	categories map[string]seed.Category
}

func NewEngine(data *seed.Data) (*Engine, error) {
	banned, err := NewBannedMatcher(data.Keywords)
	if err != nil {
		return nil, err
	}
	return &Engine{banned: banned, sellers: data.Sellers, categories: data.Categories}, nil
}

// Screen runs R1, the one rule that needs no database and may be slow, so the
// worker can run it before it takes any lock. Pass its result to Decide.
func (e *Engine) Screen(f listing.Fields) []Match {
	return e.banned.Check(f.Title, f.Description)
}

// DuplicateFinder looks up an active listing from the same seller with the
// same fingerprint as the one being decided. The worker supplies it, running
// the query inside the seller lock; Decide stays free of the database.
type DuplicateFinder func() (duplicateOf string, found bool, err error)

// Decide is the automated phase's decision function. Rules are applied in
// this order and evaluation stops at the first that applies:
//
//  1. R1 banned keyword      -> auto_rejected, reasons [R1]       (every seller)
//  2. R3 duplicate           -> auto_rejected, reasons [R3]       (every seller)
//  3. R4 trusted seller      -> published,     reasons [R4]       (R2, R5 not run)
//  4. R2 and/or R5 fire      -> needs_human,   one reason per fired rule
//  5. nothing fired          -> published,     reasons [AUTO_PASS]
//
// banned is the result of Screen for the same listing. findDuplicate is
// called only if R1 did not reject, so a rejected listing never touches the
// database. Approval and publication are a single step, so the status for an
// approval is published; auto_approved is never produced.
func (e *Engine) Decide(f listing.Fields, banned []Match, findDuplicate DuplicateFinder) (Decision, error) {
	// R1.
	if len(banned) > 0 {
		return reject(BannedReason(banned)), nil
	}

	// R3.
	dupOf, found, err := findDuplicate()
	if err != nil {
		return Decision{}, fmt.Errorf("duplicate lookup: %w", err)
	}
	if found {
		return reject(DuplicateReason(dupOf)), nil
	}

	seller, ok := e.sellers[f.SellerID]
	if !ok {
		return Decision{}, fmt.Errorf("unknown seller %q", f.SellerID)
	}
	category, ok := e.categories[f.CategoryID]
	if !ok {
		return Decision{}, fmt.Errorf("unknown category %q", f.CategoryID)
	}

	// R4. A trusted seller skips R2 and R5 entirely.
	if IsTrusted(seller) {
		return Decision{
			Status:  listing.StatusPublished,
			Outcome: OutcomeApproved,
			Reasons: []Reason{TrustedReason(seller)},
		}, nil
	}

	// R2 and R5 are one group: run both, report every one that fired.
	var reasons []Reason
	if finding, fired := CheckPrice(f.PriceVND, category.MedianPriceVND); fired {
		reasons = append(reasons, PriceReason(finding, category.ID))
	}
	if IsUnverified(seller) {
		reasons = append(reasons, UnverifiedReason(seller))
	}
	if len(reasons) > 0 {
		return Decision{
			Status:  listing.StatusNeedsHuman,
			Outcome: OutcomeEscalated,
			Reasons: reasons,
		}, nil
	}

	return Decision{
		Status:  listing.StatusPublished,
		Outcome: OutcomeApproved,
		Reasons: []Reason{AutoPassReason()},
	}, nil
}

func reject(r Reason) Decision {
	return Decision{
		Status:  listing.StatusAutoRejected,
		Outcome: OutcomeRejected,
		Reasons: []Reason{r},
	}
}

// DuplicateReason builds the R3 reason.
func DuplicateReason(duplicateOf string) Reason {
	return Reason{
		RuleID: RuleDuplicate,
		Message: fmt.Sprintf("duplicate of active listing %s from the same seller "+
			"(same category, title and description)", duplicateOf),
		Detail: map[string]any{"duplicate_of": duplicateOf},
	}
}

// AutoPassReason builds the reason for a listing no rule fired on.
func AutoPassReason() Reason {
	return Reason{
		RuleID:  RuleAutoPass,
		Message: "no moderation rule fired",
	}
}
