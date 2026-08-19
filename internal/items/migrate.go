package items

import (
	"fmt"
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
