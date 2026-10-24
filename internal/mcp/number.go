package mcp

import (
	"encoding/json"
	"math"
	"strconv"
)

// formatNumber formats native numbers without exponents and preserves
// json.Number literals. NaN and infinities keep strconv's spellings so callers
// can apply their own validation, as they did with fmt.Sprint.
func formatNumber(v any) (string, bool) {
	var x float64
	switch n := v.(type) {
	case float64:
		x = n
	case float32:
		// UNREACHABLE today (BUG-4061 note): the JSON transports deliver only
		// float64, and nothing outside tests passes a float32.
		// Widening prints float32(0.1) as 0.10000000149011612 below; a float32
		// producer would want strconv.FormatFloat(float64(n), 'f', -1, 32).
		x = float64(n)
	case int:
		return strconv.Itoa(n), true
	case int32:
		return strconv.FormatInt(int64(n), 10), true
	case int64:
		return strconv.FormatInt(n, 10), true
	case uint:
		return strconv.FormatUint(uint64(n), 10), true
	case uint32:
		return strconv.FormatUint(uint64(n), 10), true
	case uint64:
		return strconv.FormatUint(n, 10), true
	case json.Number:
		return n.String(), true
	default:
		return "", false
	}
	// MaxInt64 rounds up to 2^63 as a float64, so the upper bound is
	// exclusive. Converting only within range also normalizes -0 to 0.
	if math.Trunc(x) == x && x >= math.MinInt64 && x < 0x1p63 {
		return strconv.FormatInt(int64(x), 10), true
	}
	return strconv.FormatFloat(x, 'f', -1, 64), true
}
