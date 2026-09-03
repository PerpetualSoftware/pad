package mcp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// catalog_item_fields.go — the `fields` OBJECT alias on pad_item
// create/update (#1066, ToolSurfaceVersion 0.24).
//
// Why: since the BUG-991 normalization, reads return `fields` as a
// native object ({"status": "backlog", ...}), so writing back the
// structure you just read is the obvious call — and it used to be a
// silent no-op: `fields` was not a declared param, nothing set
// additionalProperties, so the object was accepted, never mapped by
// BuildCLIArgs, and dropped while the PATCH still ran (success
// response, bumped updated_at, unchanged value). This file makes the
// intuitive write shape correct instead of punishing it.
//
// Merge contract:
//
//   - Keys with a dedicated top-level param (status, priority,
//     category, parent, role, assign, tags) are promoted onto that
//     param — identical effect to passing the param directly.
//     ACCEPTED TRADE-OFF: a collection whose schema declares a custom
//     field literally named one of those seven keys gets redirected to
//     the reserved MCP param instead of the custom field. This layer
//     cannot see collection schemas (they live server-side, per
//     workspace), so it cannot disambiguate; the dedicated params
//     shadow such custom fields everywhere on this tool already, and
//     the `field: ["key=value"]` path has the same collision. Writing
//     a shadowed custom field from MCP requires renaming the field —
//     confirmed as accepted in the PR #1159 round-1 review exchange.
//   - Every other key merges into the `field` array path as a
//     "key=value" entry, exactly as if the caller had used
//     `field: ["key=value"]`. Server-side schema validation applies
//     unchanged, so an undeclared custom field still fails with
//     validation_failed rather than silently defaulting.
//   - The same key supplied in `fields` AND at the top level (or in
//     the `field` array) with a CONFLICTING value is refused with a
//     structured error — the surface's refuse-on-ambiguity
//     disposition; equal duplicates are unambiguous and collapse to
//     one write.
//   - Values must be scalars (string / number / bool). Nested
//     objects, arrays, and nulls have no defined write semantics here
//     and are refused with the offending key named. (`tags` is the
//     exception: promoted onto the dedicated param, which accepts an
//     array.)
//     KNOWN LIMITATION: this means the read shape is NOT fully
//     round-trippable for non-scalar field types (multi_select
//     arrays, json fields) — those refuse loudly rather than write.
//     Array/JSON value encoding onto the `field` path is a tracked
//     follow-up (PR #1159 round-1 review, scope note); until then the
//     refusal names the key so the caller knows which value to move
//     to a supported path.

// padItemPromotedFieldKeys maps a `fields` object key to the dedicated
// top-level param it promotes onto. Mirrors the "the dispatcher rolls
// those into the fields JSON automatically" list in the `field` param
// description — keep the two in sync.
var padItemPromotedFieldKeys = map[string]bool{
	"status":   true,
	"priority": true,
	"category": true,
	"parent":   true,
	"role":     true,
	"assign":   true,
	"tags":     true,
}

// actionItemCreate / actionItemUpdate replace the bare passThrough for
// the two writer actions: reshape the `fields` object (when present)
// into the dedicated-param + `field`-array paths, then dispatch as
// before. Calls without `fields` are byte-for-byte unchanged.
func actionItemCreate(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	out, errRes := reshapeItemFields("pad_item.create", input)
	if errRes != nil {
		return errRes, nil
	}
	return env.Dispatch(ctx, []string{"item", "create"}, out)
}

func actionItemUpdate(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	out, errRes := reshapeItemFields("pad_item.update", input)
	if errRes != nil {
		return errRes, nil
	}
	return env.Dispatch(ctx, []string{"item", "update"}, out)
}

