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
// hierarchyPseudoFieldKeys are `fields` keys the SERVER reads as parent-link
// directives rather than as ordinary field values (extractParentLink, in
// internal/server/handlers_items.go). They take a string ref and nothing else.
//
// `parent` is also in padItemPromotedFieldKeys below and gets a scalar check
// there; `plan` is not, which is how a structured value reached the server
// once BUG-2850 stopped refusing structures. Both are listed here so the
// guard does not depend on which other set a key happens to belong to.
var hierarchyPseudoFieldKeys = map[string]bool{
	"parent": true,
	"plan":   true,
}

// hierarchyAliasKeys is the same set in the order extractParentLink reads it
// (handlers_items.go: `for _, key := range []string{"parent", "plan"}`, no
// early exit, so the LATER key wins). Kept as a slice for deterministic error
// text when one alias conflicts with the other.
var hierarchyAliasKeys = []string{"parent", "plan"}

// identityRefFieldKeys are promoted keys whose value NAMES something (a user,
// a role) rather than being a value in its own right. They take a string and
// nothing else — see the refusal in reshapeItemFields for why a number is
// worse than useless here.
var identityRefFieldKeys = map[string]bool{
	"assign": true,
	"role":   true,
}

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
	// Runs regardless of whether `fields` is present — reshapeItemFields
	// returns early without it, and the ambiguity is reachable through the
	// `field` array alone (codex round 7).
	if errRes := checkHierarchyAliasAmbiguity("pad_item.create", input); errRes != nil {
		return errRes, nil
	}
	out, errRes := reshapeItemFields("pad_item.create", input)
	if errRes != nil {
		return errRes, nil
	}
	return env.Dispatch(ctx, []string{"item", "create"}, out)
}

