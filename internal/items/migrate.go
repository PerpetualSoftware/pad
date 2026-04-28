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

// MigrateFields maps field values from a source schema to a target schema.
// Fields with matching keys and compatible types are transferred.
// Incompatible or missing fields are dropped. Required target fields without
// values after migration are reported as errors.
func MigrateFields(
	currentFields map[string]any,
	sourceSchema []models.FieldDef,
	targetSchema []models.FieldDef,
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
