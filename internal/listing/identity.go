package listing

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// RequestHash identifies a submission for idempotency. It covers the five
// business fields as decoded, verbatim: a change of case, price, or Unicode
// form (NFC vs NFD) gives a different hash. Field order and JSON syntax of the
// original body do not matter because the hash is taken from decoded values.
// Unknown fields are not part of Fields and so never affect it.
func RequestHash(f Fields) []byte {
	h := sha256.New()
	h.Write([]byte("req/v1"))
	writeString(h, f.SellerID)
	writeString(h, f.Title)
	writeString(h, f.Description)
	writeInt(h, f.PriceVND)
	writeString(h, f.CategoryID)
	return h.Sum(nil)
}

// DupFingerprint identifies a listing's content for R3. Two listings from the
// same seller with the same fingerprint are duplicates. It covers category,
// title and description, but not price: changing only the price is still the
// same item for sale.
//
// The seller is deliberately not part of the hash: uniqueness is enforced per
// (seller_id, dup_fingerprint), so the seller is already in the key.
//
// Title and description are normalised with NormalizeText. Category ids are
// compared exactly. If the normalisation ever changes, bump the version prefix
// and recompute stored fingerprints.
func DupFingerprint(f Fields) []byte {
	h := sha256.New()
	h.Write([]byte("dup/v1"))
	writeString(h, f.CategoryID)
	writeString(h, NormalizeText(f.Title))
	writeString(h, NormalizeText(f.Description))
	return h.Sum(nil)
}

// NormalizeText is the R3 comparison form of free text: Unicode NFC, lower
// case, every run of Unicode white space (spaces, tabs, newlines, NBSP, ...)
// collapsed to one space, and the ends trimmed. It does not strip diacritics
// or do any fuzzy matching, and its result is only for comparison; the stored
// and returned text stays as submitted.
func NormalizeText(s string) string {
	s = norm.NFC.String(strings.ToLower(norm.NFC.String(s)))
	return strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
}

type byteWriter interface{ Write([]byte) (int, error) }

// writeString writes a length prefix before the bytes so that field
// boundaries are unambiguous: ("ab", "c") and ("a", "bc") hash differently.
func writeString(w byteWriter, s string) {
	writeInt(w, int64(len(s)))
	_, _ = w.Write([]byte(s))
}

func writeInt(w byteWriter, n int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(n))
	_, _ = w.Write(b[:])
}
