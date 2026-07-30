package items

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
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

func TestValidateFields_JSONType(t *testing.T) {
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{
			{Key: "arguments", Label: "Arguments", Type: "json"},
		},
	}

	cases := []struct {
		name    string
		val     any
		wantErr bool
	}{
		{"array", []any{"a", "b"}, false},
		{"object", map[string]any{"k": "v"}, false},
		{"nil", nil, false}, // optional + nil is allowed
		// Scalars are rejected: a generic web text input would corrupt a
		// structured field by emitting strings like `"[]"` instead of
		// arrays. Use "text" / "number" / "checkbox" for scalars.
		{"string-rejected", "hello", true},
		{"number-rejected", float64(42), true},
		{"bool-rejected", true, true},
		{"struct-not-decoded", struct{ X int }{1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"arguments": tc.val}
			err := ValidateFields(fields, schema)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error, got: %v", err)
			}
		})
	}
}

func TestValidateFields_PatternMatch(t *testing.T) {
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{
			{
				Key:     "invocation_slug",
				Label:   "Invocation slug",
				Type:    "text",
				Pattern: `^[a-z0-9][a-z0-9-]*[a-z0-9]$`,
			},
		},
	}

	cases := []struct {
		name    string
		val     string
		wantErr bool
	}{
		{"valid-kebab", "ship", false},
		{"valid-with-digits", "ship-blog-2", false},
		{"valid-min-two-chars", "ab", false},
		{"empty-allowed", "", false},
		{"single-char-rejected", "a", true},
		{"uppercase", "Ship", true},
		{"underscore", "ship_blog", true},
		{"leading-dash", "-ship", true},
		{"trailing-dash", "ship-", true},
		{"space", "ship blog", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]any{"invocation_slug": tc.val}
			err := ValidateFields(fields, schema)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.val)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected no error for %q, got: %v", tc.val, err)
			}
		})
	}
}

func TestValidateFields_InvalidPattern(t *testing.T) {
	schema := models.CollectionSchema{
		Fields: []models.FieldDef{
			{
				Key:     "field",
				Label:   "Field",
				Type:    "text",
				Pattern: `[unclosed`,
			},
		},
	}
	fields := map[string]any{"field": "value"}
	err := ValidateFields(fields, schema)
	if err == nil {
		t.Fatal("expected error for invalid schema pattern")
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

// --- ValidatePartialFields (TASK-2022 field-level PATCH) ---

func TestValidatePartialFields_ValidatesOnlyPresentKeys(t *testing.T) {
	schema := taskSchema()
	// Patch touches only priority; status (required) is absent and must NOT
	// be flagged missing, and no defaults should be injected.
	patch := map[string]any{"priority": "high"}
	if err := ValidatePartialFields(patch, schema); err != nil {
		t.Fatalf("expected no error validating a partial patch, got: %v", err)
	}
	if _, injected := patch["status"]; injected {
		t.Errorf("ValidatePartialFields must NOT inject defaults for absent keys; got %v", patch)
	}
}

func TestValidatePartialFields_RejectsBadEnum(t *testing.T) {
	schema := taskSchema()
	patch := map[string]any{"status": "not-a-status"}
	if err := ValidatePartialFields(patch, schema); err == nil {
		t.Fatal("expected an error for an out-of-enum select value in the patch")
	}
}

func TestValidatePartialFields_AllowsOrphanKeys(t *testing.T) {
	schema := taskSchema()
	patch := map[string]any{"pad_source_url": "https://example.com"}
	if err := ValidatePartialFields(patch, schema); err != nil {
		t.Fatalf("orphan (non-schema) keys should be allowed, got: %v", err)
	}
}

func TestValidatePartialFields_RejectsDeletingRequiredField(t *testing.T) {
	schema := taskSchema()
	// status is required — a null-delete of it would leave an invalid blob.
	patch := map[string]any{"status": nil}
	if err := ValidatePartialFields(patch, schema); err == nil {
		t.Fatal("expected an error deleting a required field via null")
	}
}

func TestValidatePartialFields_AllowsDeletingOptionalField(t *testing.T) {
	schema := taskSchema()
	// priority is optional — null-delete is fine.
	patch := map[string]any{"priority": nil}
	if err := ValidatePartialFields(patch, schema); err != nil {
		t.Fatalf("deleting an optional field via null should be allowed, got: %v", err)
	}
}

// --- ValidateFieldsDetailed (PLAN-2357 / TASK-2364) --------------------

// TestValidateFieldsDetailed_AttributesAndOrders pins the two properties
// the copy preflight depends on: every failure is attributable to a KEY
// with a kind, and the order is schema order (not map order), so a caller
// that is specified to return identical results on repeated calls can.
func TestValidateFieldsDetailed_AttributesAndOrders(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "alpha", Type: "text", Required: true},
		{Key: "beta", Type: "select", Options: []string{"x", "y"}},
		{Key: "gamma", Type: "text", Required: true},
		{Key: "delta", Type: "text", Default: "d"},
	}}

	var last []FieldIssue
	for i := 0; i < 20; i++ {
		fields := map[string]any{"beta": "nope"}
		issues := ValidateFieldsDetailed(fields, schema)
		if len(issues) != 3 {
			t.Fatalf("got %d issues, want 3: %+v", len(issues), issues)
		}
		want := []struct {
			key  string
			kind FieldIssueKind
		}{
			{"alpha", IssueRequired},
			{"beta", IssueInvalid},
			{"gamma", IssueRequired},
		}
		for j, w := range want {
			if issues[j].Key != w.key || issues[j].Kind != w.kind {
				t.Fatalf("issue %d = %+v, want key=%s kind=%s", j, issues[j], w.key, w.kind)
			}
			if issues[j].Message == "" {
				t.Errorf("issue %d carries no message", j)
			}
		}
		// Defaults are still applied in place, exactly as ValidateFields does.
		if fields["delta"] != "d" {
			t.Errorf("default not applied: %+v", fields)
		}
		if last != nil && !sameIssues(last, issues) {
			t.Fatalf("run %d differed from the previous run: %+v vs %+v", i, last, issues)
		}
		last = issues
	}
}

// TestValidateFields_DelegatesToDetailed — the joined-error surface must
// stay in step with the structured one; they share a traversal so they can
// never disagree about validity.
func TestValidateFields_DelegatesToDetailed(t *testing.T) {
	schema := models.CollectionSchema{Fields: []models.FieldDef{
		{Key: "alpha", Type: "text", Required: true},
	}}

	if err := ValidateFields(map[string]any{"alpha": "ok"}, schema); err != nil {
		t.Fatalf("valid map reported an error: %v", err)
	}
	err := ValidateFields(map[string]any{}, schema)
	if err == nil {
		t.Fatal("missing required field did not error")
	}
	issues := ValidateFieldsDetailed(map[string]any{}, schema)
	if len(issues) != 1 || issues[0].Kind != IssueRequired {
		t.Fatalf("detailed issues = %+v", issues)
	}
	if !strings.Contains(err.Error(), issues[0].Message) {
		t.Errorf("joined error %q does not contain the detailed message %q", err, issues[0].Message)
	}
}

func sameIssues(a, b []FieldIssue) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
