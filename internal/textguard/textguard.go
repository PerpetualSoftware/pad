// Package textguard holds the ONE decoded-NUL predicate that every layer of
// Pad's NUL invariant shares.
//
// It exists because BUG-2803's whole history is layers disagreeing about one
// value. The HTTP gate refuses a request body whose JSON decodes to a NUL; the
// store's write guard refuses a bound parameter that does; Postgres refuses it
// natively; a SQLite trigger will refuse it in the database. Four answers to
// one question is only safe if they are one implementation, so this package is
// the implementation and each layer supplies only its own INPUT.
//
// WHAT IS SHARED AND WHAT IS NOT, because getting this line wrong is how the
// bug family breeds. Shared: whether a value, interpreted as text or as a JSON
// document, yields a NUL. NOT shared: how a layer decides that a given value
// IS a JSON document. The HTTP gate derives that from request-body KEY NAMES
// (fields, schema, settings - the keys whose string value something downstream
// re-parses); the store derives it from the COLUMN a parameter is bound to,
// via the 86-column classification. Those two derivations cannot be merged -
// one has keys, the other has columns - so they are made explicit instead:
// every entry point here takes the JSON-ness as an argument, and the four-way
// differential test pins that both derivations agree on one corpus.
package textguard

import (
	"encoding/json"
	"strings"
)

// escapePrefix is the only spelling of a NUL a JSON document can carry. Used
// as a cheap pre-filter: a value without it cannot decode to a NUL, so the
// expensive parse is skipped.
//
// It is a PRE-FILTER on the escape's presence, never a decision about its
// meaning. BUG-2803's recorded-wrong parity filter tried to decide from raw
// bytes whether an escape was live or literal and could not: a doubled
// backslash makes the same six characters literal text, and only a decode
// tells the two apart. Anything cheaper than a decode is layer-confused.
const escapePrefix = `\u00`

// ContainsNUL reports whether the string carries a real NUL byte.
//
// This is the universal half of the invariant and it needs no classing: no
// column of any type in any dialect wants one. Postgres refuses it outright
// (SQLSTATE 22021 / 22P05), SQLite stores it and then disagrees with itself
// about the value's length.
func ContainsNUL(s string) bool { return strings.ContainsRune(s, 0) }

// IsJSONDocument reports whether a string is a complete JSON object or array -
// the shape a downstream consumer, or Postgres's own jsonb parser, will
// re-parse.
func IsJSONDocument(s string) bool {
	t := strings.TrimSpace(s)
	if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
		return false
	}
	return json.Valid([]byte(t))
}

// DocumentDecodesNUL walks a JSON document carried as a string and reports
// whether anything in it decodes to a NUL - in a value or in a KEY.
//
// This is the layer Postgres itself parses, so an escape here is fatal where
// one a level deeper is not: a fields blob bound for jsonb is refused with
// 22P05 while the same escape inside a string the blob merely contains is
// stored intact.
//
// Returns false for a string that is not a JSON document at all, so a caller
// that is unsure may pass anything.
func DocumentDecodesNUL(s string) bool {
	if !strings.Contains(s, escapePrefix) || !IsJSONDocument(s) {
		return false
	}
	var inner any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &inner); err != nil {
		return false
	}
	return ValueDecodesNUL(inner)
}

// ValueDecodesNUL walks an ALREADY-DECODED JSON value for a NUL in any string
// or any object key.
//
// No key-classing here, deliberately: at this depth every string is caller
// data, and a caller that needs classing does it before calling - which is the
// package's whole contract. The HTTP gate's key-classed walk lives with the
// gate, calls this for the unclassed subtrees, and is the reason this function
// is exported rather than folded into DocumentDecodesNUL.
func ValueDecodesNUL(v any) bool {
	switch t := v.(type) {
	case string:
		return ContainsNUL(t)
	case map[string]any:
		for k, sub := range t {
			if ContainsNUL(k) {
				return true
			}
			if ValueDecodesNUL(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range t {
			if ValueDecodesNUL(sub) {
				return true
			}
		}
	}
	return false
}

// ParameterRefused reports whether a value bound as a SQL parameter violates
// the invariant, and is the store layer's single entry point.
//
// isJSON is the caller's classification, not a guess made here - see the
// package doc. When true the value is additionally walked as a JSON document,
// which is what catches the escape form that reaches Postgres's jsonb parser
// still spelled as an escape and is refused there with 22P05.
//
// The raw check runs REGARDLESS of isJSON: a JSON-classed parameter carrying a
// literal NUL byte is refused by the same rule as any other text, and skipping
// it for JSON-classed values is precisely the regression codex round 9 caught
// in the HTTP gate's own walk.
func ParameterRefused(value string, isJSON bool) bool {
	if ContainsNUL(value) {
		return true
	}
	return isJSON && DocumentDecodesNUL(value)
}
