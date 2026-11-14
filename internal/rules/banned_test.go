package rules

import (
	"reflect"
	"strings"
	"testing"
)

var seedTerms = []string{"replica", "fake", "counterfeit", "stolen", "hang gia", "hang nhai"}

func matcher(t *testing.T, terms ...string) *BannedMatcher {
	t.Helper()
	m, err := NewBannedMatcher(terms)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBannedMatchesWholeWordsOnly(t *testing.T) {
	m := matcher(t, seedTerms...)
	cases := []struct {
		name  string
		title string
		want  []Match // nil = R1 must not fire
	}{
		{"exact word", "replica", []Match{{"title", "replica"}}},
		{"in a sentence", "Brand new replica watch", []Match{{"title", "replica"}}},
		{"upper case", "REPLICA WATCH", []Match{{"title", "replica"}}},
		{"mixed case", "RePlIcA", []Match{{"title", "replica"}}},
		{"start of text", "fake phone", []Match{{"title", "fake"}}},
		{"end of text", "phone fake", []Match{{"title", "fake"}}},
		{"hyphen is a boundary", "fake-phone", []Match{{"title", "fake"}}},
		{"punctuation is a boundary", "(fake), really", []Match{{"title", "fake"}}},
		{"digit-free neighbour", "fake!", []Match{{"title", "fake"}}},

		{"plural does not match", "fakes", nil},
		{"prefix letters", "unfake", nil},
		{"suffix letters", "fakery", nil},
		{"embedded", "snowfakeman", nil},
		{"digit neighbour", "fake2", nil},
		{"digit before", "2fake", nil},
		{"underscore neighbour", "fake_phone", nil},
		{"vietnamese letter after", "fakeđ", nil},
		{"vietnamese letter before", "đfake", nil},
		{"combining mark after", "faké", nil}, // fake + combining acute: "faké"

		{"multi-word term", "Hang gia gia re", []Match{{"title", "hang gia"}}},
		{"multi-word upper", "HANG GIA", []Match{{"title", "hang gia"}}},
		{"multi-word needs exactly one space", "hang  gia", nil},
		{"multi-word with newline does not match", "hang\ngia", nil},
		{"multi-word with tab does not match", "hang\tgia", nil},
		{"part of multi-word alone", "hang", nil},
		{"multi-word inside longer word", "shang gia", nil},

		{"diacritics are not stripped", "hàng giả", nil},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.Check(tc.title, "")
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Check(%q) = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}

func TestBannedChecksDescriptionAndReportsEveryMatch(t *testing.T) {
	m := matcher(t, seedTerms...)

	got := m.Check("Nice phone", "this is a counterfeit")
	if want := []Match{{"description", "counterfeit"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	got = m.Check("fake phone", "stolen and fake")
	want := []Match{
		{"title", "fake"},
		{"description", "fake"},
		{"description", "stolen"},
	}
	// Seed order first (replica, fake, counterfeit, stolen, ...), title before description.
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestBannedMessageQuotesTheSeedTerm(t *testing.T) {
	m := matcher(t, seedTerms...)
	cases := []struct{ title, term string }{
		{"REPLICA bag", "replica"},
		{"cheap Hang Gia shoes", "hang gia"}, // text spells it differently; the seed spelling is quoted
		{"Hang NHAI", "hang nhai"},
	}
	for _, tc := range cases {
		matches := m.Check(tc.title, "")
		if len(matches) != 1 {
			t.Fatalf("%q: matches %v", tc.title, matches)
		}
		r := BannedReason(matches)
		if r.RuleID != "R1" {
			t.Fatalf("rule id %q", r.RuleID)
		}
		// Contract: the message contains the term verbatim apart from letter case.
		if !strings.Contains(strings.ToLower(r.Message), tc.term) {
			t.Fatalf("message %q does not contain %q", r.Message, tc.term)
		}
		if !strings.Contains(r.Message, `title contains banned term "`+tc.term+`"`) {
			t.Fatalf("unexpected message %q", r.Message)
		}
	}
}

func TestBannedMessageListsEveryMatch(t *testing.T) {
	m := matcher(t, seedTerms...)
	r := BannedReason(m.Check("fake", "stolen"))
	for _, want := range []string{`title contains banned term "fake"`, `description contains banned term "stolen"`} {
		if !strings.Contains(r.Message, want) {
			t.Fatalf("message %q missing %q", r.Message, want)
		}
	}
	if got := r.Detail["matches"].([]Match); len(got) != 2 {
		t.Fatalf("detail matches: %v", got)
	}
}

// Terms are literal text, never regular expressions.
func TestBannedTermsAreLiteral(t *testing.T) {
	m := matcher(t, "a.b", "c++", "(x)", `back\slash`, `say "hi"`)

	for _, tc := range []struct {
		text string
		want bool
	}{
		{"a.b", true},
		{"axb", false}, // "." is not a wildcard
		{"buy c++ now", true},
		{"c++x", true}, // the term ends in "+", which needs no boundary after it
		{"xc++", false},
		{"cc", false},
		{"see (x) here", true},
		{`back\slash`, true},
		{`say "hi" now`, true},
	} {
		if got := len(m.Check(tc.text, "")) > 0; got != tc.want {
			t.Errorf("Check(%q) matched=%v, want %v", tc.text, got, tc.want)
		}
	}

	// The message keeps quotes and backslashes in the term as they are.
	r := BannedReason(m.Check(`say "hi" now`, ""))
	if !strings.Contains(r.Message, `"say "hi""`) {
		t.Fatalf("term was escaped in the message: %q", r.Message)
	}
}

func TestBannedUnicodeKeywordsAndCaseFolding(t *testing.T) {
	m := matcher(t, "hàng giả", "ĐỒ GIẢ")
	if len(m.Check("Bán hàng giả giá rẻ", "")) != 1 {
		t.Fatal("accented keyword should match accented text")
	}
	if len(m.Check("BÁN HÀNG GIẢ", "")) != 1 {
		t.Fatal("accented keyword should match regardless of case")
	}
	if len(m.Check("đồ giả", "")) != 1 {
		t.Fatal("upper-case seed term should match lower-case text")
	}
	if len(m.Check("hang gia", "")) != 0 {
		t.Fatal("unaccented text must not match an accented keyword")
	}
}

func TestBannedNoKeywordsNeverFires(t *testing.T) {
	m := matcher(t)
	if got := m.Check("anything fake", "stolen"); got != nil {
		t.Fatalf("got %v", got)
	}
}

func TestNewBannedMatcherRejectsBlankTerms(t *testing.T) {
	for _, terms := range [][]string{{""}, {"ok", "  "}, {"\t"}} {
		if _, err := NewBannedMatcher(terms); err == nil {
			t.Fatalf("expected an error for %q", terms)
		}
	}
}
