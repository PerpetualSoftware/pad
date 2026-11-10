package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A soft-deleted blocker is a row of the bundle but not an item of the
// workspace, so the post-import check compares live items on both sides.
func TestTASK4423_R10ImportSummaryCountsLiveItems(t *testing.T) {
	t.Parallel()
	const ts = "2026-10-15T00:00:00Z"
	item := func(id, title, deleted string, n int) models.ItemExport {
		return models.ItemExport{ID: id, CollectionID: "c", Title: title, Slug: title, Fields: "{}", Tags: "[]", ItemNumber: n, CreatedAt: ts, UpdatedAt: ts, DeletedAt: deleted}
	}
	raw, err := json.Marshal(models.WorkspaceExport{
		Version:   models.WorkspaceExportVersion,
		Workspace: models.WorkspaceExportMeta{Name: "Summary", Slug: "summary", Settings: "{}"},
		Collections: []models.CollectionExport{{
			ID: "c", Name: "Tasks", Slug: "tasks", Prefix: "TASK", Settings: "{}", CreatedAt: ts, UpdatedAt: ts, Schema: `{"fields":[]}`,
		}},
		Items:         []models.ItemExport{item("a", "One", "", 1), item("b", "Two", "", 2), item("d", "Gone", "2026-10-16T00:00:00Z", 3)},
		ScheduleGates: []models.ScheduleGateExport{{ID: "g", BlockedItemID: "b", BlockerItemID: "d", CreatedAt: ts}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := testServerWithAttachments(t)
	rr := postBundle(srv, "SummaryBundle", gzipTar(t, []bundleEntry{{"pad-export.json", raw}}))
	if rr.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Summary struct {
			InBundle struct {
				Items int `json:"items"`
			} `json:"in_bundle"`
			Imported struct {
				Items int `json:"items"`
			} `json:"imported"`
		} `json:"import_summary"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Summary.InBundle.Items != 2 || body.Summary.Imported.Items != 2 {
		t.Fatalf("summary: in_bundle %d, imported %d, want 2 and 2 (%s)", body.Summary.InBundle.Items, body.Summary.Imported.Items, rr.Body.String())
	}
}
