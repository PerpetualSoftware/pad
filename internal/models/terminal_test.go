package models

import (
	"reflect"
	"testing"
)

func TestDoneFieldKey_DefaultsToStatus(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "status", Type: "select", Options: []string{"open", "done"}},
		},
	}
	settings := CollectionSettings{}
	if got := DoneFieldKey(schema, settings); got != "status" {
		t.Fatalf("expected default done field to be 'status', got %q", got)
	}
}

func TestDoneFieldKey_HonorsBoardGroupBy(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "status", Type: "select", Options: []string{"open", "done"}},
			{Key: "resolution", Type: "select", Options: []string{"fixed", "wontfix"}},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "resolution"}
	if got := DoneFieldKey(schema, settings); got != "resolution" {
		t.Fatalf("expected done field to follow board_group_by, got %q", got)
	}
}

func TestDoneFieldKey_FallsBackWhenFieldMissing(t *testing.T) {
	// BoardGroupBy names a field that doesn't exist on the schema —
	// fall back to status rather than emitting a broken JSON path.
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "status", Type: "select", Options: []string{"open"}},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "ghost"}
	if got := DoneFieldKey(schema, settings); got != "status" {
		t.Fatalf("expected fallback to 'status' for missing field, got %q", got)
	}
}

func TestDoneFieldKey_FallsBackForNonSelectField(t *testing.T) {
	// If board_group_by somehow points at a non-select field, done-
	// detection can't meaningfully operate on it — fall back to status.
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "status", Type: "select", Options: []string{"open"}},
			{Key: "notes", Type: "text"},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "notes"}
	if got := DoneFieldKey(schema, settings); got != "status" {
		t.Fatalf("expected fallback to 'status' for non-select field, got %q", got)
	}
}

func TestDoneFieldKey_AcceptsMultiSelect(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "labels", Type: "multi_select", Options: []string{"p0", "done"}},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "labels"}
	if got := DoneFieldKey(schema, settings); got != "labels" {
		t.Fatalf("expected multi_select to qualify as done field, got %q", got)
	}
}

func TestTerminalValuesForDoneField_UsesFieldTerminals(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{
				Key:             "resolution",
				Type:            "select",
				Options:         []string{"fixed", "wontfix", "open"},
				TerminalOptions: []string{"fixed", "wontfix"},
			},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "resolution"}
	key, values := TerminalValuesForDoneField(schema, settings)
	if key != "resolution" {
		t.Fatalf("expected key 'resolution', got %q", key)
	}
	if !reflect.DeepEqual(values, []string{"fixed", "wontfix"}) {
		t.Fatalf("expected resolution terminals, got %v", values)
	}
}

func TestTerminalValuesForDoneField_FallsBackToDefaults(t *testing.T) {
	// Resolved field exists but has no terminal_options set — fall back
	// to the global default list so collections without schema-declared
	// terminals continue to function.
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "status", Type: "select", Options: []string{"open", "done"}},
		},
	}
	settings := CollectionSettings{}
	key, values := TerminalValuesForDoneField(schema, settings)
	if key != "status" {
		t.Fatalf("expected key 'status', got %q", key)
	}
	if !reflect.DeepEqual(values, DefaultTerminalStatuses) {
		t.Fatalf("expected DefaultTerminalStatuses, got %v", values)
	}
}

func TestTerminalValuesForDoneField_StatusTerminalsStillWork(t *testing.T) {
	// Back-compat: a collection whose status field has TerminalOptions set
	// should still produce those terminals when board_group_by is empty.
	schema := CollectionSchema{
		Fields: []FieldDef{
			{
				Key:             "status",
				Type:            "select",
				Options:         []string{"open", "done", "cancelled"},
				TerminalOptions: []string{"done", "cancelled"},
			},
		},
	}
	settings := CollectionSettings{}
	key, values := TerminalValuesForDoneField(schema, settings)
	if key != "status" {
		t.Fatalf("expected key 'status', got %q", key)
	}
	if !reflect.DeepEqual(values, []string{"done", "cancelled"}) {
		t.Fatalf("expected status terminals, got %v", values)
	}
}

