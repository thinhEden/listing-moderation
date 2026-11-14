package rules

import "fmt"

// Direction of a price anomaly relative to the category median.
const (
	PriceAbove = "above" // more than 3x the median
	PriceBelow = "below" // less than one third of the median
)

// PriceFinding describes why R2 fired.
type PriceFinding struct {
	Direction string
	Price     int64
	Median    int64
}

// CheckPrice implements R2: a price strictly more than 3x, or strictly less
// than one third of, the category median is anomalous. Exactly 3x and exactly
// 1/3 are not.
//
// price_vnd has no upper bound in the API, so neither comparison multiplies:
//
//	price > 3*median   <=>  price/3 > median, or price/3 == median with a remainder
//	price < median/3   <=>  price < ceil(median/3)
//
// Both are exact for every int64 pair, with no overflow. The caller must pass
// median > 0 (seed validation guarantees it).
func CheckPrice(price, median int64) (PriceFinding, bool) {
	q, r := price/3, price%3
	if q > median || (q == median && r > 0) {
		return PriceFinding{PriceAbove, price, median}, true
	}

	ceilThird := median / 3
	if median%3 != 0 {
		ceilThird++
	}
	if price < ceilThird {
		return PriceFinding{PriceBelow, price, median}, true
	}
	return PriceFinding{}, false
}

// PriceReason builds the R2 reason.
func PriceReason(f PriceFinding, categoryID string) Reason {
	var msg string
	if f.Direction == PriceAbove {
		msg = fmt.Sprintf("price %d VND is more than 3x the category median of %d VND", f.Price, f.Median)
	} else {
		msg = fmt.Sprintf("price %d VND is less than one third of the category median of %d VND", f.Price, f.Median)
	}
	return Reason{
		RuleID:  RulePrice,
		Message: msg,
		Detail: map[string]any{
			"direction":        f.Direction,
			"price_vnd":        f.Price,
			"median_price_vnd": f.Median,
			"category_id":      categoryID,
		},
	}
}
