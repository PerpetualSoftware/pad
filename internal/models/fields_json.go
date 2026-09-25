package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// DecodeFieldsJSON decodes an item's fields blob with every number kept as the
// literal it was written as (a json.Number), for code that decodes a blob in
// order to WRITE it back (BUG-3202).
//
// A plain json.Unmarshal into map[string]any turns each number into a float64,
// so the re-encode rounds an integer above 2^53 (9007199254740993 is stored
// back as 9007199254740992) and rewrites number formatting. Every field write
// door decoded the stored blob that way, so a write rounded numbers it never
// named: a fields_patch to `status` rewrote every large integer in the row.
// json.Number marshals back as its own literal, so a decode-then-encode leaves
// the numbers it did not change byte-identical.
//
// It accepts and refuses exactly what json.Unmarshal into map[string]any
// does. Trailing data after the value is refused, as Unmarshal refuses it,
// and so is a number that does not fit a float64 (1e400), which Unmarshal
// refuses with a range error. Only the REPRESENTATION of an accepted number
// changes, never the set of blobs a door accepts.
//
// A literal `null` decodes to a nil map with no error, as it does through
// Unmarshal; callers keep their existing handling of that case.
func DecodeFieldsJSON(raw []byte) (map[string]any, error) {
	var m map[string]any
	if err := DecodeJSONKeepingNumbers(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// DecodeJSONKeepingNumbers is DecodeFieldsJSON for any target: v must be a
// pointer, as for json.Unmarshal. The range check covers numbers held in
// interface{} slots (any, map[string]any, []any), which are the only slots
// UseNumber affects; a typed float64 or int destination is range-checked by
// the decoder itself, as it always was.
func DecodeJSONKeepingNumbers(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("invalid JSON: trailing data after the top-level value")
	}
	return checkNumbersFitFloat64(v)
}

// checkNumbersFitFloat64 refuses a json.Number that json.Unmarshal would have
// refused when decoding into an interface: one ParseFloat reports out of
// range.
func checkNumbersFitFloat64(v any) error {
	switch x := v.(type) {
	case *map[string]any:
		if x != nil {
			return checkNumbersFitFloat64(*x)
		}
	case *any:
		if x != nil {
			return checkNumbersFitFloat64(*x)
		}
	case *[]any:
		if x != nil {
			return checkNumbersFitFloat64(*x)
		}
	case map[string]any:
		for _, e := range x {
			if err := checkNumbersFitFloat64(e); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := checkNumbersFitFloat64(e); err != nil {
				return err
			}
		}
	case json.Number:
		if _, err := strconv.ParseFloat(string(x), 64); err != nil {
			return fmt.Errorf("json: cannot unmarshal number %s into Go value of type float64", string(x))
		}
	}
	return nil
}

// FieldValues is a request member carrying item field values, decoded keeping
// number literals (BUG-3202). The move and copy `field_overrides` are written
// into the destination item, so a plain map[string]any member rounded an
// integer above 2^53 the caller supplied. Absent and null decode to nil, as
// the plain map did.
type FieldValues map[string]any

func (f *FieldValues) UnmarshalJSON(data []byte) error {
	m, err := DecodeFieldsJSON(data)
	if err != nil {
		return err
	}
	*f = m
	return nil
}

// IsJSONNumberLiteral reports whether s is exactly one JSON number token, the
// form json.Number marshals verbatim. The field-value coercions (the server's
// items.CoerceFields and the CLI's --field typing) use it to keep a numeric
// string as its literal (BUG-3202). json.Valid alone would also accept an
// object or a quoted string, so the first byte is checked too.
func IsJSONNumberLiteral(s string) bool {
	if s == "" || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
		return false
	}
	return json.Valid([]byte(s))
}

// CanonicalJSONNumbers returns v with every number replaced by one canonical
// spelling of its exact value, for code that COMPARES decoded field values
// (BUG-3202). Since field writes keep a number's literal, one value can be
// stored as different text (1e3, 1000, 1000.0); comparing that text reports a
// change nobody made. Comparing float64s instead has the opposite hole: two
// different integers above 2^53 round to one float and compare equal. The
// canonical form is exact and equal for equal values, so both are closed.
//
// Numbers may arrive as json.Number (from DecodeJSONKeepingNumbers) or as
// float64 (from a plain decode); both are canonicalised, so a caller comparing
// a value from each gets the numeric answer. Maps and slices are copied, never
// mutated. The result is for comparison and marshals to valid JSON, but it is
// NOT a storage form: it rewrites the caller's spelling.
func CanonicalJSONNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = CanonicalJSONNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = CanonicalJSONNumbers(e)
		}
		return out
	case json.Number:
		return json.Number(canonicalNumberLiteral(string(x)))
	case float64:
		return json.Number(canonicalNumberLiteral(strconv.FormatFloat(x, 'g', -1, 64)))
	}
	return v
}

// canonicalNumberLiteral rewrites a JSON number literal as
// [-]<digits without leading or trailing zeros>e<exponent>, which is exact
// and the same for every spelling of one value: 1000, 1e3, 1.0e3 and 1000.0
// all become "1e3", and zero in any spelling becomes "0". A string that is not
// a number literal is returned unchanged.
func canonicalNumberLiteral(s string) string {
	if !IsJSONNumberLiteral(s) {
		return s
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	mant, expPart := s, ""
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, expPart = s[:i], s[i+1:]
	}
	exp := 0
	if expPart != "" {
		e, err := strconv.Atoi(expPart)
		if err != nil {
			return s // an exponent too large for int: leave it; no such value fits a float64 anyway
		}
		exp = e
	}
	intPart, frac := mant, ""
	if i := strings.IndexByte(mant, '.'); i >= 0 {
		intPart, frac = mant[:i], mant[i+1:]
	}
	digits := strings.TrimLeft(intPart+frac, "0")
	exp -= len(frac)
	if digits == "" {
		return "0"
	}
	trimmed := strings.TrimRight(digits, "0")
	exp += len(digits) - len(trimmed)
	out := trimmed + "e" + strconv.Itoa(exp)
	if neg {
		out = "-" + out
	}
	return out
}