// rejectFieldsParam wraps a non-writer pad_item action so a `fields`
// object reaching it fails loudly. Without this, the declared param
// would flow to dispatch and be dropped by BuildCLIArgs on actions
// that have no --field flag — recreating per-action the exact silent
// no-op this contract removes (an agent trying `fields` as a list
// filter would be the obvious casualty).
func rejectFieldsParam(prefix string, fn ActionFn) ActionFn {
	return func(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
		if _, present := input["fields"]; present {
			return errStructured(prefix, fmt.Errorf(
				"fields is only accepted for action=create and action=update; for list filtering use the dedicated params (status, priority, ...)")), nil
		}
		return fn(ctx, input, env)
	}
}

// reshapeItemFields folds input["fields"] into the dedicated-param and
// `field`-array paths. Returns the reshaped input, or a structured
// error result (second return) that the caller surfaces as-is. Input
// without a `fields` key is returned unchanged.
func reshapeItemFields(prefix string, input map[string]any) (map[string]any, *mcp.CallToolResult) {
	raw, present := input["fields"]
	if !present {
		return input, nil
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, errStructured(prefix, fmt.Errorf(
			"fields must be an object of field key → value (got %T); string entries go in field: [\"key=value\"]", raw))
	}

	out := make(map[string]any, len(input)+len(obj))
	for k, v := range input {
		if k == "fields" {
			continue
		}
		out[k] = v
	}

	// Existing `field` array entries, parsed key→value so a duplicate
	// key coming in via `fields` can be conflict-checked against them.
	fieldEntries, fieldByKey, errRes := parseFieldArray(prefix, out["field"])
	if errRes != nil {
		return nil, errRes
	}

	// The same values with their JSON types intact (BUG-2850).
	fieldsNative := map[string]any{}

	// Deterministic processing (and error ordering) across runs.
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		v := obj[k]
		if k == "" {
			// Without this, {"": "v"} would pass the '=' check below and
			// emit a malformed `field: ["=v"]` entry (PR #1159 round-1
			// review, bug 2).
			return nil, errStructured(prefix, fmt.Errorf("fields: keys cannot be empty"))
		}
		if strings.Contains(k, "=") {
			return nil, errStructured(prefix, fmt.Errorf("fields.%q: field keys cannot contain '='", k))
		}
		if padItemPromotedFieldKeys[k] {
			existing, has := out[k]
			if has {
				if !scalarEqual(existing, v) {
					return nil, errStructured(prefix, fmt.Errorf(
						"fields.%s conflicts with the top-level %s param (%v vs %v) — pass one of them, or the same value in both", k, k, v, existing))
				}
				continue // equal duplicate: unambiguous, already applied
			}
			// tags promotes the native array; everything else is scalar.
			if k != "tags" {
				if _, err := stringifyFieldValue(v); err != nil {
					return nil, errStructured(prefix, fmt.Errorf("fields.%s: %s", k, err))
				}
			}
			out[k] = v
			continue
		}
		// NULL stays refused (BUG-2850 lifts objects and arrays, not this).
		// A null in a fields map has no agreed meaning — "store JSON null" and
		// "clear this field" are both readable from it, and Pad already has an
		// explicit clear vocabulary (clear_parent, clear_assigned_user). Giving
		// null a silent meaning here would be inventing semantics inside a bug
		// fix; if a clear-by-null is ever wanted it should be ruled and named.
		if v == nil {
			return nil, errStructured(prefix, fmt.Errorf(
				"fields.%s: null has no defined write semantics — omit the key to leave it unchanged", k))
		}

		// NATIVE FORM, ALWAYS (BUG-2850). The value goes into fieldsNative
		// with its JSON type intact — a number stays a number, an object stays
		// an object. Whether a door can USE that depends on the door, which is
		// why the string form below is still emitted alongside it rather than
		// replaced.
		fieldsNative[k] = v

		sv, err := stringifyFieldValue(v)
		if err != nil {
			// A nested value (object/array/null). It has no `key=value`
			// encoding, so it cannot join fieldEntries — but it is no longer
			// refused here: the native map above carries it for doors that can
			// express it, and BuildCLIArgs refuses it for the stdio door that
			// cannot (BUG-2850, lifting PR #1159's blanket refusal now that
			// server-side coercion exists). A conflict with an existing
			// `field` entry is still a conflict: one key cannot be both a
			// string and a structure.
			if prev, has := fieldByKey[k]; has {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s conflicts with the field array entry %q — one key cannot be both a structured value and a string", k, k+"="+prev))
			}
			continue
		}
		if prev, has := fieldByKey[k]; has {
			if prev != sv {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s conflicts with the field array entry %q (%s vs %s) — pass one of them, or the same value in both", k, k+"="+prev, sv, prev))
			}
			continue
		}
		fieldEntries = append(fieldEntries, k+"="+sv)
		fieldByKey[k] = sv
	}

	if len(fieldEntries) > 0 {
		out["field"] = fieldEntries
	}
	// Emitted under a distinct key so each transport takes what it can use:
	// the HTTP mapper prefers these (types intact), BuildCLIArgs consults them
	// only to refuse the nested values the CLI cannot express. Both forms
	// describe the same input, so a door reading either is correct — they
	// differ only in fidelity.
	if len(fieldsNative) > 0 {
		out[fieldsNativeKey] = fieldsNative
	}
	return out, nil
}

