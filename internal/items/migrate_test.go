package items

import (
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
