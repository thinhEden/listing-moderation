package listing

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const validBody = `{"seller_id":"s-1002","title":"iPhone 13","description":"Like new","price_vnd":6000000,"category_id":"c-phones"}`

// bodyWith builds a valid body and replaces or adds raw JSON for one field.
// A nil raw removes the field.
func bodyWith(field string, raw *string) string {
	fields := []struct{ k, v string }{
		{"seller_id", `"s-1002"`}, {"title", `"iPhone 13"`}, {"description", `"Like new"`},
		{"price_vnd", `6000000`}, {"category_id", `"c-phones"`},
	}
	var parts []string
	seen := false
	for _, f := range fields {
		if f.k == field {
			seen = true
			if raw == nil {
				continue
			}
			f.v = *raw
		}
		parts = append(parts, fmt.Sprintf("%q:%s", f.k, f.v))
	}
	if !seen && raw != nil {
		parts = append(parts, fmt.Sprintf("%q:%s", field, *raw))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func raw(s string) *string { return &s }

func TestParseSubmissionValid(t *testing.T) {
	f, err := ParseSubmission([]byte(validBody))
	if err != nil {
		t.Fatal(err)
	}
	want := Fields{SellerID: "s-1002", Title: "iPhone 13", Description: "Like new", PriceVND: 6_000_000, CategoryID: "c-phones"}
	if f != want {
		t.Fatalf("got %+v, want %+v", f, want)
	}
}

func TestParseSubmissionAcceptedVariants(t *testing.T) {
	cases := map[string]string{
		"field order and whitespace": "  {\n\t\"category_id\" : \"c-phones\" ,\"price_vnd\":6000000,\n\"description\":\"Like new\",\"title\":\"iPhone 13\",\"seller_id\":\"s-1002\"}\n",
		"escaped characters":         `{"seller_id":"s-1002","title":"iPhone 13","description":"Like new","price_vnd":6000000,"category_id":"c-phones"}`,
		"unknown scalar field":       bodyWith("color", raw(`"red"`)),
		"unknown null field":         bodyWith("extra", raw(`null`)),
		"unknown nested object":      bodyWith("extra", raw(`{"a":{"b":[1,2,{"c":true}]},"d":[]}`)),
		"unknown array of objects":   bodyWith("extra", raw(`[{"a":1},{"a":2}]`)), // same key in different objects is fine
		"wrong-case key is unknown":  bodyWith("Title", raw(`"ignored"`)),
		"price zero":                 bodyWith("price_vnd", raw(`0`)),
		"price minus zero":           bodyWith("price_vnd", raw(`-0`)),
		"blank title is allowed":     bodyWith("title", raw(`"   "`)),
		"one-character title":        bodyWith("title", raw(`" "`)),
		"empty description":          bodyWith("description", raw(`""`)),
		"max int64 price":            bodyWith("price_vnd", raw(`9223372036854775807`)),
		"200 character title":        bodyWith("title", raw(`"`+strings.Repeat("a", 200)+`"`)),
		"200 multibyte title":        bodyWith("title", raw(`"`+strings.Repeat("đ", 200)+`"`)), // 400 bytes, 200 characters
		"5000 character description": bodyWith("description", raw(`"`+strings.Repeat("x", 5000)+`"`)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSubmission([]byte(body)); err != nil {
				t.Fatalf("rejected: %v\n%.200s", err, body)
			}
		})
	}
}

func TestParseSubmissionKeepsValuesVerbatim(t *testing.T) {
	body := `{"seller_id":"s-1002","title":"  Hàng  ĐẸP\t","description":"line1\nline2 à","price_vnd":1,"category_id":"c-phones"}`
	f, err := ParseSubmission([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != "  Hàng  ĐẸP\t" {
		t.Fatalf("title was altered: %q", f.Title)
	}
	if f.Description != "line1\nline2 à" {
		t.Fatalf("description: %q", f.Description)
	}
}

func TestParseSubmissionRejects(t *testing.T) {
	deep := strings.Repeat("[", 100) + strings.Repeat("]", 100)
	cases := []struct {
		name string
		body string
		want string // substring of the error
	}{
		// Document-level problems.
		{"empty body", ``, "empty"},
		{"whitespace only", " \n\t ", "empty"},
		{"truncated", `{"seller_id":`, "malformed"},
		{"not json", `hello`, "malformed"},
		{"array", `[]`, "JSON object"},
		{"string", `"x"`, "JSON object"},
		{"number", `42`, "JSON object"},
		{"null", `null`, "JSON object"},
		{"trailing garbage", validBody + ` x`, "after the JSON object"},
		{"second object", validBody + validBody, "after the JSON object"},
		{"trailing comma", `{"a":1,}`, "malformed"},

		// Duplicate keys, anywhere.
		{"duplicate top-level key", `{"title":"a","title":"b","seller_id":"s","description":"","price_vnd":1,"category_id":"c"}`, "duplicate key"},
		{"duplicate via escape", `{"title":"a","title":"b"}`, "duplicate key"},
		{"duplicate in unknown nested object", bodyWith("extra", raw(`{"a":1,"a":2}`)), "duplicate key"},
		{"duplicate deep inside unknown field", bodyWith("extra", raw(`{"x":[{"y":{"k":1,"k":2}}]}`)), "duplicate key"},
		{"duplicate inside array element", bodyWith("extra", raw(`[{"a":1,"a":2}]`)), "duplicate key"},
		{"nesting too deep", bodyWith("extra", raw(deep)), "too deeply"},

		// Missing and null.
		{"missing seller_id", bodyWith("seller_id", nil), "seller_id is required"},
		{"missing title", bodyWith("title", nil), "title is required"},
		{"missing description", bodyWith("description", nil), "description is required"},
		{"missing price_vnd", bodyWith("price_vnd", nil), "price_vnd is required"},
		{"missing category_id", bodyWith("category_id", nil), "category_id is required"},
		{"null title", bodyWith("title", raw(`null`)), "title must not be null"},
		{"null price", bodyWith("price_vnd", raw(`null`)), "price_vnd must not be null"},
		{"null description", bodyWith("description", raw(`null`)), "description must not be null"},

		// Types.
		{"title is a number", bodyWith("title", raw(`5`)), "title must be a string"},
		{"seller_id is an object", bodyWith("seller_id", raw(`{}`)), "seller_id must be a string"},
		{"category_id is an array", bodyWith("category_id", raw(`["c-phones"]`)), "category_id must be a string"},
		{"description is a bool", bodyWith("description", raw(`true`)), "description must be a string"},

		// price_vnd: plain integers only.
		{"price as string", bodyWith("price_vnd", raw(`"6000000"`)), "price_vnd must be a non-negative integer"},
		{"price with fraction", bodyWith("price_vnd", raw(`1.5`)), "price_vnd must be a non-negative integer"},
		{"price 1.0", bodyWith("price_vnd", raw(`1.0`)), "price_vnd must be a non-negative integer"},
		{"price exponent", bodyWith("price_vnd", raw(`1e3`)), "price_vnd must be a non-negative integer"},
		{"price negative", bodyWith("price_vnd", raw(`-1`)), "price_vnd must be a non-negative integer"},
		{"price minus zero point zero", bodyWith("price_vnd", raw(`-0.0`)), "price_vnd must be a non-negative integer"},
		{"price minus zero exponent", bodyWith("price_vnd", raw(`-0e0`)), "price_vnd must be a non-negative integer"},
		{"price minus one", bodyWith("price_vnd", raw(`-1`)), "price_vnd must be a non-negative integer"},
		{"price bool", bodyWith("price_vnd", raw(`true`)), "price_vnd must be a non-negative integer"},
		{"price array", bodyWith("price_vnd", raw(`[1]`)), "price_vnd must be a non-negative integer"},
		{"price one over int64", bodyWith("price_vnd", raw(`9223372036854775808`)), "too large"},
		{"price far over int64", bodyWith("price_vnd", raw(`99999999999999999999999999`)), "too large"},
		{"price leading zero", bodyWith("price_vnd", raw(`007`)), "malformed"},

		// Lengths and content.
		{"empty title", bodyWith("title", raw(`""`)), "title must be at least 1 character"},
		{"201 character title", bodyWith("title", raw(`"`+strings.Repeat("a", 201)+`"`)), "at most 200"},
		{"201 multibyte title", bodyWith("title", raw(`"`+strings.Repeat("đ", 201)+`"`)), "at most 200"},
		{"5001 character description", bodyWith("description", raw(`"`+strings.Repeat("x", 5001)+`"`)), "at most 5000"},
		{"NUL in title", bodyWith("title", raw(`"a\u0000b"`)), "NUL"},
		{"NUL in description", bodyWith("description", raw(`"\u0000"`)), "NUL"},
		{"NUL in seller_id", bodyWith("seller_id", raw(`"s\u0000"`)), "NUL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSubmission([]byte(tc.body))
			var inv *InvalidError
			if !errors.As(err, &inv) {
				t.Fatalf("want *InvalidError, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestParseSubmissionReportsEveryProblem(t *testing.T) {
	_, err := ParseSubmission([]byte(`{"title":"","price_vnd":1.5,"category_id":7}`))
	var inv *InvalidError
	if !errors.As(err, &inv) {
		t.Fatal(err)
	}
	for _, want := range []string{"seller_id is required", "description is required", "title must be at least 1 character",
		"price_vnd must be a non-negative integer", "category_id must be a string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %q", want, err)
		}
	}
}

// The duplicate-key message must stay bounded even if the key is huge.
func TestParseSubmissionTruncatesHugeKeyInError(t *testing.T) {
	k := strings.Repeat("k", 10_000)
	_, err := ParseSubmission([]byte(`{"` + k + `":1,"` + k + `":2}`))
	if err == nil || len(err.Error()) > 200 {
		t.Fatalf("error not bounded: %d bytes", len(fmt.Sprint(err)))
	}
}

// A title is judged by length only and kept exactly as sent.
func TestParseSubmissionKeepsBlankTitleVerbatim(t *testing.T) {
	f, err := ParseSubmission([]byte(bodyWith("title", raw(`" \t "`))))
	if err != nil {
		t.Fatal(err)
	}
	if f.Title != " \t " {
		t.Fatalf("title was altered: %q", f.Title)
	}
}

func TestParseSubmissionMinusZeroIsZero(t *testing.T) {
	neg, err := ParseSubmission([]byte(bodyWith("price_vnd", raw(`-0`))))
	if err != nil {
		t.Fatal(err)
	}
	pos, err := ParseSubmission([]byte(bodyWith("price_vnd", raw(`0`))))
	if err != nil {
		t.Fatal(err)
	}
	if neg.PriceVND != 0 || neg != pos {
		t.Fatalf("-0 parsed as %+v, 0 as %+v", neg, pos)
	}
	// After decoding they are the same request, so an idempotent retry that
	// writes the price the other way is a replay, not a conflict.
	if !bytes.Equal(RequestHash(neg), RequestHash(pos)) {
		t.Fatal("-0 and 0 must have the same request hash")
	}
	if !bytes.Equal(DupFingerprint(neg), DupFingerprint(pos)) {
		t.Fatal("-0 and 0 must have the same duplicate fingerprint")
	}
}
