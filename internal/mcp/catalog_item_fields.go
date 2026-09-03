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

// ─── ONE CANONICAL VIEW, ONE CONFLICT CHECK (BUG-2850, lead ruling after
// codex round 13) ───────────────────────────────────────────────────────────
//
// Rounds 5, 7, 11, 12 and 13 each found a defect in the PREVIOUS round's fix,
// and every one was the same shape: conflict handling had accreted a separate
// guard at each site that noticed a problem — the generic path, the promoted
// block, the alias check, the compat-ID block, the canonicalization predicate
// — and each guard only covered the sources its author happened to think
// about. Round 13's finding is the proof: `assign` and `assigned_user_id` are
// two names for one target, exactly like `parent`/`plan`, and NO guard
// compared them, because the alias guard knew only about hierarchy and the
// compat guard knew only about same-name collisions.
//
// So the guards are replaced by one pass that resolves EVERY source to a
// canonical key and refuses on the resulting map. A new alias pair is now one
// line in fieldAliasGroups rather than a sixth guard with its own edges.

// fieldAliasGroups maps a key to the canonical name of the thing it WRITES.
// Members of a group are different names for one target, so two of them in
// one call is ambiguous by construction — their value spaces are not even
// comparable (`assign` takes a slug or email, `assigned_user_id` a UUID),
// which is why co-occurrence refuses rather than trying to decide equality.
var fieldAliasGroups = map[string]string{
	"parent":           "parent",
	"plan":             "parent",
	"assign":           "assign",
	"assigned_user_id": "assign",
	"role":             "role",
	"agent_role_id":    "role",
}

func canonicalFieldKey(k string) string {
	if g, ok := fieldAliasGroups[k]; ok {
		return g
	}
	return k
}

// fieldContribution is one source offering one value for one canonical key.
type fieldContribution struct {
	key    string // as the caller wrote it
	source string // human-readable, for the refusal
	value  string // normalized for comparison; "" when unstringifiable
	nested bool   // a structure, with no key=value encoding
	raw    any    // the value as supplied, for comparing two structures

	// nonCanonical marks a `field` array entry written in something other
	// than its canonical `key=value` form (padding around either half). It
	// matters because the two doors then receive DIFFERENT writes.
	nonCanonical bool
}

