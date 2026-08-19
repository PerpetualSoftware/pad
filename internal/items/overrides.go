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

// ReservedOverrideKeys returns the override keys naming system metadata, sorted.
//
// Reserved keys are never settable through a field-override map, on any path.
// They are written by Pad's own endpoints — implementation notes and decision
// log entries through their append helpers (which carry BUG-2627's
// refuse-rather-than-destroy guard), github_pr through the GitHub link flow —
// and an override reaching them bypasses every one of those checks.
//
// On the copy it is worse than a validation hole: MigrateFields drops a
// referential key like github_pr when the item leaves its workspace (BUG-2674),
// and an override applied afterwards would put it straight back, defeating the
// scope rule by the simplest possible route.
//
// Callers that already run UndeclaredOverrideKeys against a schema stripped by
// SchemaForMigratedFields get this for free — a reserved key is undeclared
// there by construction. This exists for the paths with no schema gate at all.
func ReservedOverrideKeys(overrides map[string]any) []string {
	if len(overrides) == 0 {
		return nil
	}
	var bad []string
	for k := range overrides {
		if models.IsReservedItemField(k) {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}
