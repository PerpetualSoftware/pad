package items

import (
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func taskSchema() models.CollectionSchema {
	return models.CollectionSchema{
		Fields: []models.FieldDef{
			{
				Key:      "status",
				Label:    "Status",
				Type:     "select",
				Options:  []string{"open", "in-progress", "done", "cancelled"},
				Default:  "open",
				Required: true,
			},
			{
				Key:     "priority",
				Label:   "Priority",
				Type:    "select",
				Options: []string{"low", "medium", "high", "critical"},
				Default: "medium",
			},
			{
				Key:   "assignee",
				Label: "Assignee",
				Type:  "text",
			},
			{
				Key:   "due_date",
				Label: "Due Date",
				Type:  "date",
			},
			{
				Key:   "effort_hours",
				Label: "Effort",
				Type:  "number",
			},
			{
				Key:   "done",
				Label: "Done",
				Type:  "checkbox",
			},
		},
	}
}

func TestValidateFields_RequiredWithDefault(t *testing.T) {
	schema := taskSchema()
	fields := map[string]any{}

	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Required field "status" should have been filled with default
	if fields["status"] != "open" {
		t.Errorf("expected status default 'open', got %v", fields["status"])
	}
	// Optional field "priority" should have been filled with default
	if fields["priority"] != "medium" {
		t.Errorf("expected priority default 'medium', got %v", fields["priority"])
	}
}

func TestValidateFields_RequiredMissingNoDefault(t *testing.T) {
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{
			{Key: "name", Label: "Name", Type: "text", Required: true},
		},
	}
	fields := map[string]any{}

	err := ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for missing required field without default")
	}
}

func TestValidateFields_SelectInvalid(t *testing.T) {
	schema := taskSchema()
	fields := map[string]any{
		"status": "invalid-value",
	}

	err := ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for invalid select value")
	}
}

func TestValidateFields_SelectValid(t *testing.T) {
	schema := taskSchema()
	fields := map[string]any{
		"status":   "done",
		"priority": "high",
	}

	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
}

func TestValidateFields_NumberType(t *testing.T) {
	schema := taskSchema()

	// Valid number
	fields := map[string]any{
		"effort_hours": float64(5),
	}
	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error for valid number, got: %v", err)
	}

	// Invalid number
	fields = map[string]any{
		"effort_hours": "not-a-number",
	}
	err = ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for string in number field")
	}
}

func TestValidateFields_CheckboxType(t *testing.T) {
	schema := taskSchema()

	// Valid boolean
	fields := map[string]any{
		"done": true,
	}
	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error for valid checkbox, got: %v", err)
	}

	// Invalid boolean
	fields = map[string]any{
		"done": "yes",
	}
	err = ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for string in checkbox field")
	}
}

func TestValidateFields_DateType(t *testing.T) {
	schema := taskSchema()

	// Valid date
	fields := map[string]any{
		"due_date": "2026-03-25",
	}
	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error for valid date, got: %v", err)
	}

	// Valid RFC3339
	fields = map[string]any{
		"due_date": "2026-03-25T10:00:00Z",
	}
	err = ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error for valid RFC3339, got: %v", err)
	}

	// Invalid date
	fields = map[string]any{
		"due_date": "not-a-date",
	}
	err = ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for invalid date")
	}

	// Empty date is OK (optional)
	fields = map[string]any{
		"due_date": "",
	}
	err = ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error for empty date, got: %v", err)
	}
}

func TestValidateFields_TextType(t *testing.T) {
	schema := taskSchema()

	// Valid
	fields := map[string]any{
		"assignee": "alice",
	}
	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Invalid
	fields = map[string]any{
		"assignee": 42,
	}
	err = ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for number in text field")
	}
}

func TestValidateFields_MultiSelect(t *testing.T) {
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{
			{
				Key:     "labels",
				Label:   "Labels",
				Type:    "multi_select",
				Options: []string{"bug", "feature", "docs"},
			},
		},
	}

	// Valid
	fields := map[string]any{
		"labels": []any{"bug", "feature"},
	}
	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Invalid option
	fields = map[string]any{
		"labels": []any{"bug", "invalid"},
	}
	err = ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for invalid multi_select option")
	}
}

func TestValidateFields_DefaultsApplied(t *testing.T) {
	schema := taskSchema()
	fields := map[string]any{
		"assignee": "bob",
	}

	err := ValidateFields(fields, schema)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Defaults should be applied
	if fields["status"] != "open" {
		t.Errorf("expected status default, got %v", fields["status"])
	}
	if fields["priority"] != "medium" {
		t.Errorf("expected priority default, got %v", fields["priority"])
	}
	// Explicitly set field should remain
	if fields["assignee"] != "bob" {
		t.Errorf("expected assignee 'bob', got %v", fields["assignee"])
	}
}
