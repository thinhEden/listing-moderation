package listing

import (
	"bytes"
	"encoding/json"
	"strings"
)

// HumanAction is what a moderator chose.
type HumanAction string

const (
	ActionApprove HumanAction = "approve"
	ActionReject  HumanAction = "reject"
)

// HumanDecisionRequest is a validated POST /v1/listings/{id}/decision body.
type HumanDecisionRequest struct {
	Action      HumanAction
	ModeratorID string
	Note        string // "" when none was given
}

// ParseHumanDecision decodes and validates a moderator's decision body with
// the same strictness as ParseSubmission: duplicate keys at any depth, data
// after the object and malformed JSON are errors; unknown fields are ignored.
//
// decision must be "approve" or "reject"; moderator_id is a required string
// (possibly empty); note is an optional string.
func ParseHumanDecision(body []byte) (HumanDecisionRequest, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return HumanDecisionRequest{}, invalid("request body is empty")
	}
	if err := checkDocument(body); err != nil {
		return HumanDecisionRequest{}, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return HumanDecisionRequest{}, invalid("request body must be a JSON object")
	}

	var problems []string
	var req HumanDecisionRequest

	str := func(name string, required bool) (string, bool) {
		raw, ok := obj[name]
		if !ok {
			if required {
				problems = append(problems, name+" is required")
			}
			return "", false
		}
		if isNull(raw) {
			problems = append(problems, name+" must not be null")
			return "", false
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			problems = append(problems, name+" must be a string")
			return "", false
		}
		if strings.ContainsRune(s, 0) {
			// A storage limit: Postgres text cannot hold U+0000.
			problems = append(problems, name+" must not contain the NUL character (U+0000); it cannot be stored")
			return "", false
		}
		return s, true
	}

	if v, ok := str("decision", true); ok {
		switch HumanAction(v) {
		case ActionApprove, ActionReject:
			req.Action = HumanAction(v)
		default:
			problems = append(problems, `decision must be "approve" or "reject"`)
		}
	}
	// moderator_id must be present and a string; the schema does not forbid an
	// empty one, so an empty string is accepted and recorded as given.
	if v, ok := str("moderator_id", true); ok {
		req.ModeratorID = v
	}
	if v, ok := str("note", false); ok {
		req.Note = v
	}

	if len(problems) > 0 {
		return HumanDecisionRequest{}, &InvalidError{Problems: problems}
	}
	return req, nil
}
