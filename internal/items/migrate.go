package items

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// MigrateResult holds the outcome of field migration between collection schemas.
type MigrateResult struct {
	// Fields contains the migrated field values for the target schema.
	Fields map[string]any
	// Dropped lists field keys that were dropped during migration (no matching target field or incompatible types).
	Dropped []string
	// Errors lists required target fields that have no value after migration.
	Errors []string
}

// MigrateScope says how far the item is travelling. It is a required argument
// rather than an option with a default because BOTH wrong answers lose
// something (BUG-2674): using SameWorkspace for a cross-workspace copy carries
// a github_pr into a workspace whose repository it does not describe, and using
// CrossWorkspace for an ordinary move DROPS that metadata from an item whose
// repo context never changed. A caller that has to name its scope cannot pick
// one by omission.
type MigrateScope int

const (
	// SameWorkspace — a collection change within one workspace. The
	// surrounding context (repository, members, conventions) is unchanged,
	// so referential system metadata still describes something true.
	SameWorkspace MigrateScope = iota
	// CrossWorkspace — the item is landing in a different workspace, where
	// a referent belonging to the source's context no longer holds.
	CrossWorkspace
)

// ScopeFor answers the one question MigrateScope asks: is the item landing in a
// different workspace? It lives here, in the package that defines the type, so
// the copy (store) and its preflight (server) cannot answer it differently —
// a preview promising a carry the copy then drops is the DR-6 divergence the
// shared-endpoint design exists to prevent, and two inline comparisons in two
// packages is exactly how that drift starts.
func ScopeFor(sourceWorkspaceID, targetWorkspaceID string) MigrateScope {
	if sourceWorkspaceID == targetWorkspaceID {
		return SameWorkspace
	}
	return CrossWorkspace
}

// MigrateFields maps field values from a source schema to a target schema.
// Fields with matching keys and compatible types are transferred.
// Incompatible or missing fields are dropped. Required target fields without
// values after migration are reported as errors.
//
// Reserved system metadata (models.IsReservedItemField) bypasses schema
// matching entirely — it is declared by no schema, so matching it against one
// is what destroyed it before BUG-2674. Referential reserved keys additionally
// depend on scope; see MigrateScope.
func MigrateFields(
	currentFields map[string]any,
	sourceSchema []models.FieldDef,
	targetSchema []models.FieldDef,
	scope MigrateScope,
) MigrateResult {
	result := MigrateResult{
		Fields: make(map[string]any),
	}

	// Build lookup of target fields by key
	targetDefs := make(map[string]models.FieldDef)
	for _, f := range targetSchema {
		targetDefs[f.Key] = f
	}

	// Build lookup of source fields by key
	sourceDefs := make(map[string]models.FieldDef)
	for _, f := range sourceSchema {
		sourceDefs[f.Key] = f
	}

	// Migrate each current field value
	for key, value := range currentFields {
		// Reserved keys are system-written metadata that no collection
		// schema declares (BUG-2674). Schema-matching them means they are
		// absent from every targetDefs and are dropped on every move —
		// which destroyed well-formed implementation notes, decision logs
		// and linked-PR metadata outright, silently, on a routine
		// operation. They carry through untouched instead.
		//
		// PLAN-2357 DR-17 already settled the principle for the analogous
		// case: tags carry because "there is no workspace-scoped foreign
		// key to break, so dropping them would lose information for no
		// safety reason". These are the same shape — inert JSON on the
		// row, nothing that could dangle in a destination collection or
		// workspace — and the plan's carry list simply never considered
		// them. There is no migration to attempt: they have no source or
		// target FieldDef to migrate BETWEEN.
		if models.IsReservedItemField(key) {
			// Referential metadata travels only as far as its referent's
			// context. Reported through the ordinary Dropped channel with
			// no special casing — a user losing a PR link should learn it
			// the same way they learn about any other dropped value
			// (PLAN-2357 DR-17: "None of this may be silent").
			if scope == CrossWorkspace && models.IsReferentialItemField(key) {
				result.Dropped = append(result.Dropped, key)
				continue
			}
			result.Fields[key] = value
			continue
		}

		targetField, exists := targetDefs[key]
		if !exists {
			result.Dropped = append(result.Dropped, key)
			continue
		}

		sourceField := sourceDefs[key]
		migrated, ok := migrateValue(value, sourceField.Type, targetField)
		if ok {
			result.Fields[key] = migrated
		} else {
			result.Dropped = append(result.Dropped, key)
		}
	}

	// Apply defaults for target fields not yet present
	for _, f := range targetSchema {
		if _, exists := result.Fields[f.Key]; exists {
			continue
		}
		if f.Default != nil && f.Default != "" {
			result.Fields[f.Key] = f.Default
		} else if f.Required {
			result.Errors = append(result.Errors, fmt.Sprintf("required field %q has no value", f.Key))
		}
	}

	return result
}

