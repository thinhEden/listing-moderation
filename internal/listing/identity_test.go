package listing

import (
	"bytes"
	"testing"
)

func base() Fields {
	return Fields{
		SellerID:    "s-1002",
		Title:       "iPhone 13 128GB",
		Description: "Like new, with box.",
		PriceVND:    6_000_000,
		CategoryID:  "c-phones",
	}
}

func with(mut func(*Fields)) Fields {
	f := base()
	mut(&f)
	return f
}

func TestNormalizeText(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"trim":                  {"  hello  ", "hello"},
		"case":                  {"HeLLo", "hello"},
		"spaces collapse":       {"a   b", "a b"},
		"tab":                   {"a\tb", "a b"},
		"newline":               {"a\r\n\nb", "a b"},
		"nbsp":                  {"a  b", "a b"},
		"ideographic space":     {"a　b", "a b"},
		"mixed whitespace":      {" \t a  \n b \t", "a b"},
		"empty":                 {"", ""},
		"only whitespace":       {" \t\n ", ""},
		"vietnamese case":       {"ĐIỆN THOẠI", "điện thoại"},
		"nfd becomes nfc":       {"à", "à"}, // a + combining grave -> à
		"diacritics are kept":   {"hàng", "hàng"},
		"punctuation untouched": {"A-B, c.", "a-b, c."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeText(tc.in); got != tc.want {
				t.Fatalf("NormalizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestDupFingerprintTreatsThese_AsTheSameListing(t *testing.T) {
	ref := DupFingerprint(base())
	same := map[string]Fields{
		"different price":     with(func(f *Fields) { f.PriceVND = 1 }),
		"zero price":          with(func(f *Fields) { f.PriceVND = 0 }),
		"title case":          with(func(f *Fields) { f.Title = "IPHONE 13 128gb" }),
		"title spacing":       with(func(f *Fields) { f.Title = "  iPhone   13\t128GB " }),
		"title nbsp":          with(func(f *Fields) { f.Title = "iPhone 13 128GB" }),
		"description newline": with(func(f *Fields) { f.Description = "Like new,\nwith   box." }),
		"description case":    with(func(f *Fields) { f.Description = "LIKE NEW, WITH BOX." }),
	}
	for name, f := range same {
		t.Run(name, func(t *testing.T) {
			if !bytes.Equal(ref, DupFingerprint(f)) {
				t.Fatalf("expected the same fingerprint for %+v", f)
			}
		})
	}
}

func TestDupFingerprintTreatsThese_AsDifferentListings(t *testing.T) {
	ref := DupFingerprint(base())
	differ := map[string]Fields{
		"category":          with(func(f *Fields) { f.CategoryID = "c-laptops" }),
		"title word":        with(func(f *Fields) { f.Title = "iPhone 14 128GB" }),
		"description word":  with(func(f *Fields) { f.Description = "Like new, no box." }),
		"title punctuation": with(func(f *Fields) { f.Title = "iPhone 13, 128GB" }),
		"empty description": with(func(f *Fields) { f.Description = "" }),
	}
	for name, f := range differ {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(ref, DupFingerprint(f)) {
				t.Fatalf("expected a different fingerprint for %+v", f)
			}
		})
	}
}

func TestDupFingerprintKeepsDiacriticsButUnifiesUnicodeForms(t *testing.T) {
	a := with(func(f *Fields) { f.Title = "Hàng đẹp" })  // precomposed
	b := with(func(f *Fields) { f.Title = "Hàng đẹp" }) // a + combining grave
	c := with(func(f *Fields) { f.Title = "Hang dep" })
	if !bytes.Equal(DupFingerprint(a), DupFingerprint(b)) {
		t.Fatal("NFC and NFD spellings of the same text must be duplicates")
	}
	if bytes.Equal(DupFingerprint(a), DupFingerprint(c)) {
		t.Fatal("text with and without diacritics must not be treated as duplicates")
	}
}

// Moving text from one field to the other must not produce the same hash.
func TestDupFingerprintFieldBoundaries(t *testing.T) {
	a := with(func(f *Fields) { f.Title, f.Description = "ab", "c" })
	b := with(func(f *Fields) { f.Title, f.Description = "a", "bc" })
	c := with(func(f *Fields) { f.Title, f.Description = "c", "ab" })
	if bytes.Equal(DupFingerprint(a), DupFingerprint(b)) || bytes.Equal(DupFingerprint(a), DupFingerprint(c)) {
		t.Fatal("field boundaries are ambiguous")
	}
}

func TestDupFingerprintIgnoresSeller(t *testing.T) {
	other := with(func(f *Fields) { f.SellerID = "s-1003" })
	if !bytes.Equal(DupFingerprint(base()), DupFingerprint(other)) {
		t.Fatal("the seller is part of the database key, not of the fingerprint")
	}
}

func TestRequestHashIsStableForEqualValues(t *testing.T) {
	if !bytes.Equal(RequestHash(base()), RequestHash(base())) {
		t.Fatal("hash is not deterministic")
	}
	if n := len(RequestHash(base())); n != 32 {
		t.Fatalf("hash length %d", n)
	}
}

// Every business field takes part, and nothing is normalised.
func TestRequestHashDiffersWhenAnyValueDiffers(t *testing.T) {
	ref := RequestHash(base())
	differ := map[string]Fields{
		"seller":      with(func(f *Fields) { f.SellerID = "s-1003" }),
		"title":       with(func(f *Fields) { f.Title = "iPhone 13 256GB" }),
		"title case":  with(func(f *Fields) { f.Title = "iphone 13 128GB" }),
		"title space": with(func(f *Fields) { f.Title = "iPhone 13  128GB" }),
		"description": with(func(f *Fields) { f.Description = "Like new" }),
		"price":       with(func(f *Fields) { f.PriceVND = 6_000_001 }),
		"category":    with(func(f *Fields) { f.CategoryID = "c-laptops" }),
		"trailing ws": with(func(f *Fields) { f.Description = "Like new, with box. " }),
	}
	for name, f := range differ {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(ref, RequestHash(f)) {
				t.Fatalf("hash ignored a change in %s", name)
			}
		})
	}
}

func TestRequestHashDistinguishesNFCFromNFD(t *testing.T) {
	nfc := with(func(f *Fields) { f.Title = "Hàng" })
	nfd := with(func(f *Fields) { f.Title = "Hàng" })
	if bytes.Equal(RequestHash(nfc), RequestHash(nfd)) {
		t.Fatal("NFC and NFD must differ for idempotency (reusing a key with the other form is a 409)")
	}
}

func TestRequestHashFieldBoundaries(t *testing.T) {
	a := with(func(f *Fields) { f.Title, f.Description = "ab", "c" })
	b := with(func(f *Fields) { f.Title, f.Description = "a", "bc" })
	if bytes.Equal(RequestHash(a), RequestHash(b)) {
		t.Fatal("field boundaries are ambiguous")
	}
}

func TestRequestHashHandlesExtremePrices(t *testing.T) {
	lo := with(func(f *Fields) { f.PriceVND = 0 })
	hi := with(func(f *Fields) { f.PriceVND = 1<<63 - 1 })
	if bytes.Equal(RequestHash(lo), RequestHash(hi)) {
		t.Fatal("extreme prices collide")
	}
}
