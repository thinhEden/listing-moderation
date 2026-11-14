package listing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits from openapi.yaml's ListingRequest.
const (
	MaxTitleChars       = 200
	MaxDescriptionChars = 5000
)

// maxJSONDepth bounds nesting so a hostile body of "[[[[..." cannot exhaust the stack.
const maxJSONDepth = 32

// InvalidError is a request the client must fix (HTTP 400). It lists every
// problem found, not just the first.
type InvalidError struct{ Problems []string }

func (e *InvalidError) Error() string { return strings.Join(e.Problems, "; ") }

func invalid(format string, args ...any) *InvalidError {
	return &InvalidError{Problems: []string{fmt.Sprintf(format, args...)}}
}

// ParseSubmission decodes and validates a POST /v1/listings body.
//
// It is stricter than encoding/json in the ways the API needs:
//   - a duplicate object key anywhere in the document is an error (encoding/json
//     silently keeps the last one), including inside unknown fields, which may
//     hold nested objects and arrays;
//   - data after the top-level object is an error;
//   - price_vnd must be a non-negative integer that fits int64, written with
//     digits only: no fraction, exponent, string or float, and no minus sign
//     except "-0", which is read as 0;
//
// Unknown top-level fields are accepted and ignored. Whether the seller and
// category exist is not checked here; that is a 422, not a 400.
func ParseSubmission(body []byte) (Fields, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return Fields{}, invalid("request body is empty")
	}
	if err := checkDocument(body); err != nil {
		return Fields{}, err
	}

	var obj map[string]json.RawMessage
	// A literal null unmarshals into a nil map without error, so check for it.
	if err := json.Unmarshal(body, &obj); err != nil || obj == nil {
		return Fields{}, invalid("request body must be a JSON object")
	}

	var problems []string
	var f Fields

	str := func(name string, dst *string) bool {
		raw, ok := obj[name]
		switch {
		case !ok:
			problems = append(problems, name+" is required")
		case isNull(raw):
			problems = append(problems, name+" must not be null")
		default:
			var s string
			if err := json.Unmarshal(raw, &s); err != nil {
				problems = append(problems, name+" must be a string")
			} else if strings.ContainsRune(s, 0) {
				// A storage limit, not a content rule: Postgres text cannot hold
				// U+0000, so accepting it would turn into a 500 at insert time.
				problems = append(problems, name+" must not contain the NUL character (U+0000); it cannot be stored")
			} else {
				*dst = s
				return true
			}
		}
		return false
	}

	str("seller_id", &f.SellerID)
	str("category_id", &f.CategoryID)
	if str("title", &f.Title) {
		// Length only, as in the schema (minLength 1, maxLength 200). The text is
		// not trimmed or otherwise judged: it is stored and returned as sent.
		switch n := utf8.RuneCountInString(f.Title); {
		case n < 1:
			problems = append(problems, "title must be at least 1 character")
		case n > MaxTitleChars:
			problems = append(problems, fmt.Sprintf("title must be at most %d characters", MaxTitleChars))
		}
	}
	if str("description", &f.Description) && utf8.RuneCountInString(f.Description) > MaxDescriptionChars {
		problems = append(problems, fmt.Sprintf("description must be at most %d characters", MaxDescriptionChars))
	}

	rawPrice, hasPrice := obj["price_vnd"]
	if string(bytes.TrimSpace(rawPrice)) == "-0" {
		// Negative zero is the integer 0: not negative in value, so valid.
		rawPrice = json.RawMessage("0")
	}
	switch raw, ok := rawPrice, hasPrice; {
	case !ok:
		problems = append(problems, "price_vnd is required")
	case isNull(raw):
		problems = append(problems, "price_vnd must not be null")
	case !isPlainInteger(raw):
		problems = append(problems, "price_vnd must be a non-negative integer (digits only)")
	default:
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			problems = append(problems, "price_vnd is too large (maximum 9223372036854775807)")
		} else {
			f.PriceVND = n
		}
	}

	if len(problems) > 0 {
		return Fields{}, &InvalidError{Problems: problems}
	}
	return f, nil
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// isPlainInteger reports whether raw is only ASCII digits. JSON syntax has
// already been validated, so this excludes "-1", "1.0", "1e3" and strings.
func isPlainInteger(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return false
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// checkDocument walks the whole document once. It rejects malformed JSON,
// duplicate keys at any depth, excessive nesting, and anything after the first
// value. Numbers are read with UseNumber so they are never converted to float.
func checkDocument(body []byte) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := walk(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return invalid("unexpected data after the JSON object")
	}
	return nil
}

func walk(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return invalid("JSON is nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return invalid("malformed JSON: %v", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // a scalar
	}
	switch delim {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return invalid("malformed JSON: %v", err)
			}
			key, _ := keyTok.(string)
			if _, dup := seen[key]; dup {
				return invalid("duplicate key %q in JSON object", truncate(key, 64))
			}
			seen[key] = struct{}{}
			if err := walk(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walk(dec, depth+1); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil { // the closing } or ]
		return invalid("malformed JSON: %v", err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
