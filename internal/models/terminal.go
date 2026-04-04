package models

import "strings"

// DefaultTerminalStatuses is the fallback list used when a collection schema
// does not declare terminal_options on its status field. This is the union of
// all historically hardcoded terminal status values across the codebase.
var DefaultTerminalStatuses = []string{
	"done", "completed", "resolved", "cancelled", "rejected",
	"wontfix", "fixed", "implemented", "archived", "disabled", "deprecated",
}

// TerminalStatusesFromSchema extracts terminal status options from a
// CollectionSchema. If the status field has TerminalOptions set, returns
// those. Otherwise returns DefaultTerminalStatuses.
func TerminalStatusesFromSchema(schema CollectionSchema) []string {
	for _, f := range schema.Fields {
		if f.Key == "status" && (f.Type == "select" || f.Type == "multi_select") {
			if len(f.TerminalOptions) > 0 {
				return f.TerminalOptions
			}
			break
		}
	}
	return DefaultTerminalStatuses
}

// IsTerminalStatus checks whether a status string is terminal given a schema.
func IsTerminalStatus(status string, schema CollectionSchema) bool {
	lower := strings.ToLower(status)
	for _, ts := range TerminalStatusesFromSchema(schema) {
		if strings.ToLower(ts) == lower {
			return true
		}
	}
	return false
}

// IsTerminalStatusDefault checks using the default fallback list (for cases
// where no collection schema is available).
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
// and the corresponding args slice for use in SQL IN clauses.
// Example: TerminalStatusPlaceholders(schema) might return ("?,?,?", ["done","cancelled","rejected"])
func TerminalStatusPlaceholders(schema CollectionSchema) (string, []any) {
	statuses := TerminalStatusesFromSchema(schema)
	placeholders := make([]string, len(statuses))
	args := make([]any, len(statuses))
	for i, s := range statuses {
		placeholders[i] = "?"
		args[i] = strings.ToLower(s)
	}
	return strings.Join(placeholders, ","), args
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
