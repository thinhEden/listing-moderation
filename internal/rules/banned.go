package rules

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Match is one banned term found in one field.
type Match struct {
	Field string `json:"field"` // "title" or "description"
	Term  string `json:"term"`  // spelled as in seed/banned_keywords.json
}

// BannedMatcher implements R1. It is built once at startup and is safe for
// concurrent use.
type BannedMatcher struct {
	terms []bannedTerm
}

type bannedTerm struct {
	term string
	re   *regexp.Regexp
}

// notWord is a character that cannot be part of a word: anything that is not a
// letter, combining mark, number or underscore. Go's \b only understands ASCII
// words, so Vietnamese letters need this explicit class.
const notWord = `[^\p{L}\p{M}\p{N}_]`

// NewBannedMatcher compiles the seed terms. Each term is matched as a whole
// phrase, case-insensitively, exactly as spelled (a space in the term is one
// space in the text; no diacritic stripping, no \s+).
//
// A word boundary is required on a side only when the term has a word
// character at that edge, so "fake" does not match inside "fakes" or
// "unfake", but does match in "fake-phone" and "(fake)".
func NewBannedMatcher(terms []string) (*BannedMatcher, error) {
	m := &BannedMatcher{}
	for _, term := range terms {
		if strings.TrimSpace(term) == "" {
			return nil, fmt.Errorf("banned keyword is blank")
		}
		first, _ := utf8.DecodeRuneInString(term)
		last, _ := utf8.DecodeLastRuneInString(term)

		pattern := "(?i)"
		if isWordRune(first) {
			pattern += "(?:^|" + notWord + ")"
		}
		pattern += regexp.QuoteMeta(term)
		if isWordRune(last) {
			pattern += "(?:$|" + notWord + ")"
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("compile banned keyword %q: %w", term, err)
		}
		m.terms = append(m.terms, bannedTerm{term: term, re: re})
	}
	return m, nil
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r)
}

// Check returns every banned term found in the title or description, in seed
// order and title before description. An empty result means R1 did not fire.
func (m *BannedMatcher) Check(title, description string) []Match {
	var out []Match
	for _, t := range m.terms {
		if t.re.MatchString(title) {
			out = append(out, Match{Field: "title", Term: t.term})
		}
		if t.re.MatchString(description) {
			out = append(out, Match{Field: "description", Term: t.term})
		}
	}
	return out
}

// BannedReason builds the R1 reason. The message quotes each matched term as
// spelled in the seed, which the API contract requires.
func BannedReason(matches []Match) Reason {
	parts := make([]string, len(matches))
	for i, m := range matches {
		// Not %q: it would escape quotes or backslashes inside the term, and the
		// contract wants the term verbatim.
		parts[i] = fmt.Sprintf(`%s contains banned term "%s"`, m.Field, m.Term)
	}
	return Reason{
		RuleID:  RuleBanned,
		Message: strings.Join(parts, "; "),
		Detail:  map[string]any{"matches": matches},
	}
}
