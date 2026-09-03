package items

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// CoerceFields converts STRING field values to the type the collection schema
// declares, so every write door stores the same native type for the same input
// (BUG-2850).
//
// The doors did not agree. The CLI has coerced by schema type since BUG-1125
// (cmd/pad/cmd_item.go::parseFieldFlag), and local stdio MCP inherits that by
// shelling out to the binary — but the remote /mcp transport builds its field
// map in ingestFieldKVP, which does `dst[key] = val` unconditionally, so every
// value arrives as a string. validateFieldType then correctly refuses a string
// for a declared number/json field, and the net effect was that an MCP agent on
// that transport could not write those fields AT ALL: every attempt a 400.
// Typing belongs to the server, keyed on the schema, so the doors cannot drift
// again.
//
// WHAT THIS DELIBERATELY DOES NOT DO:
//
//   - It does not report errors. A value that will not coerce is left as the
//     string and handed to the validator, which already produces the right
//     message ("field %q must be a number"). Coercion never invents an error
//     path, which is also why it cannot turn a currently-PASSING write into a
//     failure — the only inputs whose behaviour changes are ones that are
//     400ing today.
//   - It does not touch non-string values. A caller already sending 42 keeps
//     sending 42; this is not a re-typing pass over well-formed input.
//   - It does not touch keys the schema does not declare. Those are stored as
//     given, silently, which is the OTHER half of BUG-2850 — see the decision
//     point below.
//
// It returns a new map rather than mutating in place: two of the call sites
// re-marshal the map they pass, and a function that quietly rewrote their input
// would change what gets stored from behind a name that does not say so.
func CoerceFields(fields map[string]any, schema models.CollectionSchema) map[string]any {
	if len(fields) == 0 {
		return fields
	}
	byKey := make(map[string]models.FieldDef, len(schema.Fields))
	for i := range schema.Fields {
		byKey[schema.Fields[i].Key] = schema.Fields[i]
	}

	out := make(map[string]any, len(fields))
	for k, v := range fields {
		def, declared := byKey[k]
		if !declared {
			// DECISION POINT — BUG-2850 undeclared-key disposition (with Dave:
			// refuse / warn / keep). Today's behaviour is KEEP, which is what
			// silently stores materials_cost="42" on a collection that never
			// declared it. Refuse becomes a returned issue here; warn becomes a
			// collected key. Deliberately isolated to this branch so the choice
			// drops in without touching the coercion above it.
			out[k] = v
			continue
		}
		out[k] = coerceValue(def, v)
	}
	return out
}

// coerceValue converts one string value to its declared type, or returns it
// unchanged when it is not a string or will not parse.
func coerceValue(def models.FieldDef, v any) any {
	s, ok := v.(string)
	if !ok {
		return v
	}
	switch def.Type {
	case "json", "multi_select":
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
	case "number":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			// Reject NaN / ±Inf rather than storing them: encoding/json cannot
			// marshal either, and the downstream json.Marshal(fields) error is
			// ignored, so a non-finite float silently drops the ENTIRE fields
			// payload. Falling through leaves the string for the validator,
			// which says "must be a number" — the same reasoning as the CLI's
			// guard in parseFieldFlag (BUG-1125).
			if !math.IsNaN(f) && !math.IsInf(f, 0) {
				return f
			}
		}
	case "checkbox":
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	}
	// text, url, select, date, relation — a string is already the right type.
	// Anything that did not parse above also lands here, on purpose.
	return s
}

// UndeclaredFieldKeys returns the keys of fields the schema does not declare,
// sorted, excluding system-written metadata (BUG-2850).
//
// These keys are stored, not refused — see ItemWriteWarnings for why. The
// caller reports them so a typo leaves a trace: once written, a mistyped key
// and a deliberate extra field look identical, and the reporter's case was an
// agent that could not tell its own data had gone somewhere unintended.
//
// Reserved keys are excluded via models.IsReservedItemField rather than a
// second list here — that set exists precisely so callers ask instead of
// re-listing, and its own doc comment records what re-listing cost last time.
func UndeclaredFieldKeys(fields map[string]any, schema models.CollectionSchema) []string {
	if len(fields) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(schema.Fields))
	for i := range schema.Fields {
		declared[schema.Fields[i].Key] = struct{}{}
	}
	var out []string
	for k := range fields {
		if _, ok := declared[k]; ok {
			continue
		}
		if models.IsReservedItemField(k) {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