func TestTerminalPlaceholdersForDoneField_LowercasesArgs(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{
				Key:             "resolution",
				Type:            "select",
				Options:         []string{"Fixed", "WontFix", "Open"},
				TerminalOptions: []string{"Fixed", "WontFix"},
			},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "resolution"}
	key, placeholders, args := TerminalPlaceholdersForDoneField(schema, settings)
	if key != "resolution" {
		t.Fatalf("expected key 'resolution', got %q", key)
	}
	if placeholders != "?,?" {
		t.Fatalf("expected '?,?', got %q", placeholders)
	}
	if !reflect.DeepEqual(args, []any{"fixed", "wontfix"}) {
		t.Fatalf("expected lowercased args, got %v", args)
	}
}

func TestIsTerminalItem_TrueWhenFieldMatches(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{
				Key:             "resolution",
				Type:            "select",
				Options:         []string{"fixed", "wontfix", "open"},
				TerminalOptions: []string{"fixed", "wontfix"},
			},
		},
	}
	settings := CollectionSettings{BoardGroupBy: "resolution"}
	if !IsTerminalItem(map[string]any{"resolution": "fixed"}, schema, settings) {
		t.Fatal("expected item with resolution=fixed to be terminal")
	}
	if IsTerminalItem(map[string]any{"resolution": "open"}, schema, settings) {
		t.Fatal("expected item with resolution=open NOT to be terminal")
	}
}

func TestIsTerminalItem_FalseWhenFieldMissing(t *testing.T) {
	schema := CollectionSchema{
		Fields: []FieldDef{
			{Key: "status", Type: "select", Options: []string{"open", "done"}},
		},
	}
	settings := CollectionSettings{}
	// No "status" key in the item's fields map — treat as not terminal.
	if IsTerminalItem(map[string]any{}, schema, settings) {
		t.Fatal("expected item with no status field to be non-terminal")
	}
}

func TestIsTerminalItem_CaseInsensitive(t *testing.T) {
	// Queries in the SQL layer lowercase both sides; the Go-side helper
	// should match that semantics so behavior is consistent regardless of
	// which path a caller takes.
	schema := CollectionSchema{
		Fields: []FieldDef{
			{
				Key:             "status",
				Type:            "select",
				Options:         []string{"Open", "Done"},
				TerminalOptions: []string{"Done"},
			},
		},
	}
	settings := CollectionSettings{}
	if !IsTerminalItem(map[string]any{"status": "done"}, schema, settings) {
		t.Fatal("expected case-insensitive match to terminal option")
	}
	if !IsTerminalItem(map[string]any{"status": "DONE"}, schema, settings) {
		t.Fatal("expected case-insensitive match to terminal option (uppercase)")
	}
}

func TestTerminalStatusesFromSchema_CompatShim(t *testing.T) {
	// The legacy API should still pick up status-field terminals.
	schema := CollectionSchema{
		Fields: []FieldDef{
			{
				Key:             "status",
				Type:            "select",
				Options:         []string{"open", "done"},
				TerminalOptions: []string{"done"},
			},
		},
	}
	got := TerminalStatusesFromSchema(schema)
	if !reflect.DeepEqual(got, []string{"done"}) {
		t.Fatalf("expected ['done'], got %v", got)
	}
}

func TestIsTerminalStatusDefault(t *testing.T) {
	if !IsTerminalStatusDefault("done") {
		t.Fatal("expected 'done' to be default-terminal")
	}
	if !IsTerminalStatusDefault("DONE") {
		t.Fatal("expected case-insensitive default-terminal match")
	}
	if IsTerminalStatusDefault("in-progress") {
		t.Fatal("expected 'in-progress' to NOT be default-terminal")
	}
}