// topLevelConflictKeys are the top-level params that can collide with a
// `fields` entry. Built from the three sets so adding a key to any of them
// cannot leave this pass behind.
func topLevelConflictKeys() []string {
	seen := map[string]bool{}
	var out []string
	for _, set := range []map[string]bool{padItemPromotedFieldKeys, compatIDFieldKeys} {
		for k := range set {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	for k := range hierarchyPseudoFieldKeys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// detectFieldConflicts builds the canonical map and refuses on it. It is the
// ONLY place a conflict is decided; the emission code below runs knowing the
// input is unambiguous.
//
// SCOPE: this runs only when a `fields` object is present, because
// reshapeItemFields does. A top-level param colliding with a `field:[]` entry
// and no `fields` object is OUTSIDE it and keeps its documented
// last-write-wins resolution — deliberately, per the round-7 boundary the
// lead confirmed, and pinned on both doors by the SameNameDuplicate tests.
func detectFieldConflicts(prefix string, input map[string]any) *mcp.CallToolResult {
	// Parsed here rather than passed in, so this pass sees the same inputs
	// whether or not a `fields` object exists (codex round 14). It used to
	// run only from inside reshapeItemFields, which returns early without
	// `fields` — so an ALIAS pair arriving through the top level and the
	// `field` array alone slipped past it, and the doors diverged:
	// `assigned_user_id:"B"` with `field:["assign=dave"]` applies the compat
	// ID over HTTP while stdio drops it and sends only the generic field.
	//
	// Round 7 had already built exactly this always-run guard, for the
	// hierarchy pair only (checkHierarchyAliasAmbiguity, now deleted). So the
	// restructure that was supposed to end guard accretion had itself left
	// TWO alias mechanisms with different reach. One is what the ruling
	// asked for; this is it.
	obj, _ := input["fields"].(map[string]any) // nil when absent; shape errors belong to reshapeItemFields
	// NOTE: there is deliberately no `fieldsPresent` here any more. Both
	// gates below ask the per-KEY question — is THIS key carried by the
	// object — and rounds 17 and 19 were each a per-request predicate
	// standing in for it. The variable's absence is the fix's shape: if a
	// future gate wants "does the request have a fields object", that is
	// almost certainly the same mistake a third time.
	// The INDEX is deliberately discarded here — this pass walks the raw
	// entries so two entries naming one key stay two contributions (round 18).
	fieldEntries, _, errRes := parseFieldArray(prefix, input["field"])
	if errRes != nil {
		return nil // the caller parses for real and owns this error surface
	}

	groups := map[string][]fieldContribution{}
	add := func(canonical string, c fieldContribution) {
		groups[canonical] = append(groups[canonical], c)
	}

	objKeys := make([]string, 0, len(obj))
	for k := range obj {
		objKeys = append(objKeys, k)
	}
	sort.Strings(objKeys) // deterministic refusal text across runs
	for _, k := range objKeys {
		sv, err := stringifyFieldValue(obj[k])
		// COMPARED TRIMMED, EMITTED RAW (codex round 19). Entry values are
		// trimmed for comparison because ingestFieldKVP trims them, so
		// comparing a trimmed entry against an untrimmed `fields` value was
		// apples to oranges: `fields:{"note":" x "}` with `field:["note= x "]`
		// read as " x " vs "x" and refused, though both doors write " x ".
		// Only `value` (the comparison key) is trimmed; `raw` keeps the
		// original, and the canonical re-emission still carries the untrimmed
		// value to the wire.
		add(canonicalFieldKey(k), fieldContribution{
			key: k, source: "fields." + k, value: strings.TrimSpace(sv), nested: err != nil, raw: obj[k],
		})
	}

	// ONE CONTRIBUTION PER ENTRY, not per indexed key (codex round 18).
	//
	// parseFieldArray indexes by NORMALIZED key, so two entries naming one
	// key collapse to a single index slot — and iterating that index made
	// this pass's own input lossy. `field:["effort=l", " effort=l"]` arrived
	// as ONE contribution, fell under the len < 2 early exit, and passed
	// unchecked: HTTP trims both to `effort` while stdio writes `effort` AND
	// a junk `" effort"`. The pass claims to adjudicate one canonical key
	// offered by multiple sources; two array entries ARE multiple sources,
	// and it could not see them.
	//
	// Walking the raw entries keeps the multiplicity, so the existing rules
	// apply unchanged: equal canonical duplicates collapse, and a padded twin
	// is refused by the non-canonical check like any other.
	for _, entry := range fieldEntries {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue // no '=' — the CLI owns that error surface, not this pass
		}
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		add(canonicalFieldKey(key), fieldContribution{
			key: key, source: "the field array entry " + strconv.Quote(entry), value: val, raw: val,
			// Whether the entry as WRITTEN matches its canonical form. The
			// index is normalized so a padded entry can be recognized at all;
			// this remembers that the doors will not receive it identically.
			nonCanonical: entry != key+"="+val,
		})
	}

	for _, k := range topLevelConflictKeys() {
		v, present := input[k]
		if !present || !topLevelValueProvided(k, v) {
			continue
		}
		sv, err := stringifyFieldValue(v)
		add(canonicalFieldKey(k), fieldContribution{
			key: k, source: "the top-level " + k + " param", value: sv, nested: err != nil, raw: v,
		})
	}

	canonicals := make([]string, 0, len(groups))
	for k := range groups {
		canonicals = append(canonicals, k)
	}
	sort.Strings(canonicals)

	for _, canonical := range canonicals {
		contribs := groups[canonical]
		if len(contribs) < 2 {
			continue
		}
		// ALIAS COLLISION: two different NAMES for one target. Refused even
		// when the values look equal — the names address the same thing
		// through different vocabularies, so "equal" is not a question this
		// layer can answer.
		for i := range contribs {
			if contribs[i].key == contribs[0].key {
				continue
			}
			a, b := contribs[0], contribs[i]
			return errStructured(prefix, fmt.Errorf(
				"%s conflicts with %s — %q and %q are two names for the same thing and the doors resolve them differently; pass one of them",
				a.source, b.source, a.key, b.key))
		}
		// SAME NAME, different sources: equal collapses, differing refuses.
		//
		// Gated on `fields` being present, because that is this merge's
		// scope. A top-level param colliding with a `field:[]` entry and no
		// `fields` object keeps its documented last-write-wins resolution —
		// the round-7 boundary the lead confirmed, pinned on both doors by
		// the SameNameDuplicate tests. Alias collisions above are NOT gated:
		// last-write-wins is only defensible when both sources name the same
		// key, and two names for one target have incomparable value spaces.
		// EQUALITY ONLY LICENSES A COLLAPSE WHEN BOTH DOORS RECEIVE THE SAME
		// WRITE — and this runs BEFORE the exemption below, deliberately
		// (codex round 16).
		//
		// The conflict index is normalized, so `field:["k = A"]` compares
		// EQUAL to a top-level `k:"A"` and the pair was accepted while the
		// entry stayed padded on the wire. HTTP trims it and writes `k`; the
		// CLI does not, and writes a junk `"k "` key instead. The
		// normalization that lets the collision be SEEN is exactly what made
		// accepting it wrong.
		//
		// It sits above the exemption because padding breaks the exemption's
		// own premise — that both doors resolve the duplicate identically —
		// for EVERY key class, not just the compat IDs. Round 15 was this
		// same mistake (a premise verified for declared params, generalized
		// to keys it did not hold for); putting this check below the
		// exemption would have repeated it one round later, and my first
		// draft did exactly that.
		//
		// Only when nothing will canonicalize the entry, i.e. no `fields`
		// object — with one present, reshapeItemFields re-emits it
		// canonically and equality is safe again.
		//
		// Deliberately NOT extended to a padded entry standing ALONE with no
		// colliding param: that is BUG-2870, ruled out of this PR's scope,
		// and it changes what every CLI caller receives. Here the caller has
		// supplied one key twice and one of the forms is malformed, which is
		// a narrower and locally-answerable question.
		// WHETHER THIS KEY GETS CANONICALIZED IS A PER-KEY QUESTION, not a
		// per-request one (codex round 17).
		//
		// Round 16 gated this on `!fieldsPresent`, reasoning that with a
		// `fields` object present reshapeItemFields re-emits the entry
		// canonically. True — for keys that are IN that object. With
		// `fields:{}`, or a `fields` carrying some OTHER key, nothing
		// canonicalizes `field:["status = done"]` and it reaches the doors
		// padded exactly as it does with no `fields` at all.
		//
		// Third round running that I generalized a property verified on one
		// subset to the whole: round 15 (a premise true of declared params,
		// applied to the compat IDs), round 16's first draft (a check placed
		// below the exemption so it covered one key class), and now a
		// per-key property read as per-request. The predicate is now the
		// actual question — will anything canonicalize THIS key.
		canonicalized := false
		for _, c := range contribs {
			if _, inObj := obj[c.key]; inObj {
				canonicalized = true
				break
			}
		}
		if !canonicalized {
			for i := 1; i < len(contribs); i++ {
				a, b := contribs[0], contribs[i]
				if a.nonCanonical || b.nonCanonical {
					return errStructured(prefix, fmt.Errorf(
						"%s conflicts with %s — the field entry is not in canonical key=value form, so the transports would write different keys; remove the padding or pass only one of them",
						a.source, b.source))
				}
			}
		}

		// ...and the exemption holds only where the doors PROVABLY agree,
		// which is not everywhere (codex round 15).
		//
		// For a schema-declared param the CLI has a real flag, so stdio
		// receives BOTH forms (`--status open --field status=done`) and its
		// overlay order resolves them exactly as the HTTP mapper does — the
		// premise the round-7 boundary rests on, pinned per door.
		//
		// The v0.16 compat IDs are the exception, and being undeclared is
		// precisely why: BuildCLIArgs emits the CLI's real flags, and there
		// is no flag behind `assigned_user_id`, so the top-level value is
		// DROPPED and stdio sees only the field entry — while HTTP reads the
		// top-level param. Same call, two different people assigned, with no
		// `fields` object anywhere. Last-write-wins cannot be the answer when
		// the two doors do not receive the same writes.
		//
		// I generalised the round-7 premise from the params I had verified to
		// the two whose whole nature is being unverifiable that way. This is
		// the narrowing.
		// PER-KEY HERE TOO (codex round 19). Round 17 made the padded gate
		// per-key and left this one per-request — the same mistake in the
		// sibling gate, one round later. With `fields:{"other":"x"}` and
		// `field:["effort=l","effort=s"]`, `effort` is not in the object,
		// nothing arbitrates it but the doors themselves, and both keep the
		// last entry — so refusing it was a false refusal on a call that
		// resolves deterministically.
		//
		// `canonicalized` is exactly the right question and is already
		// computed above: is THIS key carried by the `fields` object.
		if !canonicalized && !compatIDFieldKeys[canonical] && !compatIDFieldKeys[contribs[0].key] {
			continue
		}
		for i := 1; i < len(contribs); i++ {
			a, b := contribs[0], contribs[i]
			if a.nested && b.nested {
				// TWO STRUCTURES: equal ones collapse like any other equal
				// duplicate (codex round 14). Refusing them unconditionally
				// was a regression the restructure introduced — `tags:["a"]`
				// plus `fields:{"tags":["a"]}` is one unambiguous value, and
				// scalarEqual had always collapsed it before.
				if !scalarEqual(a.raw, b.raw) {
					return errStructured(prefix, fmt.Errorf(
						"%s conflicts with %s — pass one of them, or the same value in both", a.source, b.source))
				}
				continue
			}
			if a.nested || b.nested {
				return errStructured(prefix, fmt.Errorf(
					"%s conflicts with %s — one key cannot be both a structured value and a string", a.source, b.source))
			}
			if a.value != b.value {
				return errStructured(prefix, fmt.Errorf(
					"%s conflicts with %s (%s vs %s) — pass one of them, or the same value in both",
					a.source, b.source, a.value, b.value))
			}
		}
	}
	return nil
}

// topLevelValueProvided reports whether a top-level param VALUE counts as
// supplied for the duplicate checks below (codex round 12).
//
// An empty string does not. That is the convention every schema-declared
// string on this tool follows — promotedParamValue treats "" as absent, the
// CLI's `status != ""` guards do, and `assign: ""` is documented as inert —
// so a client that zero-fills its optional params was being REFUSED for
// asking one question, on every promoted key at once.
//
// Round 10 fixed exactly this for the hierarchy keys and I never asked
// whether the same reasoning covered their siblings; it did, for all six.
// CONVE-18: the reviewer names an instance, the fix owes the population. The
// probe that drove all eight keys is what turned one named key into six —
// and, below, into the two that are NOT part of the class.
//
// THE COMPAT ID KEYS ARE EXCLUDED. For `assigned_user_id` / `agent_role_id`
// an empty string is not absence — it is a CLEAR to NULL, the deliberate
// v0.16 semantics (see dispatch_http_advanced.go, which forwards "" verbatim
// for exactly these two). So a blank there IS an effective directive and DOES
// conflict with a non-empty `fields` value, the same way `field:["parent="]`
// does. Applying the round-12 finding uniformly would have discarded a clear
// in favour of the `fields` value — a spurious refusal traded for a silent
// wrong write, which is the worse half of the trade.
//
// Both call sites consult this, including the compat block that the carve-out
// is FOR. That is not decoration: when only the promoted block called it, the
// carve-out was unreachable and a mutant deleting it survived every test.
func topLevelValueProvided(key string, v any) bool {
	if compatIDFieldKeys[key] {
		return true // "" is a clear here, not an absence
	}
	if str, isString := v.(string); isString && str == "" {
		return false
	}
	return true
}

// compatIDFieldKeys are the v0.16 remote-transport compat params. They are
// deliberately never schema-declared (see version.go), which is exactly why
// they need naming here: nothing else in this file knows they can arrive at
// the top level.
var compatIDFieldKeys = map[string]bool{
	"assigned_user_id": true,
	"agent_role_id":    true,
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
	// ONE conflict decision, over the canonical view of every source, run
	// whether or not a `fields` object is present (lead ruling after round
	// 13; reach corrected after round 14).
	if errRes := detectFieldConflicts("pad_item.create", input); errRes != nil {
		return errRes, nil
	}
	out, errRes := reshapeItemFields("pad_item.create", input)
	if errRes != nil {
		return errRes, nil
	}
	return env.Dispatch(ctx, []string{"item", "create"}, out)
}

func actionItemUpdate(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	// ONE conflict decision, over the canonical view of every source, run
	// whether or not a `fields` object is present (lead ruling after round
	// 13; reach corrected after round 14).
	if errRes := detectFieldConflicts("pad_item.update", input); errRes != nil {
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
		v, ok := input[alias]
		if !ok {
			continue
		}
		// AN EMPTY STRING IS NOT PROVIDED (codex round 10). Every declared
		// string param on this tool follows that convention — it is why
		// promotedParamValue treats "" as absent and why `assign: ""` is
		// deliberately inert. Counting it here made `parent: ""` collide with
		// a perfectly good `fields.plan`, refusing a call that asks for one
		// hierarchy directive and passes the other as a padded-out zero
		// value, which is exactly what a client that fills every declared
		// optional param does.
		//
		// NOT the same as an empty value in the `field` ARRAY or in `fields`:
		// `field:["parent="]` and `fields:{"parent":""}` are the documented
		// CLEAR signal (BUG-2013 / BUG-2078), so they are semantically
		// effective and stay conflicts. The asymmetry is real and load-
		// bearing: one is a param left blank, the other is an explicit
		// instruction that happens to look like one.
		if !topLevelValueProvided(alias, v) {
			continue
		}
		origHierarchyParams[alias] = v
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
			// AN EMPTY HIERARCHY VALUE HERE IS A SILENT NO-OP, so it is
			// refused rather than accepted (codex round 11).
			//
			// `fields:{"parent":""}` promotes onto the top-level `parent`
			// param, where BOTH doors then treat empty as NOT PROVIDED —
			// promotedParamValue on the remote side, the `parentRef != ""`
			// guard in cmd_item.go on the CLI side. So the call reported
			// success and detached nothing. That is the exact failure mode
			// this bug exists to remove, and it is worse here than elsewhere
			// because the caller's intent (detach) is unambiguous.
			//
			// Refused, not silently promoted to a clear: giving this door
			// clear semantics is a decision about what `fields` MEANS, and
			// v0.19 already made clear_parent the canonical detach precisely
			// so that an empty string would not have to carry it. Inventing
			// the semantics inside a bug fix is what the `null` refusal above
			// declines to do, for the same reason. The message names both
			// working forms so the caller is not merely blocked.
			if str, isString := v.(string); isString && str == "" {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s: an empty value here is silently ignored, not a detach — use clear_parent: true, or field: [\"%s=\"] if you want the raw fields_patch form", k, k))
			}
			if _, isString := v.(string); !isString {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s must be a string ref (e.g. %q) — a %T here would be read as a hierarchy directive and could detach the item", k, "PLAN-12", v))
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
		// THE v0.16 COMPAT ID PARAMS CONFLICT LIKE ANY OTHER KEY (codex
		// round 11). `assigned_user_id` / `agent_role_id` are accepted at the
		// top level as a documented, never-schema-declared compat form, so
		// they are invisible to padItemPromotedFieldKeys and took the generic
		// path — where the conflict check only looks at the `field` array.
		// With `assigned_user_id:"A"` plus `fields:{"assigned_user_id":"B"}`
		// the doors then disagreed outright: the remote mapper reads the
		// top-level A while stdio emits only `--field assigned_user_id=B`,
		// because the top-level form has no CLI flag behind it. One call,
		// two different people assigned.
		if compatIDFieldKeys[k] {
			// Conflicts are already decided; an equal duplicate just collapses
			// so exactly one form reaches dispatch.
			if existing, has := out[k]; has && topLevelValueProvided(k, existing) {
				delete(out, k)
			}
		}

		if identityRefFieldKeys[k] {
			if _, isString := v.(string); !isString {
				return nil, errStructured(prefix, fmt.Errorf(
					"fields.%s must be a string (a slug, email, or id) — a %T is silently dropped by one transport and rejected by the other", k, v))
			}
		}

		if padItemPromotedFieldKeys[k] {
			// Conflicts are decided; what is left here is EMISSION. An equal
			// duplicate in the field array is dropped so the value is written
			// once, through the dedicated param.
			if _, inArray := fieldByKey[k]; inArray {
				dropFieldKeys[k] = true
				delete(fieldByKey, k)
			}
			if existing, has := out[k]; has && !topLevelValueProvided(k, existing) {
				delete(out, k)
			} else if has {
				continue // equal duplicate, already applied
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
			// A structure has no key=value encoding; the native map carries it.
			continue
		}
		if _, has := fieldByKey[k]; has {
			// Known equal (the pass above refused anything else). Re-emit
			// canonically when the retained entry is padded, so every door
			// writes the key the caller meant.
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
