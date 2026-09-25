package models

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
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
