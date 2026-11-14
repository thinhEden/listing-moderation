// Package rules contains the moderation rules R1-R5 as pure functions: no
// database, no clock, no I/O. Everything that needs the database (the R3
// duplicate lookup) is injected by the caller, so the worker can run it inside
// its seller lock while the decision order stays here, in one place.
package rules

import "listing-moderation/internal/listing"

// Rule ids as they appear in API reasons.
const (
	RuleBanned     = "R1"
	RulePrice      = "R2"
	RuleDuplicate  = "R3"
	RuleTrusted    = "R4"
	RuleUnverified = "R5"
	RuleAutoPass   = "AUTO_PASS"
)

// Outcome is the API's decision.outcome.
type Outcome string

const (
	OutcomeApproved  Outcome = "approved"
	OutcomeRejected  Outcome = "rejected"
	OutcomeEscalated Outcome = "escalated"
)

// Reason explains one rule's contribution to a decision. It marshals to the
// API's Reason schema.
type Reason struct {
	RuleID  string         `json:"rule_id"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

// Decision is the committed result of the automated phase: the listing's new
// status and the API decision (outcome + reasons).
type Decision struct {
	Status  listing.Status
	Outcome Outcome
	Reasons []Reason
}
