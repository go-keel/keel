package types

import "github.com/shopspring/decimal"

// The three functions below convert between decimal.Decimal and the decimal string on the API (the proto
// `keel.v1.decimal` rule). They handle only strings and numbers: an `optional` field is *string in Go; read it with
// the generated GetXxx() (an empty string when not sent), and the caller takes the address when writing.

// DecimalString converts a number to the decimal string for an API field.
//
// The output is exact, not truncated to the rule's 8 decimal places: the rule only guards requests, and the number itself decides how many places a response carries.
func DecimalString(d decimal.Decimal) string {
	return d.String()
}

// ParseDecimal parses the decimal string of an API field; an empty or unparseable string returns an error.
//
// The shape is enforced by the proto rule in the validation middleware; this does not repeat that regex, so strings
// that skipped validation (raw routes, internal calls) parse in a wider range than the rule: scientific notation and
// a plus sign are accepted.
func ParseDecimal(s string) (decimal.Decimal, error) {
	return decimal.NewFromString(s)
}

// DecimalOrZero is like ParseDecimal, but returns 0 for an empty or unparseable string.
//
// Use it only where "missing means 0" is genuinely the business meaning; elsewhere use ParseDecimal, so a missing
// number does not quietly become 0.
func DecimalOrZero(s string) decimal.Decimal {
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}
