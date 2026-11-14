package listing

import (
	"errors"
	"strings"
	"testing"
)

func TestParseHumanDecisionValid(t *testing.T) {
	cases := map[string]struct {
		body string
		want HumanDecisionRequest
	}{
		"approve":                       {`{"decision":"approve","moderator_id":"m-1"}`, HumanDecisionRequest{ActionApprove, "m-1", ""}},
		"reject":                        {`{"decision":"reject","moderator_id":"m-2"}`, HumanDecisionRequest{ActionReject, "m-2", ""}},
		"with note":                     {`{"decision":"approve","moderator_id":"m-1","note":"checked the photos"}`, HumanDecisionRequest{ActionApprove, "m-1", "checked the photos"}},
		"empty note":                    {`{"decision":"reject","moderator_id":"m-1","note":""}`, HumanDecisionRequest{ActionReject, "m-1", ""}},
		"unknown field":                 {`{"decision":"approve","moderator_id":"m-1","extra":{"a":[1,{"b":2}]}}`, HumanDecisionRequest{ActionApprove, "m-1", ""}},
		"field order":                   {`{"note":"n","moderator_id":"m-1","decision":"approve"}`, HumanDecisionRequest{ActionApprove, "m-1", "n"}},
		"empty moderator_id is allowed": {`{"decision":"approve","moderator_id":""}`, HumanDecisionRequest{ActionApprove, "", ""}},
		"unicode":                       {`{"decision":"approve","moderator_id":"kiểm-duyệt-1","note":"Hàng thật"}`, HumanDecisionRequest{ActionApprove, "kiểm-duyệt-1", "Hàng thật"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := ParseHumanDecision([]byte(tc.body))
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestParseHumanDecisionRejects(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"empty body", ``, "empty"},
		{"not an object", `[]`, "JSON object"},
		{"null", `null`, "JSON object"},
		{"malformed", `{"decision":`, "malformed"},
		{"trailing data", `{"decision":"approve","moderator_id":"m"} x`, "after the JSON object"},
		{"duplicate key", `{"decision":"approve","decision":"reject","moderator_id":"m"}`, "duplicate key"},
		{"duplicate key in unknown field", `{"decision":"approve","moderator_id":"m","x":{"a":1,"a":2}}`, "duplicate key"},
		{"missing decision", `{"moderator_id":"m"}`, "decision is required"},
		{"missing moderator", `{"decision":"approve"}`, "moderator_id is required"},
		{"unknown decision", `{"decision":"maybe","moderator_id":"m"}`, `must be "approve" or "reject"`},
		{"decision in wrong case", `{"decision":"Approve","moderator_id":"m"}`, `must be "approve" or "reject"`},
		{"decision with space", `{"decision":"approve ","moderator_id":"m"}`, `must be "approve" or "reject"`},
		{"decision not a string", `{"decision":true,"moderator_id":"m"}`, "decision must be a string"},
		{"null decision", `{"decision":null,"moderator_id":"m"}`, "decision must not be null"},
		{"moderator not a string", `{"decision":"approve","moderator_id":7}`, "moderator_id must be a string"},
		{"null note", `{"decision":"approve","moderator_id":"m","note":null}`, "note must not be null"},
		{"note not a string", `{"decision":"approve","moderator_id":"m","note":5}`, "note must be a string"},
		{"NUL in note", `{"decision":"approve","moderator_id":"m","note":"a\u0000b"}`, "NUL"},
		{"NUL in moderator", `{"decision":"approve","moderator_id":"m\u0000"}`, "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseHumanDecision([]byte(tc.body))
			var inv *InvalidError
			if !errors.As(err, &inv) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an InvalidError containing %q", err, tc.want)
			}
		})
	}
}
