package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2683, the case it was filed for: a bulk move to a collection with no
// home for an ORDINARY schema field records that field in the move's activity
// row, as the single move does (BUG-2674). The report was threaded out of
// bulkMoveCollection by TASK-2878 (#1246); its existing pin,
// TestRelationDoors_BulkMoveReportsDroppedFields, exercises only a RELATION
// drop, so the plain-field case this bug names was unpinned.
func TestBUG2683_BulkMoveRecordsAnOrdinaryDroppedField(t *testing.T) {
	f := newDoorFixture(t)
	src := mustSchemaCollection(t, f.srv, f.ws.ID, "Estimated", `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]},
		{"key":"estimate","label":"Estimate","type":"text"}
	]}`)
	dst := mustSchemaCollection(t, f.srv, f.ws.ID, "Unestimated", `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["open","done"]}
	]}`)
	item, err := f.srv.store.CreateItem(f.ws.ID, src.ID, models.ItemCreate{
		Title: "Sized", Fields: `{"status":"open","estimate":"3d"}`, CreatedBy: f.owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}

	rr := f.call(f.srv.handleBulkItems, "POST",
		"/api/v1/workspaces/"+f.ws.Slug+"/items/bulk", nil,
		map[string]any{"op": "move", "ids": []string{item.ID}, "collection": dst.Slug})
	if rr.Code != http.StatusOK {
		t.Fatalf("bulk move: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	acts, err := f.srv.store.ListDocumentActivity(item.ID, models.ActivityListParams{Limit: 20})
	if err != nil {
		t.Fatalf("ListDocumentActivity: %v", err)
	}
	var moved *models.Activity
	for i := range acts {
		if acts[i].Action == "moved" {
			moved = &acts[i]
			break
		}
	}
	if moved == nil {
		t.Fatalf("no `moved` activity row for a bulk collection move: %+v", acts)
	}
	if !strings.Contains(moved.Metadata, `"dropped_fields":"estimate"`) {
		t.Fatalf("the bulk move discarded `estimate` and must name exactly it in dropped_fields; metadata was %s", moved.Metadata)
	}
}
