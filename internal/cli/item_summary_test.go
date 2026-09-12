package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }

// TestToItemSummary_DropsContentAndUUIDs verifies the default summary
// projection replaces the rich body with a preview and drops the UUID
// plumbing / duplicate join fields. TASK-2000.
func TestToItemSummary_DropsContentAndUUIDs(t *testing.T) {
	item := models.Item{
		ID:               "uuid-1",
		WorkspaceID:      "ws-uuid",
		CollectionID:     "coll-uuid",
		Title:            "Fix the widget",
		Slug:             "fix-the-widget",
		Ref:              "TASK-5",
		Content:          "# Heading\n\nThe body has real detail that costs tokens.",
		Fields:           `{"status":"open","priority":"high"}`,
		Tags:             `["bug","ui"]`,
		CollectionSlug:   "tasks",
		CollectionName:   "Tasks",
		ItemNumber:       intPtr(5),
		AssignedUserID:   strPtr("user-uuid"),
		AssignedUserName: "Dave",
		AgentRoleSlug:    "implementer",
		ParentRef:        "PLAN-2",
		ParentTitle:      "Big plan",
	}

	sum := ToItemSummary(item)

	// Marshal to JSON and confirm the heavy/duplicate fields are gone.
	b, err := json.Marshal(sum)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)

	for _, banned := range []string{"uuid-1", "ws-uuid", "coll-uuid", "user-uuid", `"content"`, "real detail", "Big plan", "\"id\""} {
		if strings.Contains(js, banned) {
			t.Errorf("summary JSON should not contain %q; got %s", banned, js)
		}
	}
	if sum.Ref != "TASK-5" || sum.Title != "Fix the widget" || sum.CollectionSlug != "tasks" {
		t.Errorf("summary dropped a field it should keep: %+v", sum)
	}
	if sum.ContentPreview == "" || strings.Contains(sum.ContentPreview, "#") {
		t.Errorf("content_preview should be a heading-stripped snippet, got %q", sum.ContentPreview)
	}
	if sum.AssignedUser != "Dave" || sum.AgentRole != "implementer" || sum.ParentRef != "PLAN-2" {
		t.Errorf("summary dropped a human-readable field: %+v", sum)
	}
	// fields/tags should be nested JSON, not escaped strings.
	if !strings.Contains(js, `"fields":{"status":"open"`) {
		t.Errorf("fields should be a nested object, got %s", js)
	}
	if !strings.Contains(js, `"tags":["bug","ui"]`) {
		t.Errorf("tags should be a nested array, got %s", js)
	}
}

func TestToItemAgentView_KeepsWorkContextAndDropsStoragePlumbing(t *testing.T) {
	item := models.Item{
		ID:             "item-uuid",
		WorkspaceID:    "workspace-uuid",
		CollectionID:   "collection-uuid",
		Title:          "Implement the compact reader",
		Slug:           "implement-compact-reader",
		Ref:            "TASK-9",
		Content:        "The complete body an agent needs.",
		Fields:         `{"status":"in-progress","estimate":3,"implementation_notes":[{"summary":"Preserve this execution context"}],"decision_log":[{"decision":"Use an additive projection"}]}`,
		Tags:           `["agents"]`,
		CollectionSlug: "tasks",
		AssignedUserID: strPtr("user-uuid"),
		ImplementationNotes: []models.ItemImplementationNote{
			{Summary: "Preserve this execution context"},
		},
		DecisionLog: []models.ItemDecisionLogEntry{
			{Decision: "Use an additive projection"},
		},
	}

	b, err := json.Marshal(ToItemAgentView(item))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, want := range []string{
		`"ref":"TASK-9"`,
		`"content":"The complete body an agent needs."`,
		`"estimate":3`,
		`"status":"in-progress"`,
		`"implementation_notes"`,
		`"decision_log"`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("agent view should contain %q; got %s", want, js)
		}
	}
	for _, banned := range []string{
		"item-uuid", "workspace-uuid", "collection-uuid", "user-uuid",
		`"workspace_id"`, `"collection_id"`, `"content_preview"`,
	} {
		if strings.Contains(js, banned) {
			t.Errorf("agent view should not contain %q; got %s", banned, js)
		}
	}
	if strings.Count(js, "Preserve this execution context") != 1 || strings.Count(js, "Use an additive projection") != 1 {
		t.Errorf("hydrated notes and decisions should appear once, got %s", js)
	}
}

// TestContentPreview_TruncatesAndStrips checks the preview helper.
func TestContentPreview_TruncatesAndStrips(t *testing.T) {
	if got := contentPreview(""); got != "" {
		t.Errorf("empty content should yield empty preview, got %q", got)
	}
	if got := contentPreview("\n\n## Title line\nmore"); got != "Title line" {
		t.Errorf("preview should take first non-blank line, heading-stripped, got %q", got)
	}
	long := strings.Repeat("a", contentPreviewLimit+50)
	got := contentPreview(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("over-long preview should be ellipsized, got %q", got)
	}
	if len([]rune(got)) > contentPreviewLimit+1 {
		t.Errorf("preview should be capped near %d runes, got %d", contentPreviewLimit, len([]rune(got)))
	}
}

// TestRawJSONOrNil_OmitsEmpty confirms empty containers are dropped so
// the summary stays lean and never emits invalid RawMessage("").
func TestRawJSONOrNil_OmitsEmpty(t *testing.T) {
	for _, empty := range []string{"", "{}", "[]", "null", "  "} {
		if got := rawJSONOrNil(empty); got != nil {
			t.Errorf("rawJSONOrNil(%q) = %s, want nil", empty, got)
		}
	}
	if got := rawJSONOrNil(`{"a":1}`); string(got) != `{"a":1}` {
		t.Errorf("rawJSONOrNil should pass through real JSON, got %s", got)
	}
	// Malformed stored value must not break marshal — falls back to a
	// valid JSON string rather than an invalid RawMessage.
	got := rawJSONOrNil(`{not valid`)
	if got == nil {
		t.Fatal("malformed value should fall back, not drop")
	}
	if !json.Valid(got) {
		t.Errorf("fallback must be valid JSON, got %s", got)
	}
	if string(got) != `"{not valid"` {
		t.Errorf("malformed value should be emitted as a JSON string, got %s", got)
	}
}

// TestToItemSummaries_EmptyIsNonNil ensures a nil input encodes as `[]`.
func TestToItemSummaries_EmptyIsNonNil(t *testing.T) {
	out := ToItemSummaries(nil)
	if out == nil {
		t.Fatal("ToItemSummaries(nil) should be non-nil")
	}
	b, _ := json.Marshal(out)
	if string(b) != "[]" {
		t.Errorf("empty summaries should encode as [], got %s", b)
	}
}
