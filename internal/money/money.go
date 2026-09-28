// Package money keeps all internal amounts in integer minor units (paise for INR).
package money

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// Minor is an amount in minor units, e.g. 700000 == ₹7,000.
type Minor int64

// ParseMajor converts a provider-supplied major-unit value ("7000", 7000, 7000.50)
// into minor units without going through float64.
func ParseMajor(v any) (Minor, error) {
	var s string
	switch t := v.(type) {
	case nil:
		return 0, fmt.Errorf("empty amount")
	case string:
		s = strings.TrimSpace(t)
	case json.Number:
		s = t.String()
	case float64:
		s = strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		s = strconv.Itoa(t)
	case int64:
		s = strconv.FormatInt(t, 10)
	default:
		return 0, fmt.Errorf("unsupported amount type %T", v)
	}
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	s = strings.ReplaceAll(s, ",", "")

	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	r.Mul(r, big.NewRat(100, 1))
	if !r.IsInt() {
		// Round half-up to the nearest minor unit.
		r.Add(r, big.NewRat(1, 2))
	}
	return Minor(new(big.Int).Div(r.Num(), r.Denom()).Int64()), nil
}

// Major renders minor units as a decimal string suitable for JSON output.
func (m Minor) Major() json.Number {
	neg := ""
	v := int64(m)
	if v < 0 {
		neg, v = "-", -v
	}
	if v%100 == 0 {
		return json.Number(fmt.Sprintf("%s%d", neg, v/100))
	}
	return json.Number(strings.TrimRight(fmt.Sprintf("%s%d.%02d", neg, v/100, v%100), "0"))
}

// ApplyBps applies a basis-point percentage, rounding down so we never over-reward.
func (m Minor) ApplyBps(bps int32) Minor {
	return Minor(int64(m) * int64(bps) / 10000)
}