// fieldsNativeKey is the dispatch-input key carrying the `fields` object with
// JSON types intact (BUG-2850). Deliberately not a name a caller could send:
// it is produced by this merge, never accepted from the wire, and strict input
// validation would reject it as an undeclared param if it were.
const fieldsNativeKey = "__fields_native"

// parseFieldArray normalizes an existing `field` param into a []string
// plus a key→value index. Entries the CLI would reject anyway (no '=')
// are passed through unindexed rather than pre-empting the CLI's own
// error surface.
func parseFieldArray(prefix string, raw any) ([]string, map[string]string, *mcp.CallToolResult) {
	byKey := map[string]string{}
	if raw == nil {
		return nil, byKey, nil
	}
	var entries []string
	switch v := raw.(type) {
	case []string:
		entries = append(entries, v...)
	case []any:
		for i, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, nil, errStructured(prefix, fmt.Errorf("field[%d] is %T, want \"key=value\" string", i, e))
			}
			entries = append(entries, s)
		}
	case string:
		// Lenient fallback: a single entry passed unwrapped.
		entries = append(entries, v)
	default:
		return nil, nil, errStructured(prefix, fmt.Errorf("field is %T, want array of \"key=value\" strings", raw))
	}
	for _, e := range entries {
		if k, val, ok := strings.Cut(e, "="); ok {
			byKey[k] = val
		}
	}
	return entries, byKey, nil
}

// stringifyFieldValue renders one `fields` value into the string form a
// `field: ["key=value"]` entry carries. Scalars only — nested
// structures and nulls have no defined "key=value" encoding.
func stringifyFieldValue(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case bool:
		return strconv.FormatBool(t), nil
	case float64: // JSON numbers decode to float64
		return strconv.FormatFloat(t, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(t), nil
	case nil:
		return "", fmt.Errorf("null has no write semantics here — to clear a value use the field's documented clear form")
	default:
		return "", fmt.Errorf("value is %T, want a scalar (string, number, or bool)", v)
	}
}

// scalarEqual compares a top-level param value with a `fields` value
// for the equal-duplicate check. Scalars compare by their stringified
// form (so 3 == 3.0 and "done" == "done"); anything non-scalar (e.g.
// two tags arrays) compares by fmt.Sprint — good enough to distinguish
// "same call twice" from a genuine conflict.
func scalarEqual(a, b any) bool {
	as, aerr := stringifyFieldValue(a)
	bs, berr := stringifyFieldValue(b)
	if aerr == nil && berr == nil {
		return as == bs
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}
