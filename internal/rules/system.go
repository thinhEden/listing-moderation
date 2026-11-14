package rules

import (
	"fmt"
	"unicode/utf8"

	"listing-moderation/internal/listing"
)

// Rule ids for results that do not come from R1-R5.
const (
	RuleHuman         = "HUMAN"
	RuleSysEvalFailed = "SYS_EVAL_FAILED"
)

// maxErrorInMessage keeps the stored last_error readable inside a reason.
const maxErrorInMessage = 300

// DecideAfterRetries is the decision once automated evaluation has failed too
// many times to be trusted to finish normally. It exists so a listing is never
// left pending forever, and it is deliberately narrower than Decide:
//
//  1. R1 found a banned term -> auto_rejected [R1]. R1 is never skipped: a
//     listing that is banned must not reach a human approval button just
//     because the system was struggling.
//  2. R3 finds an active duplicate -> auto_rejected [R3]. Escalating would put
//     a second active listing for the same item into the queue.
//  3. Otherwise -> needs_human [SYS_EVAL_FAILED], explaining the failure from
//     the last recorded error. A moderator decides it through the normal
//     human-resolution path.
//
// R4, R2 and R5 are not consulted: the system could not finish those checks
// reliably, so a trusted seller gets no shortcut here either.
//
// banned must come from a successful Screen. If R1 itself cannot run, the
// caller must not call this at all; it keeps retrying instead.
func (e *Engine) DecideAfterRetries(banned []Match, findDuplicate DuplicateFinder, failedAttempts int, lastError string) (Decision, error) {
	if len(banned) > 0 {
		return reject(BannedReason(banned)), nil
	}

	dupOf, found, err := findDuplicate()
	if err != nil {
		return Decision{}, fmt.Errorf("duplicate lookup: %w", err)
	}
	if found {
		return reject(DuplicateReason(dupOf)), nil
	}

	return Decision{
		Status:  listing.StatusNeedsHuman,
		Outcome: OutcomeEscalated,
		Reasons: []Reason{SysEvalFailedReason(failedAttempts, lastError)},
	}, nil
}

// SysEvalFailedReason explains an escalation caused by the system, not by the
// listing: how many attempts failed and what the last error was.
func SysEvalFailedReason(failedAttempts int, lastError string) Reason {
	msg := fmt.Sprintf("automated moderation did not complete after %d failed attempts; sent for human review", failedAttempts)
	detail := map[string]any{"failed_attempts": failedAttempts}
	if lastError != "" {
		msg += " (last error: " + truncateRunes(lastError, maxErrorInMessage) + ")"
		detail["last_error"] = lastError
	}
	return Reason{RuleID: RuleSysEvalFailed, Message: msg, Detail: detail}
}

// HumanDecision is the result of a moderator resolving a needs_human listing:
// published when approved, rejected otherwise. It replaces the automated
// escalation as the listing's current decision.
func HumanDecision(approve bool, moderatorID, note string) Decision {
	if approve {
		return Decision{
			Status:  listing.StatusPublished,
			Outcome: OutcomeApproved,
			Reasons: []Reason{HumanReason(true, moderatorID, note)},
		}
	}
	return Decision{
		Status:  listing.StatusRejected,
		Outcome: OutcomeRejected,
		Reasons: []Reason{HumanReason(false, moderatorID, note)},
	}
}

// HumanReason records who decided and why.
func HumanReason(approved bool, moderatorID, note string) Reason {
	verb := "rejected"
	if approved {
		verb = "approved"
	}
	msg := verb + " by moderator"
	if moderatorID != "" { // the API accepts an empty moderator_id
		msg += " " + moderatorID
	}
	detail := map[string]any{"moderator_id": moderatorID}
	if note != "" {
		msg += ": " + note
		detail["note"] = note
	}
	return Reason{RuleID: RuleHuman, Message: msg, Detail: detail}
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "..."
}
