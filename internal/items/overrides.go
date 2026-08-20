package items

import (
	"sort"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// UndeclaredOverrideKeys returns the keys in an override map that the target
// schema does not declare, sorted so a caller's error message is stable for a
// given input. A nil or empty override map returns nil.
//
// It lives HERE, in the package both consumers already import, because two
// implementations of it is exactly the divergence PLAN-2357's DR-6 exists to
// prevent (Codex round 17). The cross-workspace copy PREFLIGHT refuses an
// undeclared override with a 400, and the COPY refuses it in
// Store.migrateCopyFields; if the two ever disagreed about which keys count as
// undeclared, the preview would promise something the copy refuses, or worse
// permit something the copy silently persists as an orphan key.
//
// The rule is a straight key-set difference with no exemptions. There is no
// reserved-key escape hatch in items.fields — PLAN-2357 DR-2 records why the
// cross-workspace provenance pointer needed its own table rather than a fields
// key — so every key the schema does not declare is an orphan.
//
// Why refusing is the right answer rather than dropping the key silently:
// ValidateFields ignores keys the schema does not declare, so an undeclared
// override that is merged is WRITTEN, invisible to every schema-driven
// surface. Dropping it instead would be no better — a client that asked for a
// value would get an item without it and no way to tell.
func UndeclaredOverrideKeys(overrides map[string]any, targetFields []models.FieldDef) []string {
	if len(overrides) == 0 {
		return nil
	}
	declared := make(map[string]struct{}, len(targetFields))
	for _, f := range targetFields {
		declared[f.Key] = struct{}{}
	}
	var bad []string
	for k := range overrides {
		if _, ok := declared[k]; !ok {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}

// ReservedFieldKeysIn returns the keys of a caller-supplied field map that name
// system metadata, sorted.
//
// Two kinds of map are gated by it, and they are gated for the same reason:
//
//   - FIELD-OVERRIDE maps, on every path that has one: the same-workspace move,
//     the copy preflight, and the mutating copy (the last two via
//     UndeclaredOverrideKeys against a stripped schema).
//   - `fields_patch`, the partial-update door every USER field-setter lowers
//     into — `pad item update --field`, the MCP `field` param on both
//     transports, and anything else PATCHing an item (BUG-2627 part 2).
//
// It was named ReservedOverrideKeys until the second caller arrived; the list
// and the semantics are unchanged.
//
// That is still NOT the same as "reserved keys are unwritable", and the
// remaining hole is deliberate rather than missed. A FULL `fields` blob reaches
// them, because that door is shared: `pad item note` / `pad item decide` /
// `pad github link` send one, and so does convention activation via
// models.BuildConventionItemFields → ItemCreate. Closing it would break the
// system writers it exists for. So item CREATE is still a mint site for a
// hand-written reserved key, tracked with the rest of the surface in BUG-2685.
//
// What the fields_patch gate buys is that the UPDATE door — the one a user or
// an agent actually reaches for, on all three transports — can no longer write
// a reserved key at all. That matters because the append helpers
// (AppendImplementationNote / AppendDecisionLogEntry) refuse rather than
// destroy an undecodable stored value (BUG-2627 part 3): a `--field` write
// that lands a malformed blob does not merely look wrong, it disables the
// legitimate append path for that item until someone repairs the row.
//
// On the copy it is worse than a validation hole: MigrateFields drops a
// referential key like github_pr when the item leaves its workspace (BUG-2674),
// and an override applied afterwards would put it straight back, defeating the
// scope rule by the simplest possible route.
//
// Callers that already run UndeclaredOverrideKeys against a schema stripped by
// SchemaForMigratedFields get this for free — a reserved key is undeclared
// there by construction. This exists for the paths with no schema gate at all.
func ReservedFieldKeysIn(fields map[string]any) []string {
	if len(fields) == 0 {
		return nil
	}
	var bad []string
	for k := range fields {
		if models.IsReservedItemField(k) {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}
