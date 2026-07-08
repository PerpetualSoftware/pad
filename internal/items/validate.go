package items

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// patternCache memoizes compiled regexes so repeat validations don't pay the
// re-compile cost. Schemas change rarely; the entries are tiny.
var (
	patternCache   = make(map[string]*regexp.Regexp)
	patternCacheMu sync.RWMutex
)

func compilePattern(pat string) (*regexp.Regexp, error) {
	patternCacheMu.RLock()
	re, ok := patternCache[pat]
	patternCacheMu.RUnlock()
	if ok {
		return re, nil
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return nil, err
	}
	patternCacheMu.Lock()
	patternCache[pat] = re
	patternCacheMu.Unlock()
	return re, nil
}

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

// ValidatePartialFields validates ONLY the keys present in `patch` against
// the schema (TASK-2022, field-level PATCH / IDEA-1480). Unlike
// ValidateFields it does NOT enforce required-field presence and does NOT
// inject schema defaults — a partial patch is "change exactly these keys,
// leave everything else alone," so absent keys are neither missing nor
// candidates for default population. Keys the schema doesn't declare (orphan
// keys) are accepted and persist unchanged, matching the full-blob path.
//
// A nil value marks a key for DELETION (see store.mergeFieldsPatch); those
// are skipped here since there's no value to type-check.
func ValidatePartialFields(patch map[string]any, schema models.CollectionSchema) error {
	// Index declared fields by key for O(1) lookup.
	defByKey := make(map[string]models.FieldDef, len(schema.Fields))
	for _, def := range schema.Fields {
		defByKey[def.Key] = def
	}

	var errs []string
	for key, val := range patch {
		if val == nil {
			// Deletion sentinel — nothing to type-check.
			continue
		}
		def, ok := defByKey[key]
		if !ok {
			// Orphan key (not in schema) — allowed, persists as-is.
			continue
		}
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
	case "json":
		// Accept only structured JSON values (object, array, null). Raw
		// strings / numbers / bools are rejected so a generic text input in
		// the UI can't silently corrupt a structured field (e.g. emitting
		// the string "[]" instead of an actual array). Callers that want a
		// scalar field should use "text", "number", or "checkbox".
		switch val.(type) {
		case map[string]any, []any, nil:
			// ok
		default:
			return fmt.Errorf("field %q must be a JSON object, array, or null", def.Key)
		}
	}

	// Pattern check applies to string-typed values (text, url, and JSON strings).
	if def.Pattern != "" {
		s, ok := val.(string)
		if ok && s != "" {
			re, err := compilePattern(def.Pattern)
			if err != nil {
				return fmt.Errorf("field %q has an invalid pattern in its schema: %v", def.Key, err)
			}
			if !re.MatchString(s) {
				return fmt.Errorf("field %q value %q does not match required pattern %q", def.Key, s, def.Pattern)
			}
		}
	}
	return nil
}