// migrateValue attempts to convert a value from sourceType to the targetField's type.
// Returns the migrated value and true if successful, or zero-value and false if incompatible.
func migrateValue(value any, sourceType string, target models.FieldDef) (any, bool) {
	targetType := target.Type

	// Same type — validate further for select fields
	if sourceType == targetType {
		if targetType == "select" && target.Options != nil {
			strVal := fmt.Sprintf("%v", value)
			for _, opt := range target.Options {
				if opt == strVal {
					return value, true
				}
			}
			// Value not in target options — drop it
			return nil, false
		}
		return value, true
	}

	// Compatible type conversions
	strVal := fmt.Sprintf("%v", value)
	switch {
	case sourceType == "text" && targetType == "url":
		return value, true
	case sourceType == "url" && targetType == "text":
		return value, true
	case sourceType == "number" && targetType == "text":
		return strVal, true
	case sourceType == "select" && targetType == "text":
		return strVal, true
	case sourceType == "checkbox" && targetType == "text":
		return strVal, true
	case sourceType == "text" && targetType == "number":
		if _, err := strconv.ParseFloat(strVal, 64); err == nil {
			return value, true
		}
		return nil, false
	default:
		return nil, false
	}
}

// StillDropped filters a MigrateResult.Dropped list down to the keys that are
// genuinely absent from the FINAL field map.
//
// Dropped is computed during migration, before field overrides are merged and
// before validation injects destination defaults. A key can therefore be listed
// as dropped and then supplied moments later — a number→select value migration
// rejected, say, that the caller replaced with a valid one in the same request.
// Reporting it anyway states something false about the item that is about to be
// written, and a report that cries loss over data the user can see on the item
// is worse than no report at all: it teaches the reader to distrust the channel.
//
// Returns a sorted slice so callers rendering it get stable output.
func StillDropped(dropped []string, finalFields map[string]any) []string {
	if len(dropped) == 0 {
		return nil
	}
	out := make([]string, 0, len(dropped))
	seen := make(map[string]struct{}, len(dropped))
	for _, key := range dropped {
		// PRESENT-AND-NON-NIL is the test, not mere presence. The move path
		// writes overrides straight into the map including a nil one, where
		// the copy path deletes the key instead (see migrateCopyFields) — so
		// on a move, `{"due_date": null}` leaves the key present with a nil
		// value. Treating that as restored would suppress a REAL drop on the
		// strength of a key that carries nothing, which is the silent loss
		// this whole change exists to end (Codex round 3).
		if v, restored := finalFields[key]; restored && v != nil {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

// SchemaForMigratedFields returns schema with any reserved-key FieldDef
// removed, for validating the OUTPUT of MigrateFields.
//
// Only migration and copy need this, which is exactly why it is a separate
// function rather than a skip inside ValidateFieldsDetailed (Codex round 3).
// That validator is shared with create, full update and every bulk path, and a
// blanket skip there would stop enforcing a grandfathered declaration on the
// paths where the user really is authoring that key — letting arbitrary junk
// into implementation_notes through create while fields_patch, which uses
// ValidatePartialFields, kept rejecting it. Full and partial updates
// disagreeing about the same key is a worse bug than the one being fixed.
//
// The narrow problem it does solve: collection schemas may not declare a
// reserved key since BUG-2674, but that gate GRANDFATHERS existing ones, so a
// legacy `implementation_notes: text` FieldDef still exists in the wild.
// MigrateFields hands reserved keys through by identity — they are system-owned
// and schema-matching them is what destroyed them — so validating the migrated
// map against that FieldDef would meet the notes ARRAY and reject it, failing
// every move and copy for a collection whose only sin is a field name someone
// was once allowed to pick.
//
// Returns schema unchanged (no copy) when it declares no reserved key, which is
// every collection that has not been grandfathered.
func SchemaForMigratedFields(schema models.CollectionSchema) models.CollectionSchema {
	var reserved bool
	for _, f := range schema.Fields {
		if models.IsReservedItemField(f.Key) {
			reserved = true
			break
		}
	}
	if !reserved {
		return schema
	}

	out := schema
	out.Fields = make([]models.FieldDef, 0, len(schema.Fields))
	for _, f := range schema.Fields {
		if models.IsReservedItemField(f.Key) {
			continue
		}
		out.Fields = append(out.Fields, f)
	}
	return out
}
