package items

import (
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ValidateFields checks field values against the collection schema.
// It validates required fields are present, types are correct, and select
// values are within the allowed options. It applies defaults for missing
// optional fields, mutating the fields map in place.
func ValidateFields(fields map[string]any, schema models.CollectionSchema) error {
	var errs []string

	for _, def := range schema.Fields {
		val, exists := fields[def.Key]

		// Apply default if field is missing and a default is defined
		if !exists || val == nil {
			if def.Required {
				if def.Default != nil {
					fields[def.Key] = def.Default
					continue
				}
				errs = append(errs, fmt.Sprintf("field %q is required", def.Key))
				continue
			}
			if def.Default != nil {
				fields[def.Key] = def.Default
			}
			continue
		}

		// Validate by type
		if err := validateFieldType(def, val); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("field validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}

func validateFieldType(def models.FieldDef, val any) error {
	switch def.Type {
	case "text", "url":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("field %q must be a string", def.Key)
		}
	case "number":
		switch val.(type) {
		case float64, int, int64, float32:
			// ok
		default:
			return fmt.Errorf("field %q must be a number", def.Key)
		}
	case "checkbox":
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("field %q must be a boolean", def.Key)
		}
	case "date":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("field %q must be a date string (ISO 8601)", def.Key)
		}
		if s != "" {
			// Accept YYYY-MM-DD or full RFC3339
			if _, err := time.Parse("2006-01-02", s); err != nil {
				if _, err := time.Parse(time.RFC3339, s); err != nil {
					return fmt.Errorf("field %q has invalid date format (expected YYYY-MM-DD or RFC3339)", def.Key)
				}
			}
		}
	case "select":
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("field %q must be a string", def.Key)
		}
		if s != "" && len(def.Options) > 0 {
			found := false
			for _, opt := range def.Options {
				if opt == s {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("field %q value %q is not in allowed options %v", def.Key, s, def.Options)
			}
		}
	case "multi_select":
		// Accept a slice of strings
		switch v := val.(type) {
		case []any:
			for i, item := range v {
				s, ok := item.(string)
				if !ok {
					return fmt.Errorf("field %q item %d must be a string", def.Key, i)
				}
				if len(def.Options) > 0 {
					found := false
					for _, opt := range def.Options {
						if opt == s {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("field %q value %q is not in allowed options %v", def.Key, s, def.Options)
					}
				}
			}
		case []string:
			for _, s := range v {
				if len(def.Options) > 0 {
					found := false
					for _, opt := range def.Options {
						if opt == s {
							found = true
							break
						}
					}
					if !found {
						return fmt.Errorf("field %q value %q is not in allowed options %v", def.Key, s, def.Options)
					}
				}
			}
		default:
			return fmt.Errorf("field %q must be an array of strings", def.Key)
		}
	case "relation":
		if _, ok := val.(string); !ok {
			return fmt.Errorf("field %q must be a string (item ID)", def.Key)
		}
	}
	return nil
}
