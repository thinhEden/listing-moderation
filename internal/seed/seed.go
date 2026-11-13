// Package seed loads and validates the reference data the rules depend on:
// sellers, categories and banned keywords. It is read-only after Load.
package seed

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

type Seller struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	TrustScore int    `json:"trust_score"`
	Verified   bool   `json:"verified"`
}

type Category struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	MedianPriceVND int64  `json:"median_price_vnd"`
}

// Data is the validated seed set. Keywords keep their original spelling and
// order because R1 messages must quote the term as it appears in the seed.
type Data struct {
	Sellers    map[string]Seller
	Categories map[string]Category
	Keywords   []string
}

// MaxMedianVND is the largest category median accepted. R2 multiplies prices
// and medians by 3, so anything above MaxInt64/3 could overflow.
const MaxMedianVND = math.MaxInt64 / 3

// Load reads and validates the three seed files from dir. Any problem is an
// error: the service must not start with reference data it cannot trust.
func Load(dir string) (*Data, error) {
	var (
		sellers    []Seller
		categories []Category
		keywords   []string
	)
	for file, dst := range map[string]any{
		"sellers.json":         &sellers,
		"categories.json":      &categories,
		"banned_keywords.json": &keywords,
	} {
		if err := readJSON(filepath.Join(dir, file), dst); err != nil {
			return nil, err
		}
	}

	d := &Data{
		Sellers:    make(map[string]Seller, len(sellers)),
		Categories: make(map[string]Category, len(categories)),
		Keywords:   keywords,
	}
	var errs []error

	for i, s := range sellers {
		switch {
		case s.ID == "":
			errs = append(errs, fmt.Errorf("sellers.json[%d]: empty id", i))
		default:
			if _, dup := d.Sellers[s.ID]; dup {
				errs = append(errs, fmt.Errorf("sellers.json[%d]: duplicate id %q", i, s.ID))
				continue
			}
			d.Sellers[s.ID] = s
		}
	}
	for i, c := range categories {
		switch {
		case c.ID == "":
			errs = append(errs, fmt.Errorf("categories.json[%d]: empty id", i))
		case c.MedianPriceVND <= 0 || c.MedianPriceVND > MaxMedianVND:
			errs = append(errs, fmt.Errorf("categories.json[%d] %q: median_price_vnd %d must be in (0, %d]",
				i, c.ID, c.MedianPriceVND, int64(MaxMedianVND)))
		default:
			if _, dup := d.Categories[c.ID]; dup {
				errs = append(errs, fmt.Errorf("categories.json[%d]: duplicate id %q", i, c.ID))
				continue
			}
			d.Categories[c.ID] = c
		}
	}
	for i, k := range keywords {
		// An empty or blank term would match every listing.
		if strings.TrimSpace(k) == "" {
			errs = append(errs, fmt.Errorf("banned_keywords.json[%d]: blank keyword", i))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("invalid seed in %s: %w", dir, err)
	}
	return d, nil
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read seed: %w", err)
	}
	// Unmarshal rejects trailing data after the top-level value. Unknown
	// fields such as "note" are deliberately tolerated.
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
