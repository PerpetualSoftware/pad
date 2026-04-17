package models

import "strings"

// DefaultTerminalStatuses is the fallback list used when a collection's
// done field has no `terminal_options` declared on its schema. This is the
// union of all historically hardcoded terminal status values across the
// codebase.
var DefaultTerminalStatuses = []string{
	"done", "completed", "resolved", "cancelled", "rejected",
	"wontfix", "fixed", "implemented", "archived", "disabled", "deprecated",
}

// DoneFieldKey resolves which field on a collection's schema represents
// "is this item done?". The resolution is:
//
//  1. If CollectionSettings.BoardGroupBy names a select / multi_select
//     field on the schema, use that. This lets a collection whose board
//     is organized by e.g. "resolution" naturally drive done-detection
//     from the same field.
//
//  2. Otherwise, fall back to the literal key "status". Every collection
//     shipped today groups by status by default, so this preserves
//     existing behavior for all pre-TASK-604 collections.
//
// The function does not assume the resolved key actually exists on the
// schema — callers should treat the return value as "the field key whose
// value we read from items.fields JSON". A missing field on an item just
// means we read an empty string, which is the safe, expected behavior for
// unstyled items.
func DoneFieldKey(schema CollectionSchema, settings CollectionSettings) string {
	candidate := strings.TrimSpace(settings.BoardGroupBy)
	if candidate == "" {
		return "status"
	}
	for _, f := range schema.Fields {
		if f.Key == candidate && (f.Type == "select" || f.Type == "multi_select") {
			return candidate
		}
	}
	return "status"
}

// TerminalValuesForDoneField returns the resolved done-field key and the
// list of terminal values for that field. If the resolved field has no
// terminal_options set on the schema, falls back to DefaultTerminalStatuses
// so existing collections without schema-declared terminals continue to
// work.
func TerminalValuesForDoneField(
	schema CollectionSchema,
	settings CollectionSettings,
) (fieldKey string, values []string) {
	fieldKey = DoneFieldKey(schema, settings)
	for _, f := range schema.Fields {
		if f.Key == fieldKey && (f.Type == "select" || f.Type == "multi_select") {
			if len(f.TerminalOptions) > 0 {
				return fieldKey, f.TerminalOptions
			}
			break
		}
	}
	return fieldKey, DefaultTerminalStatuses
}

// TerminalPlaceholdersForDoneField is a SQL-layer convenience that returns
// the done-field key plus the placeholder + args pair needed for an IN
// clause. All values are lowercased to match the WHERE clause pattern used
// across the codebase (LOWER(json_extract(...)) IN (?, ?, ?)).
func TerminalPlaceholdersForDoneField(
	schema CollectionSchema,
	settings CollectionSettings,
) (fieldKey string, placeholders string, args []any) {
	key, values := TerminalValuesForDoneField(schema, settings)
	ph := make([]string, len(values))
	ar := make([]any, len(values))
	for i, v := range values {
		ph[i] = "?"
		ar[i] = strings.ToLower(v)
	}
	return key, strings.Join(ph, ","), ar
}

// IsTerminalItem reports whether an item's fields map indicates the item
// is in a terminal state for its collection. This is the canonical Go-side
// "is done" check when the caller has both the item's parsed fields and
// the collection's schema + settings in scope.
func IsTerminalItem(
	itemFields map[string]any,
	schema CollectionSchema,
	settings CollectionSettings,
) bool {
	key, values := TerminalValuesForDoneField(schema, settings)
	raw, ok := itemFields[key]
	if !ok {
		return false
	}
	s, ok := raw.(string)
	if !ok {
		return false
	}
	lower := strings.ToLower(s)
	for _, v := range values {
		if strings.ToLower(v) == lower {
			return true
		}
	}
	return false
}

// ── Back-compat wrappers ────────────────────────────────────────────────
// The original API (status-only) stays in place so callers that don't yet
// have CollectionSettings in scope keep working unchanged. Internally each
// of these delegates to the new done-field-aware implementation with
// empty settings — which resolves the done field to "status", matching
// pre-TASK-604 behavior byte-for-byte.

// TerminalStatusesFromSchema extracts terminal status options from a
// CollectionSchema. If the `status` field has TerminalOptions set, returns
// those. Otherwise returns DefaultTerminalStatuses.
//
// Deprecated: prefer TerminalValuesForDoneField when settings are in
// scope. This wrapper forces done-field resolution to the literal
// "status" key; callers that want to honor the collection's configured
// board_group_by should migrate to the settings-aware API.
func TerminalStatusesFromSchema(schema CollectionSchema) []string {
	_, values := TerminalValuesForDoneField(schema, CollectionSettings{})
	return values
}

// IsTerminalStatus checks whether a status string is terminal given a
// schema. Like the status extract above, this is hardcoded to the `status`
// field — it takes a pre-extracted status string and checks membership
// against that field's terminal options. Use when you already know you're
// working with the status field specifically (e.g. link-payload joins).
func IsTerminalStatus(status string, schema CollectionSchema) bool {
	lower := strings.ToLower(status)
	for _, ts := range TerminalStatusesFromSchema(schema) {
		if strings.ToLower(ts) == lower {
			return true
		}
	}
	return false
}

// IsTerminalStatusDefault checks using the default fallback list (for
// cases where no collection schema is available).
func IsTerminalStatusDefault(status string) bool {
	lower := strings.ToLower(status)
	for _, ts := range DefaultTerminalStatuses {
		if ts == lower {
			return true
		}
	}
	return false
}

// TerminalStatusPlaceholders returns a comma-separated placeholder string
// and the corresponding args slice for use in SQL IN clauses. Like the
// *FromSchema variant it is hardcoded to the `status` field; prefer
// TerminalPlaceholdersForDoneField when settings are in scope.
func TerminalStatusPlaceholders(schema CollectionSchema) (string, []any) {
	_, placeholders, args := TerminalPlaceholdersForDoneField(schema, CollectionSettings{})
	return placeholders, args
}

// DefaultTerminalStatusPlaceholders returns placeholders and args for the
// default terminal statuses list.
func DefaultTerminalStatusPlaceholders() (string, []any) {
	placeholders := make([]string, len(DefaultTerminalStatuses))
	args := make([]any, len(DefaultTerminalStatuses))
	for i, s := range DefaultTerminalStatuses {
		placeholders[i] = "?"
		args[i] = s
	}
	return strings.Join(placeholders, ","), args
}
