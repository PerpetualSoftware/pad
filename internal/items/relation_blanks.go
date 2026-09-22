package items

import (
	"sort"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3028: a scalar `relation` field had three stored spellings of "no
// target" — the key absent, `""`, and whitespace-only — and `required` was
// enforced only against the first, so a required relation holding `""`
// validated. The canonical form is the one multi_relation already uses (U4):
// the key ABSENT. These helpers find and remove the other two spellings.
//
// Which blanks a door removes BEFORE validation and which AFTER is the whole
// of the provenance rule (the lead's ruling on BUG-3028): a blank the write
// itself SUPPLIES is removed first, so the ordinary required check refuses it;
// a legacy blank the write merely CARRIES is removed after, so it becomes
// absent without refusing a write that never touched the field.

// IsBlankRelationValue reports whether v is a string that trims to "" — one
// of the two non-canonical spellings of "no target".
func IsBlankRelationValue(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// BlankRelationKeys returns, sorted, the keys of fields that the schema
// declares as a scalar `relation` and whose value is blank.
func BlankRelationKeys(fields map[string]any, schema models.CollectionSchema) []string {
	var keys []string
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		if v, ok := fields[def.Key]; ok && IsBlankRelationValue(v) {
			keys = append(keys, def.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// DropBlankRelations deletes the blank scalar relation keys for which keep is
// false (every one when keep is nil) and returns the keys it deleted.
func DropBlankRelations(fields map[string]any, schema models.CollectionSchema, keep func(key string) bool) []string {
	var dropped []string
	for _, k := range BlankRelationKeys(fields, schema) {
		if keep != nil && keep(k) {
			continue
		}
		delete(fields, k)
		dropped = append(dropped, k)
	}
	return dropped
}

// ScalarRelationKeys returns the keys the schema declares as scalar
// `relation`, in schema order.
func ScalarRelationKeys(schema models.CollectionSchema) []string {
	var keys []string
	for _, def := range schema.Fields {
		if def.Type == "relation" {
			keys = append(keys, def.Key)
		}
	}
	return keys
}