func actionItemUpdate(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	if errRes := checkHierarchyAliasAmbiguity("pad_item.update", input); errRes != nil {
		return errRes, nil
	}
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

	// The ORIGINAL top-level hierarchy params, snapshotted before the loop
	// starts writing promoted keys into `out` (codex round 9).
	//
	// The alias check below reads this rather than `out`. Reading `out` made
	// the guard work by ACCIDENT for a pair arriving wholly inside `fields`:
	// keys are processed in sorted order, so `parent` was promoted into
	// out["parent"] and `plan` then collided with it. Right answer, wrong
	// mechanism — it says "conflicts with the top-level parent param" to a
	// caller who passed no such param, and it would evaporate the day
	// `parent` left padItemPromotedFieldKeys. The fields-vs-fields case is
	// now checked directly against `obj`, and this snapshot keeps the
	// top-level message honest.
	origHierarchyParams := map[string]any{}
	for _, alias := range hierarchyAliasKeys {
		if v, ok := input[alias]; ok {
			origHierarchyParams[alias] = v
		}
	}

	// The same values with their JSON types intact (BUG-2850).
	fieldsNative := map[string]any{}

	// Promoted keys whose array entry is an equal duplicate and is therefore
	// removed from `field` before dispatch, so the value is written once
	// through its dedicated param (codex round 6).
	dropFieldKeys := map[string]bool{}

	// Generic keys whose array entry was a PADDED equal duplicate: the raw
	// entry is dropped and re-emitted in canonical `key=value` form, so the
	// doors that do not trim write the key the caller meant (codex round 7).
	reEmitFields := map[string]string{}

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
		// THESE TWO GUARDS RUN BEFORE THE PROMOTED BRANCH, and that ordering
		// is the fix, not a detail (codex round 4). They were below it, so
		// `tags: null` and `parent: 42` reached the promoted path and skipped
		// both: the null was converted into a silent no-op, and a numeric
		// parent was dropped later by the handler. A guard that a whole class
		// of keys walks around is not a guard.
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

		// HIERARCHY PSEUDO-KEYS TAKE A STRING REF, ALWAYS (BUG-2850, codex
		// round 3). `plan` is not in padItemPromotedFieldKeys, so before this
		// it fell to the generic path and — once structures stopped being
		// refused — a `fields:{"plan":{…}}` reached the server natively. There
		// `extractParentLink` reads any PRESENT non-string plan/parent as a
		// hierarchy directive, drops the key, and on update CLEARS the item's
		// existing parent link. So lifting the nested refusal quietly opened a
		// path where a malformed value silently detaches an item from its
		// parent. A structure has no meaning here at all: the only value these
		// keys take is a ref.
		if hierarchyPseudoFieldKeys[k] {
			if _, isString := v.(string); !isString {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s must be a string ref (e.g. %q) — a %T here would be read as a hierarchy directive and could detach the item", k, "PLAN-12", v))
			}
			// ...AND `parent` AND `plan` ARE THE SAME DIRECTIVE, so the
			// same-name conflict guards below do not cover them (codex round
			// 6). `extractParentLink` (handlers_items.go) resolves the link
			// with `for _, key := range []string{"parent", "plan"}` and NO
			// early exit, so when both arrive the LATER key wins — the exact
			// alias bypass BUG-2078's round-1 review found for clear_parent,
			// arriving here through a different door. `fields:{"parent":"A"}`
			// with `field:["plan=B"]` therefore relinks the item to B while
			// reporting success for A.
			//
			// Refused rather than resolved, even when the two values are
			// EQUAL. Two hierarchy directives in one call are ambiguous by
			// construction, and this catalog already refuses the same shape
			// for parent + clear_parent "including via the plan alias"
			// (v0.19). Consistency with that ruling matters more than
			// accepting a pair that happens to agree today.
			for _, alias := range hierarchyAliasKeys {
				if alias == k {
					continue // same-name conflicts fall to the guards below
				}
				// Both aliases inside ONE `fields` object. Checked against
				// `obj` directly so the refusal does not depend on which key
				// sorts first or on `parent` happening to be a promoted key.
				if other, has := obj[alias]; has {
					return nil, errStructured(prefix, fmt.Errorf(
						"fields.%s and fields.%s are the same hierarchy directive (%v vs %v) and the server applies whichever it reads last; pass one of them", k, alias, v, other))
				}
				if prev, has := fieldByKey[alias]; has {
					return nil, errStructured(prefix, fmt.Errorf(
						"fields.%s conflicts with the field array entry %q — %q and %q are the same hierarchy directive and the server applies whichever it reads last; pass one of them", k, alias+"="+prev, "parent", "plan"))
				}
				if existing, has := origHierarchyParams[alias]; has {
					return nil, errStructured(prefix, fmt.Errorf(
						"fields.%s conflicts with the top-level %s param (%v vs %v) — %q and %q are the same hierarchy directive; pass one of them", k, alias, v, existing, "parent", "plan"))
				}
			}
		}

		// IDENTITY-REFERENCE KEYS TAKE A STRING, ALWAYS (codex round 9).
		// `assign` and `role` name a person or a role — a slug, an email, a
		// UUID. A number has no meaning for either, and the two doors
		// disagreed about what to do with one: the HTTP dispatcher's
		// `rawAssign.(string)` turns it into "" and treats it as NOT
		// PROVIDED, silently dropping the write, while stdio emits
		// `--assign 123` and the CLI fails loudly on the lookup. Same call,
		// one door silent and one door red.
		//
		// Refused here, at the door-independent layer, rather than taught to
		// each dispatcher — that is what stops them drifting again. Note this
		// does NOT walk back round 6's decision to accept non-string promoted
		// values generally: `priority` may legitimately be a number in a
		// custom schema, and create has always passed such values through.
		// These two keys are references, not values.
		if identityRefFieldKeys[k] {
			if _, isString := v.(string); !isString {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s must be a string (a slug, email, or id) — a %T is silently dropped by one transport and rejected by the other", k, v))
			}
		}

		if padItemPromotedFieldKeys[k] {
			// THE `field: ["k=v"]` CONFLICT GUARD RUNS HERE TOO (codex round
			// 5). It lives on the generic path below, which a promoted key
			// never reaches — so `fields.status` vs `field:["status=…"]` was
			// the one ambiguity in this function that did NOT refuse. And it
			// did not fail closed either: the promoted branch writes the
			// top-level param while the array stays in out["field"], and both
			// the HTTP mappers and the CLI overlay --field entries AFTER the
			// named flags, so the array silently won. On `status` that
			// cancels an item you asked to mark done; on `parent` it relinks
			// or detaches it.
			//
			// Fourth round running that a guard was written on the generic
			// path only. The top-level check below is not a substitute:
			// `field` and the dedicated param are different doors, and
			// covering one says nothing about the other.
			if prev, inArray := fieldByKey[k]; inArray {
				sv, err := stringifyFieldValue(v)
				if err != nil {
					// tags is the promoted key that takes a structure; the
					// generic path refuses this same shape for the same
					// reason, one key cannot be both.
					return nil, errStructured(prefix, fmt.Errorf(
						"fields.%s conflicts with the field array entry %q — one key cannot be both a structured value and a string", k, k+"="+prev))
				}
				if sv != prev {
					return nil, errStructured(prefix, fmt.Errorf(
						"fields.%s conflicts with the field array entry %q (%s vs %s) — pass one of them, or the same value in both", k, k+"="+prev, sv, prev))
				}
				// Equal duplicate: unambiguous, so it applies — ONCE, through
				// the dedicated param, which means the array entry has to GO
				// (codex round 6). Leaving it produced two writes of one
				// value through two different mechanisms: `role` was resolved
				// to agent_role_id AND written as a literal `role` key in the
				// fields blob, where no schema declares it — so an
				// equal-duplicate call silently created an undeclared field
				// (and, since dc3fc2d5, a warning naming it). Same for
				// `assign` and `tags`.
				//
				// Deleting from fieldByKey too, so a later key in this loop
				// sees the entry as gone rather than conflicting with
				// something no longer being sent.
				dropFieldKeys[k] = true
				delete(fieldByKey, k)
			}
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
			// EQUAL DUPLICATE — but the RETAINED entry may be the padded one
			// (codex round 7). The index is normalized so ` effort=l` matches
			// `fields:{"effort":"l"}`, yet the raw entry stayed in `field`
			// and the CLI door does not trim: stdio would store an undeclared
			// `" effort"` key and leave `effort` untouched — a silent
			// mis-write created by the very normalization that let the
			// duplicate be recognized. Re-emit the entry in canonical form so
			// every door writes the key the caller meant.
			//
			// Only when the raw form actually differs, so a well-formed call
			// keeps its array untouched and in order.
			if hasNonCanonicalFieldEntry(fieldEntries, k, sv) {
				dropFieldKeys[k] = true
				reEmitFields[k] = sv
			}
			continue
		}
		fieldEntries = append(fieldEntries, k+"="+sv)
		fieldByKey[k] = sv
	}

	// Remove the equal-duplicate entries the promoted branch claimed. Matched
	// on the NORMALIZED key, the same form parseFieldArray indexed on, so
	// `field:[" role=implementer"]` is dropped by the same rule that let it
	// be recognized as a duplicate in the first place.
	if len(dropFieldKeys) > 0 {
		kept := fieldEntries[:0]
		for _, e := range fieldEntries {
			if key, _, ok := strings.Cut(e, "="); ok && dropFieldKeys[strings.TrimSpace(key)] {
				continue
			}
			kept = append(kept, e)
		}
		fieldEntries = kept
	}
	// Canonical re-emissions go on AFTER the filter, or the filter would
	// remove them again — they carry the same key it just matched on.
	if len(reEmitFields) > 0 {
		reKeys := make([]string, 0, len(reEmitFields))
		for k := range reEmitFields {
			reKeys = append(reKeys, k)
		}
		sort.Strings(reKeys) // deterministic arg order across runs
		for _, k := range reKeys {
			fieldEntries = append(fieldEntries, k+"="+reEmitFields[k])
		}
	}
	if len(fieldEntries) > 0 {
		out["field"] = fieldEntries
	} else {
		// An entry set emptied by the drop above must not leave the caller's
		// original `field` array in place — that is the value we just decided
		// not to send twice.
		delete(out, "field")
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

// hasNonCanonicalFieldEntry reports whether ANY entry for this key is written
// in something other than the canonical `key=value` form.
//
// "Any", not "no canonical entry exists" (codex round 8). The first version
// asked whether a canonical entry was PRESENT and left the array alone if one
// was — so `field:["effort=l", " effort=l"]` kept the padded twin, and the
// doors then disagreed about it: HTTP trims and writes `effort`, the CLI does
// not and writes an undeclared `" effort"`. One canonical entry does not make
// its padded sibling harmless; every entry for the key has to be canonical,
// or the key gets re-emitted once and cleanly.
//
// Collapsing duplicates in that re-emission is correct rather than lossy:
// parseFieldArray already indexes them to a single value, so two entries for
// one key were never two writes.
func hasNonCanonicalFieldEntry(entries []string, key, value string) bool {
	want := key + "=" + value
	for _, e := range entries {
		k, _, ok := strings.Cut(e, "=")
		if !ok || strings.TrimSpace(k) != key {
			continue
		}
		if e != want {
			return true
		}
	}
	return false
}

// checkHierarchyAliasAmbiguity refuses a call that names BOTH hierarchy
// aliases, whichever doors they arrive through (codex round 7).
//
// The alias guard added in round 6 lives inside reshapeItemFields' per-key
// loop, so it only fires when the `fields` OBJECT carries a hierarchy key —
// and reshapeItemFields returns early when there is no `fields` at all. That
// left the ambiguity fully reachable through the array alone:
// `field:["parent=A","plan=B"]` was accepted, and extractParentLink's
// no-early-exit loop applied `plan` while the caller had every reason to
// believe `parent` was what they set. A guard a caller can step around by
// moving the same two values into a different param is not a guard — the
// lesson this unit has now produced at four consecutive rounds.
//
// SCOPE, stated plainly because it widens what this tool refuses: the
// pure-`field` form was accepted before BUG-2850 too, so this is not a
// regression being closed but a pre-existing silent mis-write. It is fixed
// here rather than filed because shipping the round-6 guard without it would
// advertise a refusal that any caller can bypass in one edit. Refusing is
// consistent with v0.19, which already refuses parent + clear_parent
// "including via the plan alias".
//
// Deliberately NOT extended to same-name duplicates (a top-level `parent`
// param plus `field:["parent=B"]`). Those resolve last-write-wins
// identically on both doors — documented behaviour, not an ambiguity — and
// widening the refusal to cover them is a policy change, not a defect fix.
func checkHierarchyAliasAmbiguity(prefix string, input map[string]any) *mcp.CallToolResult {
	_, byKey, errRes := parseFieldArray(prefix, input["field"])
	if errRes != nil {
		// Shape errors belong to the caller that parses for real; staying
		// silent here keeps a single error surface for them.
		return nil
	}
	planVal, hasPlan := byKey["plan"]
	if !hasPlan {
		return nil
	}
	if parentVal, hasParent := byKey["parent"]; hasParent {
		return errStructured(prefix, fmt.Errorf(
			"field entries %q and %q are the same hierarchy directive and the server applies whichever it reads last; pass one of them",
			"parent="+parentVal, "plan="+planVal))
	}
	if v, ok := input["parent"].(string); ok && v != "" {
		return errStructured(prefix, fmt.Errorf(
			"the parent param (%s) conflicts with the field array entry %q — %q and %q are the same hierarchy directive; pass one of them",
			v, "plan="+planVal, "parent", "plan"))
	}
	return nil
}

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
			// NORMALIZED THE WAY THE DOOR WILL NORMALIZE (codex round 6).
			// ingestFieldKVP (dispatch_http.go) TrimSpaces both halves before
			// storing them, so an un-trimmed index here does not describe what
			// the remote door is about to write: `field:[" status=cancelled"]`
			// indexed under " status" missed every conflict check against
			// `fields:{"status":…}` and then silently overrode it. Trimming
			// the value closes the mirror-image false refusal, where
			// `field:["status= done"]` looked different from "done" and was
			// refused as a conflict with a call that agrees.
			//
			// Only the INDEX is normalized. `entries` stays verbatim so every
			// door still parses exactly what the caller sent — the CLI door
			// does not trim (cmd/pad/cmd_item.go), and this must not quietly
			// change what it receives. The effect is a conflict check that is
			// conservative on both doors, which is the correct direction for
			// a guard whose disposition is refuse-on-ambiguity.
			byKey[strings.TrimSpace(k)] = strings.TrimSpace(val)
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
