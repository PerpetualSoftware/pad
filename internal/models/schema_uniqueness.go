package models

import "fmt"

// ValidateSchemaKeysAndOptions refuses a collection schema in which a field
// has no key, two fields share a key, or a select or multi_select field's
// options include an empty value or the same value twice (TASK-3539). Every client renders a
// collection's fields and options as lists keyed by those values, and a
// repeated key throws during render and blanks the view (BUG-3538); a field
// key named twice is also ambiguous for every reader that looks one up.
//
// It is a WRITE check: called on collection create and update, never on a
// read, an import or a migration, so a schema already stored with a repeat
// keeps working (the web normalises it for rendering) until someone edits it,
// and the error then names exactly what to fix. Values compare exactly, as
// stored.
func ValidateSchemaKeysAndOptions(schema CollectionSchema) error {
	seen := make(map[string]bool, len(schema.Fields))
	for i, f := range schema.Fields {
		if f.Key == "" {
			return fmt.Errorf("field %d (label %q) has no key", i+1, f.Label)
		}
		if seen[f.Key] {
			return fmt.Errorf("field key %q is defined more than once", f.Key)
		}
		seen[f.Key] = true
		// Options are a list the clients render only for a select or a
		// multi_select. Another type can carry a legacy `options` list (the old
		// field DSL put a third part there for every type), and refusing that
		// would block an unrelated schema save.
		if f.Type != "select" && f.Type != "multi_select" {
			continue
		}
		options := make(map[string]bool, len(f.Options))
		for _, o := range f.Options {
			if o == "" {
				return fmt.Errorf("field %q has an empty option", f.Key)
			}
			if options[o] {
				return fmt.Errorf("field %q lists the option %q more than once", f.Key, o)
			}
			options[o] = true
		}
	}
	return nil
}
