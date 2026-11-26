package server

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestMoveItem_BlankSelectStaysBlank_BUG3694(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", slug, err)
	}

	schema := `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"}]}`
	src := mustSchemaCollection(t, srv, ws.ID, "Select Src", schema)
	dst := mustSchemaCollection(t, srv, ws.ID, "Select Dst", schema)

	item := createItem(t, srv, slug, src.Slug, map[string]interface{}{
		"title":  "Uncategorized",
		"fields": `{"status":""}`,
	})

	// Premise: the source item starts with an explicit blank select.
	var createdFields map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &createdFields); err != nil {
		t.Fatalf("unmarshal created fields: %v (%s)", err, item.Fields)
	}
	if v, ok := createdFields["status"]; !ok || v != "" {
		t.Fatalf("premise failed: created item status = %#v (present=%v), want \"\"", v, ok)
	}

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{"target_collection": dst.Slug})
	if rr.Code != http.StatusOK {
		t.Fatalf("move: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var moved models.Item
	parseJSON(t, rr, &moved)

	var movedFields map[string]any
	if err := json.Unmarshal([]byte(moved.Fields), &movedFields); err != nil {
		t.Fatalf("unmarshal moved fields: %v (%s)", err, moved.Fields)
	}
	got, ok := movedFields["status"]
	if !ok {
		t.Fatalf("moved item fields missing status: %#v", movedFields)
	}
	if got != "" {
		t.Errorf("moved item fields.status = %#v, want \"\"", got)
	}
}

func TestMoveItem_InvalidOptionalDefaultDiscarded_BUG3696(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", slug, err)
	}

	src := mustSchemaCollection(t, srv, ws.ID, "Default Src",
		`{"fields":[{"key":"note","label":"Note","type":"text"}]}`)

	// Target declares an optional multi_select with a stale scalar default "open".
	dstColl := bug3079Collection(t, srv, slug, "Default Dst",
		`{"fields":[{"key":"note","label":"Note","type":"text"},{"key":"status","label":"Status","type":"multi_select","options":["open","done"],"default":"open"}]}`)

	item := createItem(t, srv, slug, src.Slug, map[string]interface{}{
		"title":  "Lacks status",
		"fields": `{"note":"hi"}`,
	})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{"target_collection": dstColl.Slug})
	if rr.Code != http.StatusOK {
		t.Fatalf("move: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var moved models.Item
	parseJSON(t, rr, &moved)

	var movedFields map[string]any
	if err := json.Unmarshal([]byte(moved.Fields), &movedFields); err != nil {
		t.Fatalf("unmarshal moved fields: %v (%s)", err, moved.Fields)
	}
	if v, ok := movedFields["status"]; ok {
		t.Errorf("invalid optional default was stored on moved item: status = %#v", v)
	}
	if moved.Warnings == nil || !slices.Contains(moved.Warnings.DroppedFields, "status") {
		var got []string
		if moved.Warnings != nil {
			got = moved.Warnings.DroppedFields
		}
		t.Errorf("warnings.dropped_fields = %#v, want it to contain \"status\"", got)
	}
}
