package rules

import (
	"math"
	"math/big"
	"math/rand"
	"strings"
	"testing"
)

// reference computes R2 with arbitrary-precision integers, so it cannot overflow.
func reference(price, median int64) (string, bool) {
	p, m := big.NewInt(price), big.NewInt(median)
	three := big.NewInt(3)
	if p.Cmp(new(big.Int).Mul(m, three)) > 0 {
		return PriceAbove, true
	}
	if new(big.Int).Mul(p, three).Cmp(m) < 0 {
		return PriceBelow, true
	}
	return "", false
}

func TestCheckPriceBoundaries(t *testing.T) {
	const (
		phones     = 6_000_000  // 3x = 18,000,000; 1/3 = 2,000,000 exactly
		motorbikes = 32_000_000 // 3x = 96,000,000; 1/3 = 10,666,666.67
	)
	cases := []struct {
		name          string
		price, median int64
		want          string // "" = R2 does not fire
	}{
		{"equal to median", phones, phones, ""},
		{"exactly 3x is allowed", 18_000_000, phones, ""},
		{"one over 3x", 18_000_001, phones, PriceAbove},
		{"just under 3x", 17_999_999, phones, ""},
		{"exactly one third is allowed", 2_000_000, phones, ""},
		{"one under one third", 1_999_999, phones, PriceBelow},
		{"just over one third", 2_000_001, phones, ""},

		{"3x of a non-multiple median", 96_000_000, motorbikes, ""},
		{"one over 3x of motorbikes", 96_000_001, motorbikes, PriceAbove},
		// 32,000,000 / 3 is not whole: 10,666,667 * 3 = 32,000,001 > median.
		{"first price not below a third", 10_666_667, motorbikes, ""},
		{"last price below a third", 10_666_666, motorbikes, PriceBelow},

		{"free listing", 0, phones, PriceBelow},
		{"free listing, tiny median", 0, 1, PriceBelow},
		{"price 1, median 1", 1, 1, ""},
		{"median 1: 3 is exactly 3x", 3, 1, ""},
		{"median 1: 4 is over 3x", 4, 1, PriceAbove},
		{"median 2: price 0 is below a third", 0, 2, PriceBelow},
		{"median 2: price 1 is not (3 > 2)", 1, 2, ""},
		{"median 3: price 1 is exactly a third", 1, 3, ""},
		{"median 3: price 0 is below", 0, 3, PriceBelow},

		// price_vnd has no upper bound: nothing here may overflow.
		{"max price, small median", math.MaxInt64, 6_000_000, PriceAbove},
		{"max price, median 1", math.MaxInt64, 1, PriceAbove},
		{"max price, max median", math.MaxInt64, math.MaxInt64, ""},
		{"max price, median = max/3 (3*median = max-1)", math.MaxInt64, math.MaxInt64 / 3, PriceAbove},
		{"price = 3*median at the top of the range", 3 * (math.MaxInt64 / 3), math.MaxInt64 / 3, ""},
		{"price just over 3*median at the top", 3*(math.MaxInt64/3) + 1, math.MaxInt64 / 3, PriceAbove},
		// MaxInt64 = 3*(MaxInt64/3) + 1, so 3*(MaxInt64/3) is still below the median.
		{"huge median: max/3 is still below a third", math.MaxInt64 / 3, math.MaxInt64, PriceBelow},
		{"huge median: max/3 + 1 is the first price not below", math.MaxInt64/3 + 1, math.MaxInt64, ""},
		{"huge median, price 0", 0, math.MaxInt64, PriceBelow},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, fired := CheckPrice(tc.price, tc.median)
			if fired != (tc.want != "") || got.Direction != tc.want {
				t.Fatalf("CheckPrice(%d, %d) = (%q, %v), want %q", tc.price, tc.median, got.Direction, fired, tc.want)
			}
			if fired && (got.Price != tc.price || got.Median != tc.median) {
				t.Fatalf("finding carries wrong numbers: %+v", got)
			}
			// Every case must also agree with the overflow-proof reference.
			if want, wantFired := reference(tc.price, tc.median); want != got.Direction || wantFired != fired {
				t.Fatalf("reference says (%q, %v)", want, wantFired)
			}
		})
	}
}

// Randomised comparison with big.Int, biased towards the edges where bugs live.
func TestCheckPriceMatchesBigIntReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	pick := func() int64 {
		switch rng.Intn(5) {
		case 0:
			return rng.Int63() // anywhere in range
		case 1:
			return int64(rng.Intn(100)) // tiny
		case 2:
			return math.MaxInt64 - int64(rng.Intn(10)) // near the top
		case 3:
			return math.MaxInt64/3 + int64(rng.Intn(7)) - 3 // around max/3
		default:
			return int64(rng.Intn(100_000_000)) // realistic prices
		}
	}
	for i := 0; i < 200_000; i++ {
		price, median := pick(), pick()
		if median <= 0 {
			median = 1
		}
		got, fired := CheckPrice(price, median)
		want, wantFired := reference(price, median)
		if fired != wantFired || got.Direction != want {
			t.Fatalf("CheckPrice(%d, %d) = (%q, %v), reference (%q, %v)", price, median, got.Direction, fired, want, wantFired)
		}
	}

	// Exhaustively around the edges (median/3, median, 3*median) of awkward medians.
	for _, median := range []int64{1, 2, 3, 4, 5, 6, 7, 1000, 1001, 1002, 32_000_000, math.MaxInt64 / 3, math.MaxInt64/3 - 1, math.MaxInt64} {
		centres := []int64{median / 3, median/3 + 1, median}
		if median <= math.MaxInt64/3 {
			centres = append(centres, 3*median)
		}
		for _, centre := range centres {
			for d := int64(-3); d <= 3; d++ {
				price := centre + d
				if price < 0 { // below zero, or wrapped past MaxInt64: not a valid price
					continue
				}
				got, fired := CheckPrice(price, median)
				want, wantFired := reference(price, median)
				if fired != wantFired || got.Direction != want {
					t.Fatalf("edge CheckPrice(%d, %d) = (%q, %v), reference (%q, %v)", price, median, got.Direction, fired, want, wantFired)
				}
			}
		}
	}
}

func TestPriceReason(t *testing.T) {
	above := PriceReason(PriceFinding{PriceAbove, 30_000_000, 6_000_000}, "c-phones")
	if above.RuleID != "R2" || !strings.Contains(above.Message, "more than 3x") || !strings.Contains(above.Message, "30000000") {
		t.Fatalf("above: %+v", above)
	}
	below := PriceReason(PriceFinding{PriceBelow, 100, 6_000_000}, "c-phones")
	if !strings.Contains(below.Message, "less than one third") {
		t.Fatalf("below: %+v", below)
	}
	if below.Detail["category_id"] != "c-phones" || below.Detail["median_price_vnd"] != int64(6_000_000) || below.Detail["direction"] != PriceBelow {
		t.Fatalf("detail: %v", below.Detail)
	}
}
