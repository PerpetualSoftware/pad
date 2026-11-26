package items

import (
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMigrateFields_BlankSelectPreserved_BUG3694(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open"},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open"},
	}
	current := map[string]any{"status": ""}

	result := MigrateFields(current, source, target, SameWorkspace)

	got, ok := result.Fields["status"]
	if !ok {
		t.Fatalf("status missing from result.Fields: %#v", result.Fields)
	}
	if got != "" {
		t.Errorf("status = %#v, want \"\"", got)
	}
	if slices.Contains(result.Dropped, "status") {
		t.Errorf("status must not be in Dropped, got %#v", result.Dropped)
	}
}

func TestMigrateFields_SelectEmptyOptionsAcceptsAnyString_BUG3694(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{}},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{}},
	}
	current := map[string]any{"status": "anything"}

	result := MigrateFields(current, source, target, SameWorkspace)

	if got := result.Fields["status"]; got != "anything" {
		t.Errorf("status = %#v, want \"anything\"", got)
	}
	if slices.Contains(result.Dropped, "status") {
		t.Errorf("status must not be in Dropped, got %#v", result.Dropped)
	}
}

func TestMigrateFields_SelectNotInOptionsStillDropped_BUG3694(t *testing.T) {
	source := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"weird", "open", "done"}},
	}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open"},
	}
	current := map[string]any{"status": "weird"}

	result := MigrateFields(current, source, target, SameWorkspace)

	if !slices.Contains(result.Dropped, "status") {
		t.Errorf("expected status in Dropped, got %#v", result.Dropped)
	}
	if got := result.Fields["status"]; got != "open" {
		t.Errorf("status = %#v, want \"open\" (destination default after drop)", got)
	}
}

func TestMigrateFields_InvalidOptionalDefaultNotInjected_BUG3696(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "multi_select", Options: []string{"open", "done"}, Default: "open"},
	}

	result := MigrateFields(map[string]any{}, source, target, SameWorkspace)

	if v, ok := result.Fields["status"]; ok {
		t.Errorf("invalid optional default was injected: status = %#v", v)
	}
	if !slices.Contains(result.DiscardedDefaults, "status") {
		t.Errorf("expected status in DiscardedDefaults, got %#v", result.DiscardedDefaults)
	}
	if len(result.Errors) != 0 {
		t.Errorf("expected no Errors, got %#v", result.Errors)
	}
}

func TestMigrateFields_InvalidRequiredDefaultErrors_BUG3696(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "multi_select", Options: []string{"open", "done"}, Default: "open", Required: true},
	}

	result := MigrateFields(map[string]any{}, source, target, SameWorkspace)

	if v, ok := result.Fields["status"]; ok {
		t.Errorf("invalid required default was injected: status = %#v", v)
	}
	if !slices.Contains(result.DiscardedDefaults, "status") {
		t.Errorf("expected status in DiscardedDefaults, got %#v", result.DiscardedDefaults)
	}
	wantErr := `required field "status" has no value`
	if !slices.Contains(result.Errors, wantErr) {
		t.Errorf("Errors = %#v, want it to contain %q", result.Errors, wantErr)
	}
}

func TestMigrateFields_ValidDefaultStillInjected_BUG3696(t *testing.T) {
	source := []models.FieldDef{}
	target := []models.FieldDef{
		{Key: "status", Type: "select", Options: []string{"open", "done"}, Default: "open"},
	}

	result := MigrateFields(map[string]any{}, source, target, SameWorkspace)

	if got := result.Fields["status"]; got != "open" {
		t.Errorf("status = %#v, want \"open\"", got)
	}
	if len(result.DiscardedDefaults) != 0 {
		t.Errorf("expected no DiscardedDefaults, got %#v", result.DiscardedDefaults)
	}
	if len(result.Errors) != 0 {
		t.Errorf("expected no Errors, got %#v", result.Errors)
	}
}
