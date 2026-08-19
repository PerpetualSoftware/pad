package items

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMigrateFields_MatchingTypes(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}},
		{Key: "priority", Type: "select", Options: []string{"low", "high"}},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "closed"}, Required: true},
		{Key: "priority", Type: "select", Options: []string{"low", "medium", "high"}},
	}
	fields := map[string]any{"status": "open", "priority": "high"}

	result := MigrateFields(fields, source, target)

	if result.Fields["status"] != "open" {
		t.Errorf("status: got %v, want 'open'", result.Fields["status"])
	}
	if result.Fields["priority"] != "high" {
		t.Errorf("priority: got %v, want 'high'", result.Fields["priority"])
	}
	if len(result.Dropped) != 0 {
		t.Errorf("dropped: got %v, want none", result.Dropped)
	}
}

func TestMigrateFields_SelectValueNotInTarget(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "in-progress", "done"}},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"todo", "doing", "done"}, Required: true, Default: "todo"},
	}
	fields := map[string]any{"status": "in-progress"}

	result := MigrateFields(fields, source, target)

	// "in-progress" is not in target options, should be dropped and default applied
	if result.Fields["status"] != "todo" {
		t.Errorf("status: got %v, want 'todo' (default after drop)", result.Fields["status"])
	}
}

func TestMigrateFields_DropsExtraFields(t *testing.T) {
	source := []models.FieldDef{
		{Key: "severity", Type: "select", Options: []string{"low", "high"}},
		{Key: "browser", Type: "text"},
	}
	target := []models.FieldDef{
		{Key: "priority", Type: "select", Options: []string{"low", "high"}},
	}
	fields := map[string]any{"severity": "high", "browser": "Chrome"}

	result := MigrateFields(fields, source, target)

	if len(result.Dropped) != 2 {
		t.Errorf("dropped: got %d, want 2", len(result.Dropped))
	}
}

func TestMigrateFields_TypeConversion(t *testing.T) {
	source := []models.FieldDef{
		{Key: "count", Type: "number"},
		{Key: "status", Type: "select", Options: []string{"open"}},
	}
	target := []models.FieldDef{
		{Key: "count", Type: "text"},
		{Key: "status", Type: "text"},
	}
	fields := map[string]any{"count": 42, "status": "open"}

	result := MigrateFields(fields, source, target)

	if result.Fields["count"] != "42" {
		t.Errorf("count: got %v, want '42'", result.Fields["count"])
	}
	if result.Fields["status"] != "open" {
		t.Errorf("status: got %v, want 'open'", result.Fields["status"])
	}
}

func TestMigrateFields_RequiredFieldMissing(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open"}, Required: true},
	}
	fields := map[string]any{}

	result := MigrateFields(fields, source, target)

	if len(result.Errors) != 1 {
		t.Errorf("errors: got %d, want 1", len(result.Errors))
	}
}

func TestMigrateFields_DefaultApplied(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Required: true, Default: "open"},
	}
	fields := map[string]any{}

	result := MigrateFields(fields, source, target)

	if result.Fields["status"] != "open" {
		t.Errorf("status: got %v, want 'open'", result.Fields["status"])
	}
	if len(result.Errors) != 0 {
		t.Errorf("errors: got %v, want none", result.Errors)
	}
}

// BUG-2674. MigrateFields dropped every key absent from the TARGET schema, and
// implementation_notes / decision_log / github_pr / convention are reserved
// metadata that no schema declares — so a routine `pad item move` destroyed
// well-formed notes, decision logs and linked-PR metadata outright, silently,
// and reported success. Reproduced live before the fix: a note written through
// `pad item note` was gone after moving the item to another collection.
//
// The assertion is deliberately on the VALUE surviving intact, not merely on
// the key being present: a carry-through that re-encoded or zeroed the payload
// would satisfy a presence check while still losing the notes (CONVE-12).
func TestMigrateFields_ReservedKeysCarryThroughUntouched(t *testing.T) {
	source := []models.FieldDef{{Key: "severity", Type: "select", Options: []string{"low", "high"}}}
	target := []models.FieldDef{{Key: "priority", Type: "select", Options: []string{"low", "high"}}}

	notes := []any{map[string]any{"id": "note-1", "summary": "carried"}}
	decisions := []any{map[string]any{"id": "decision-1", "decision": "carried"}}
	pr := map[string]any{"number": float64(42), "url": "https://example.invalid/42"}
	convention := map[string]any{"trigger": "always", "enforcement": "must"}

	fields := map[string]any{
		"implementation_notes": notes,
		"decision_log":         decisions,
		"github_pr":            pr,
		"convention":           convention,
		"severity":             "high", // an ordinary key with no target home
	}

	result := MigrateFields(fields, source, target)

	for key, want := range map[string]any{
		"implementation_notes": notes,
		"decision_log":         decisions,
		"github_pr":            pr,
		"convention":           convention,
	} {
		got, ok := result.Fields[key]
		if !ok {
			t.Errorf("%s must carry through a move, got dropped", key)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s must carry through UNTOUCHED:\n got %#v\nwant %#v", key, got, want)
		}
		for _, d := range result.Dropped {
			if d == key {
				t.Errorf("%s must not be reported dropped", key)
			}
		}
	}

	// Control: the ordinary key with no home in the target still drops, and
	// still reports. Without this leg, "carry everything" passes.
	if _, ok := result.Fields["severity"]; ok {
		t.Error("severity has no target field and must still drop")
	}
	if len(result.Dropped) != 1 || result.Dropped[0] != "severity" {
		t.Errorf("dropped: got %#v, want exactly [severity]", result.Dropped)
	}
}

// Reserved keys are carried by IDENTITY, not by name-matching against the
// target schema — so they survive even when the target happens to declare a
// field of the same name, and they are never run through migrateValue.
func TestMigrateFields_ReservedKeysBypassSchemaMatching(t *testing.T) {
	source := []models.FieldDef{}
	// A pathological target that declares implementation_notes as a plain
	// text field. Type-migrating a []any into "text" is exactly the kind of
	// coercion that would corrupt or drop the payload.
	target := []models.FieldDef{{Key: "implementation_notes", Type: "text"}}

	notes := []any{map[string]any{"id": "note-1", "summary": "carried"}}
	result := MigrateFields(map[string]any{"implementation_notes": notes}, source, target)

	got, ok := result.Fields["implementation_notes"]
	if !ok {
		t.Fatal("implementation_notes must carry through even when the target declares the key")
	}
	if !reflect.DeepEqual(got, notes) {
		t.Errorf("payload must not be coerced by the target FieldDef:\n got %#v\nwant %#v", got, notes)
	}
}
