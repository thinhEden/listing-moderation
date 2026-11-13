package seed

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The repository's own seed/ must always load: it is what the service ships with.
func TestLoadRealSeed(t *testing.T) {
	d, err := Load(filepath.Join("..", "..", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sellers) != 8 || len(d.Categories) != 5 || len(d.Keywords) != 6 {
		t.Fatalf("unexpected sizes: %d sellers, %d categories, %d keywords",
			len(d.Sellers), len(d.Categories), len(d.Keywords))
	}
	if s := d.Sellers["s-1006"]; s.TrustScore != 80 || !s.Verified {
		t.Fatalf("s-1006 should be trust 80, verified: %+v", s)
	}
	if c := d.Categories["c-motorbikes"]; c.MedianPriceVND != 32_000_000 {
		t.Fatalf("c-motorbikes median: %+v", c)
	}
	if d.Keywords[4] != "hang gia" {
		t.Fatalf("keyword order/spelling not preserved: %q", d.Keywords)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, sellers, categories, keywords string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "sellers.json", sellers)
	write(t, dir, "categories.json", categories)
	write(t, dir, "banned_keywords.json", keywords)
	return dir
}

const (
	okSellers    = `[{"id":"s-1","trust_score":50,"verified":true}]`
	okCategories = `[{"id":"c-1","name":"x","median_price_vnd":1000}]`
	okKeywords   = `["fake"]`
)

func TestLoadAcceptsUnknownFields(t *testing.T) {
	dir := fixture(t,
		`[{"id":"s-1","trust_score":50,"verified":true,"note":"x","extra":{"a":[1]}}]`,
		okCategories, okKeywords)
	if _, err := Load(dir); err != nil {
		t.Fatal(err)
	}
}

func TestLoadRejectsInvalidSeed(t *testing.T) {
	maxOK := int64(MaxMedianVND)
	cases := map[string]struct {
		sellers, categories, keywords, want string
	}{
		"missing file":    {"", "", "", "read seed"},
		"malformed json":  {`[{`, okCategories, okKeywords, "parse"},
		"trailing data":   {okSellers + ` []`, okCategories, okKeywords, "parse"},
		"empty seller id": {`[{"id":"","trust_score":1}]`, okCategories, okKeywords, "empty id"},
		"duplicate seller": {
			`[{"id":"s-1","trust_score":1},{"id":"s-1","trust_score":2}]`, okCategories, okKeywords, "duplicate id"},
		"zero median": {
			okSellers, `[{"id":"c-1","median_price_vnd":0}]`, okKeywords, "median_price_vnd"},
		"negative median": {
			okSellers, `[{"id":"c-1","median_price_vnd":-5}]`, okKeywords, "median_price_vnd"},
		"median could overflow x3": {
			okSellers, `[{"id":"c-1","median_price_vnd":` + strconv.FormatInt(maxOK+1, 10) + `}]`, okKeywords, "median_price_vnd"},
		"duplicate category": {
			okSellers, `[{"id":"c-1","median_price_vnd":1},{"id":"c-1","median_price_vnd":2}]`, okKeywords, "duplicate id"},
		"blank keyword": {okSellers, okCategories, `["fake","  "]`, "blank keyword"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if name != "missing file" {
				write(t, dir, "sellers.json", tc.sellers)
				write(t, dir, "categories.json", tc.categories)
				write(t, dir, "banned_keywords.json", tc.keywords)
			}
			_, err := Load(dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestLoadAcceptsLargestSafeMedian(t *testing.T) {
	dir := fixture(t, okSellers,
		`[{"id":"c-1","median_price_vnd":`+strconv.FormatInt(int64(MaxMedianVND), 10)+`}]`, okKeywords)
	d, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m := d.Categories["c-1"].MedianPriceVND; m*3 > math.MaxInt64 || m*3 < 0 {
		t.Fatalf("median*3 overflowed: %d", m)
	}
}
