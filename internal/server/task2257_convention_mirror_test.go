package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2257: editing a convention's ordinary metadata keys (what agents obey)
// keeps item.convention (what the web reads) in step. Measured before the fix:
// `fields_patch {trigger: on-commit}` answered 200 and item.convention still
// read "always".
func TestTASK2257_FieldsPatchKeepsTheConventionCopyInStep(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/conventions/items", map[string]any{
		"title":      "Mirror rule",
		"fields":     `{"status":"active","trigger":"always","scope":"all","priority":"should","enforcement":"should"}`,
		"convention": map[string]any{"trigger": "always", "surfaces": []string{"all"}, "enforcement": "should"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)

	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+it.Slug, map[string]any{
		"fields_patch": map[string]any{"trigger": "on-commit", "priority": "must", "enforcement": "must"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("patch: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+it.Slug, nil)
	var got models.Item
	parseJSON(t, rr, &got)
	if got.Convention == nil || got.Convention.Trigger != "on-commit" || got.Convention.Enforcement != "must" {
		t.Fatalf("item.convention = %+v, want trigger on-commit, enforcement must", got.Convention)
	}
	if len(got.Convention.Surfaces) != 1 || got.Convention.Surfaces[0] != "all" {
		t.Fatalf("untouched surfaces changed: %v", got.Convention.Surfaces)
	}
}

// A full `fields` write that carries the stored reserved copy unchanged
// (BUG-3163's rule) and changes `trigger` passes the carry guard, and the copy
// follows.
func TestTASK2257_FullFieldsCarryStillPassesAndTheCopyFollows(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/conventions/items", map[string]any{
		"title":      "Full carry",
		"fields":     `{"status":"active","trigger":"always","scope":"all","priority":"should","enforcement":"should"}`,
		"convention": map[string]any{"trigger": "always", "surfaces": []string{"all"}, "enforcement": "should"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)

	full := `{"status":"active","trigger":"on-implement","scope":"all","priority":"should","enforcement":"should","convention":{"enforcement":"should","surfaces":["all"],"trigger":"always"}}`
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+ws+"/items/"+it.Slug, map[string]any{"fields": full})
	if rr.Code != http.StatusOK {
		t.Fatalf("full fields carrying the reserved copy: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items/"+it.Slug, nil)
	var got models.Item
	parseJSON(t, rr, &got)
	if got.Convention == nil || got.Convention.Trigger != "on-implement" {
		t.Fatalf("item.convention = %+v, want trigger on-implement", got.Convention)
	}
}
