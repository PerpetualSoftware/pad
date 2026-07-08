package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// createTaskWithFields is a small helper for the field-patch/OCC tests:
// POST a task with the given fields JSON and return the created item.
func createTaskWithFields(t *testing.T, srv *Server, wsSlug, title, fields string) models.Item {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/tasks/items", map[string]interface{}{
		"title":  title,
		"fields": fields,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var created models.Item
	parseJSON(t, rr, &created)
	return created
}

func decodeItemFields(t *testing.T, fieldsJSON string) map[string]any {
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

// TestPatchItemFieldsPatchMerges is the HTTP-layer regression for the
// field-level merge (IDEA-1480 / TASK-2022): a PATCH carrying `fields_patch`
// changes only the named key and preserves the rest of the blob.
func TestPatchItemFieldsPatchMerges(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open","priority":"high"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "done"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH fields_patch: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	fields := decodeItemFields(t, updated.Fields)
	if fields["status"] != "done" {
		t.Errorf("status: got %v want done", fields["status"])
	}
	if fields["priority"] != "high" {
		t.Errorf("priority clobbered by field-level PATCH: got %v want high", fields["priority"])
	}
}

// TestPatchItemFieldsPatchNullDeletes verifies the JSON-null delete sentinel
// over the wire.
func TestPatchItemFieldsPatchNullDeletes(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open","priority":"high"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"priority": nil},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH fields_patch delete: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	fields := decodeItemFields(t, updated.Fields)
	if _, ok := fields["priority"]; ok {
		t.Errorf("priority should be deleted, got %v", fields["priority"])
	}
	if fields["status"] != "open" {
		t.Errorf("status: got %v want open", fields["status"])
	}
}

// TestPatchItemFieldsAndFieldsPatchMutuallyExclusive: sending both is a 400.
func TestPatchItemFieldsAndFieldsPatchMutuallyExclusive(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields":       `{"status":"done"}`,
		"fields_patch": map[string]interface{}{"priority": "high"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("combining fields + fields_patch: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPatchItemFieldsPatchValidatesAgainstSchema: an out-of-enum select value
// in the patch is rejected with a validation error.
func TestPatchItemFieldsPatchValidatesAgainstSchema(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "not-a-real-status"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid select value in patch: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPatchItemFieldsPatchRejectsDeletingRequiredField: null-deleting a
// required schema field (status on tasks) is a 400 — it would leave a blob
// the full-update validator would reject.
func TestPatchItemFieldsPatchRejectsDeletingRequiredField(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open","priority":"high"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": nil},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("null-delete of required field: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	// The row must be untouched.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, nil)
	var reread models.Item
	parseJSON(t, rr, &reread)
	if got := decodeItemFields(t, reread.Fields)["status"]; got != "open" {
		t.Errorf("rejected patch should be a no-op; status got %v want open", got)
	}
}

// TestPatchItemExpectedUpdatedAtConflict is the HTTP-layer conflict-envelope
// test (TASK-2022): a stale expected_updated_at yields a 409 with the
// pad-structured-error shape (code=update_conflict + details), and the item
// is left unchanged.
func TestPatchItemExpectedUpdatedAtConflict(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"expected_updated_at": "2000-01-01T00:00:00Z",
		"fields_patch":        map[string]interface{}{"status": "done"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("stale expected_updated_at: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				Ref               string `json:"ref"`
				ExpectedUpdatedAt string `json:"expected_updated_at"`
				ActualUpdatedAt   string `json:"actual_updated_at"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode conflict envelope: %v", err)
	}
	if envelope.Error.Code != "update_conflict" {
		t.Errorf("error.code: got %q want update_conflict", envelope.Error.Code)
	}
	if envelope.Error.Details.ExpectedUpdatedAt != "2000-01-01T00:00:00Z" {
		t.Errorf("details.expected_updated_at: got %q", envelope.Error.Details.ExpectedUpdatedAt)
	}
	if envelope.Error.Details.ActualUpdatedAt == "" {
		t.Error("details.actual_updated_at should be populated")
	}

	// Item must be unchanged.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, nil)
	var reread models.Item
	parseJSON(t, rr, &reread)
	if got := decodeItemFields(t, reread.Fields)["status"]; got != "open" {
		t.Errorf("conflicting PATCH should be a no-op; status got %v want open", got)
	}
}

// TestPatchItemExpectedUpdatedAtMatchSucceeds: round-tripping the item's real
// updated_at lets the update through.
func TestPatchItemExpectedUpdatedAtMatchSucceeds(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	expected := item.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"expected_updated_at": expected,
		"fields_patch":        map[string]interface{}{"status": "done"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("matching expected_updated_at: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPatchItemExpectedUpdatedAtMalformed: a non-RFC3339 token is a 400, not a
// 500 or a silent pass.
func TestPatchItemExpectedUpdatedAtMalformed(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"expected_updated_at": "not-a-timestamp",
		"fields_patch":        map[string]interface{}{"status": "done"},
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed expected_updated_at: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPatchItemFieldsPatchTriggersOpenChildrenGuard confirms the open-children
// guard fires on the field-level PATCH path too (not just the full-`fields`
// path): marking a parent terminal via fields_patch while it has a
// non-terminal child is rejected with the structured open_children 409.
func TestPatchItemFieldsPatchTriggersOpenChildrenGuard(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open", "done"})

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("fields_patch terminal transition with open child: expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "open_children" {
		t.Errorf("error.code: got %q want open_children", resp.Error.Code)
	}

	// --force overrides on the patch path.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "completed"},
		"force":        true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("fields_patch + force: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestPatchItemFieldsPatchPreservesExistingDate: a status→completed patch must
// NOT clobber an end_date the caller isn't touching (Codex round 2). The
// auto-populate only fills a date when the current value is empty.
func TestPatchItemFieldsPatchPreservesExistingDate(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
		"title":  "Plan",
		"fields": `{"status":"active","end_date":"2020-01-01"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create plan: %d: %s", rr.Code, rr.Body.String())
	}
	var plan models.Item
	parseJSON(t, rr, &plan)

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch status→completed: %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	fields := decodeItemFields(t, updated.Fields)
	if fields["end_date"] != "2020-01-01" {
		t.Errorf("existing end_date clobbered by auto-populate: got %v want 2020-01-01", fields["end_date"])
	}
	if fields["status"] != "completed" {
		t.Errorf("status: got %v want completed", fields["status"])
	}
}

// TestPatchItemFieldsPatchAutoFillsEmptyDate: when end_date is empty, a
// status→completed patch DOES auto-fill it (the feature still works).
func TestPatchItemFieldsPatchAutoFillsEmptyDate(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
		"title":  "Plan",
		"fields": `{"status":"planned"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create plan: %d: %s", rr.Code, rr.Body.String())
	}
	var plan models.Item
	parseJSON(t, rr, &plan)

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	fields := decodeItemFields(t, updated.Fields)
	if ed, _ := fields["end_date"].(string); ed == "" {
		t.Errorf("empty end_date should have been auto-filled on completion; got %v", fields["end_date"])
	}
}

// TestPatchItemConflictWinsOverOpenChildrenGuard: when a stale
// expected_updated_at AND a guarded terminal transition both apply, the
// optimistic-concurrency conflict must win (the caller was operating on a
// stale view), not the open_children guard (Codex round 2 ordering fix).
func TestPatchItemConflictWinsOverOpenChildrenGuard(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"expected_updated_at": "2000-01-01T00:00:00Z", // stale
		"fields_patch":        map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Error.Code != "update_conflict" {
		t.Errorf("error.code: got %q want update_conflict (conflict must win over open_children)", resp.Error.Code)
	}
}

// TestListItemVersionsHTTP exercises the read-only history endpoint the CLI
// `pad item history` and the MCP `pad_item.history` action consume: after a
// content change, the version list carries at least one row.
func TestListItemVersionsHTTP(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Item", `{"status":"open"}`)

	// Change content so a version row is recorded (source differs from the
	// create-time attribution, bypassing the per-(actor,source) throttle).
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug, map[string]interface{}{
		"content": "new body",
		"source":  "cli",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH content: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list versions: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var versions []models.Version
	if err := json.Unmarshal(rr.Body.Bytes(), &versions); err != nil {
		t.Fatalf("decode versions: %v", err)
	}
	if len(versions) == 0 {
		t.Fatal("expected at least one version row after a content change")
	}
	for _, v := range versions {
		if v.CreatedAt.IsZero() {
			t.Error("version row missing created_at")
		}
	}
}
