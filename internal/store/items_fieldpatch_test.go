package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// decodeFields is a small test helper: parse an item's fields JSON into a map.
func decodeFields(t *testing.T, fieldsJSON string) map[string]any {
	t.Helper()
	m := map[string]any{}
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return m
	}
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		t.Fatalf("decode fields %q: %v", fieldsJSON, err)
	}
	return m
}

// TestMergeFieldsPatch exercises the pure shallow-merge helper directly:
// set, delete-on-null, orphan-key preservation, and empty-base handling.
func TestMergeFieldsPatch(t *testing.T) {
	cases := []struct {
		name    string
		current string
		patch   map[string]any
		want    map[string]any
	}{
		{
			name:    "set one key preserves the rest",
			current: `{"status":"open","priority":"high"}`,
			patch:   map[string]any{"status": "done"},
			want:    map[string]any{"status": "done", "priority": "high"},
		},
		{
			name:    "null deletes a key",
			current: `{"status":"open","priority":"high"}`,
			patch:   map[string]any{"priority": nil},
			want:    map[string]any{"status": "open"},
		},
		{
			name:    "add a new orphan key",
			current: `{"status":"open"}`,
			patch:   map[string]any{"custom": "x"},
			want:    map[string]any{"status": "open", "custom": "x"},
		},
		{
			name:    "empty base",
			current: "",
			patch:   map[string]any{"status": "open"},
			want:    map[string]any{"status": "open"},
		},
		{
			name:    "empty-object base",
			current: "{}",
			patch:   map[string]any{"status": "open"},
			want:    map[string]any{"status": "open"},
		},
		{
			name:    "delete a key that isn't there is a no-op",
			current: `{"status":"open"}`,
			patch:   map[string]any{"missing": nil},
			want:    map[string]any{"status": "open"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mergeFieldsPatch(tc.current, tc.patch)
			if err != nil {
				t.Fatalf("mergeFieldsPatch: %v", err)
			}
			var gotMap map[string]any
			if err := json.Unmarshal([]byte(got), &gotMap); err != nil {
				t.Fatalf("merged JSON invalid: %v (%q)", err, got)
			}
			if len(gotMap) != len(tc.want) {
				t.Fatalf("key count: got %v want %v", gotMap, tc.want)
			}
			for k, v := range tc.want {
				if gotMap[k] != v {
					t.Errorf("key %q: got %v want %v", k, gotMap[k], v)
				}
			}
		})
	}
}

// TestUpdateItemFieldsPatchMergesNotReplaces is the core race-fix regression
// (IDEA-1480 / TASK-2022): a FieldsPatch update touches ONE key and leaves the
// rest of the fields blob intact, unlike a full `fields` write which replaces
// everything.
func TestUpdateItemFieldsPatchMergesNotReplaces(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "FieldPatch")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Item",
		Fields: `{"status":"open","priority":"high"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	// Patch only status; priority must survive.
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{
		FieldsPatch: map[string]any{"status": "done"},
	})
	if err != nil {
		t.Fatalf("UpdateItem (patch): %v", err)
	}
	fields := decodeFields(t, updated.Fields)
	if fields["status"] != "done" {
		t.Errorf("status: got %v want done", fields["status"])
	}
	if fields["priority"] != "high" {
		t.Errorf("priority clobbered: got %v want high (the whole point of field-level merge)", fields["priority"])
	}
}

// TestUpdateItemFieldsPatchNullDeletes verifies the JSON-null delete sentinel
// removes a single key while preserving the others.
func TestUpdateItemFieldsPatchNullDeletes(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "FieldPatchDelete")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Item",
		Fields: `{"status":"open","priority":"high"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{
		FieldsPatch: map[string]any{"priority": nil},
	})
	if err != nil {
		t.Fatalf("UpdateItem (patch delete): %v", err)
	}
	fields := decodeFields(t, updated.Fields)
	if _, ok := fields["priority"]; ok {
		t.Errorf("priority should have been deleted, got %v", fields["priority"])
	}
	if fields["status"] != "open" {
		t.Errorf("status: got %v want open", fields["status"])
	}
}

// TestUpdateItemFieldsPatchSequentialNoClobber simulates the concurrent
// single-field writes IDEA-1480 describes, but sequentially against the store:
// two independent patches (status, then a custom meta key) must BOTH survive.
// Before field-level merge, the second full-blob write would have overwritten
// the first if it had been built from a stale read.
func TestUpdateItemFieldsPatchSequentialNoClobber(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "FieldPatchSeq")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{
		Title:  "Item",
		Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{
		FieldsPatch: map[string]any{"status": "done"},
	}); err != nil {
		t.Fatalf("UpdateItem patch 1: %v", err)
	}
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{
		FieldsPatch: map[string]any{"pad_source_url": "https://example.com"},
	})
	if err != nil {
		t.Fatalf("UpdateItem patch 2: %v", err)
	}
	fields := decodeFields(t, updated.Fields)
	if fields["status"] != "done" {
		t.Errorf("first patch lost: status got %v want done", fields["status"])
	}
	if fields["pad_source_url"] != "https://example.com" {
		t.Errorf("second patch lost: pad_source_url got %v", fields["pad_source_url"])
	}
}

// TestUpdateItemExpectedUpdatedAtMatch: a matching optimistic-concurrency
// token lets the update through.
func TestUpdateItemExpectedUpdatedAtMatch(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCMatch")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Item", "body")

	expected := item.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	patch := map[string]any{"status": "done"}
	updated, err := s.UpdateItem(item.ID, models.ItemUpdate{
		ExpectedUpdatedAt: expected,
		FieldsPatch:       patch,
	})
	if err != nil {
		t.Fatalf("UpdateItem with matching expected_updated_at should succeed: %v", err)
	}
	if got := decodeFields(t, updated.Fields)["status"]; got != "done" {
		t.Errorf("status: got %v want done", got)
	}
}

// TestUpdateItemExpectedUpdatedAtConflict: a stale token is rejected with
// *UpdateConflictError, and the item is left unchanged.
func TestUpdateItemExpectedUpdatedAtConflict(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OCCConflict")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Item", "body")

	// A timestamp that definitely doesn't match the row's updated_at.
	stale := "2000-01-01T00:00:00Z"
	_, err := s.UpdateItem(item.ID, models.ItemUpdate{
		ExpectedUpdatedAt: stale,
		FieldsPatch:       map[string]any{"status": "done"},
	})
	if err == nil {
		t.Fatal("expected UpdateConflictError, got nil")
	}
	var conflict *UpdateConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *UpdateConflictError, got %T: %v", err, err)
	}
	if conflict.ExpectedUpdatedAt != stale {
		t.Errorf("conflict.ExpectedUpdatedAt: got %q want %q", conflict.ExpectedUpdatedAt, stale)
	}
	if conflict.ActualUpdatedAt.IsZero() {
		t.Error("conflict.ActualUpdatedAt should carry the row's real timestamp")
	}

	// The write must NOT have landed.
	reread, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if got := decodeFields(t, reread.Fields)["status"]; got != "open" {
		t.Errorf("conflicting update should be a no-op; status got %v want open", got)
	}
}
