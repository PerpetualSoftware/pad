package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The DR-12 fix on the EXISTING intra-workspace move path (PLAN-2357).
//
// handleMoveItem merged field_overrides into result.Fields and then tested
// result.Errors — a value MigrateFields computes BEFORE any override
// exists. So an override that satisfied a required destination field still
// 400'd, and an override with an invalid value was never type-checked and
// went straight into the item.
//
// The bug was dormant because the web UI's handleMove passes
// field_overrides: undefined. PLAN-2357's copy dialog is the first caller
// to send them, which is what puts the fix in this PR rather than a
// follow-up.

func moveTestCollections(t *testing.T, srv *Server) (string, *models.Collection, *models.Collection) {
	t.Helper()
	slug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", slug, err)
	}
	src := mustSchemaCollection(t, srv, ws.ID, "Move Src", `{"fields":[
		{"key":"note","label":"Note","type":"text"}
	]}`)
	dst := mustSchemaCollection(t, srv, ws.ID, "Move Dst", `{"fields":[
		{"key":"note","label":"Note","type":"text"},
		{"key":"ticket","label":"Ticket","type":"text","required":true},
		{"key":"priority","label":"Priority","type":"select","options":["low","high"]}
	]}`)
	return slug, src, dst
}

// TestMoveItem_OverrideSatisfiesRequiredField — the first half of DR-12.
// Pre-fix this returned 400 missing_required_fields even though `ticket`
// was supplied.
func TestMoveItem_OverrideSatisfiesRequiredField(t *testing.T) {
	srv := testServer(t)
	slug, src, dst := moveTestCollections(t, srv)

	item := createItem(t, srv, slug, src.Slug, map[string]interface{}{
		"title": "Movable", "fields": `{"note":"hi"}`,
	})

	// Control: without the override the destination's required field is
	// genuinely unsatisfiable, so the move must still be refused.
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{"target_collection": dst.Slug})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("move without the override: expected 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "missing_required_fields" {
		t.Errorf("error code = %q, want missing_required_fields", code)
	}

	// With the override the required field IS satisfied, so the move lands.
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{
			"target_collection": dst.Slug,
			"field_overrides":   map[string]interface{}{"ticket": "T-9"},
		})
	if rr.Code != http.StatusOK {
		t.Fatalf("move with a satisfying override: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var moved models.Item
	parseJSON(t, rr, &moved)
	if moved.CollectionSlug != dst.Slug {
		t.Errorf("item landed in %q, want %q", moved.CollectionSlug, dst.Slug)
	}
	if want := `"ticket":"T-9"`; !strings.Contains(moved.Fields, want) {
		t.Errorf("moved item fields = %s, want it to contain %s", moved.Fields, want)
	}
}

// TestMoveItem_InvalidOverrideRejected — the second half of DR-12. Pre-fix
// "urgent" was written into the item unchecked, because result.Errors was
// empty and nothing else validated the merged map.
func TestMoveItem_InvalidOverrideRejected(t *testing.T) {
	srv := testServer(t)
	slug, src, dst := moveTestCollections(t, srv)

	item := createItem(t, srv, slug, src.Slug, map[string]interface{}{
		"title": "Movable", "fields": `{"note":"hi"}`,
	})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{
			"target_collection": dst.Slug,
			"field_overrides":   map[string]interface{}{"ticket": "T-9", "priority": "urgent"},
		})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for an out-of-options override, got %d: %s", rr.Code, rr.Body.String())
	}
	if code := errCode(t, rr); code != "invalid_fields" {
		t.Errorf("error code = %q, want invalid_fields", code)
	}

	// And the item must not have moved.
	fresh, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if fresh.CollectionID != src.ID {
		t.Errorf("item moved despite the 400 — collection is now %q", fresh.CollectionID)
	}
}

// TestMoveItem_OverrideValueIsTypedServerSide pins the fact BUG-2870 turned
// on. `pad item move --field n=3` sends the value as a STRING — moveCmd is
// the one CLI site that never calls parseFieldFlag — and the item ends up
// with the NUMBER 3 anyway, because the merged map is validated (and its
// values coerced) on this path like every other write door.
//
// That is why BUG-2870's fix routes move through the shared KEY parse only
// and does NOT add client-side typing: for a declared field there is nothing
// left to type, and for the undeclared key a padded entry used to create,
// no client-side typing could have helped. The measurement is easy to lose
// — it is invisible from cmd/pad, where the value is plainly a string — so
// it is pinned here rather than left as a claim on the trail.
func TestMoveItem_OverrideValueIsTypedServerSide(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", slug, err)
	}
	src := mustSchemaCollection(t, srv, ws.ID, "Typed Src", `{"fields":[
		{"key":"note","label":"Note","type":"text"}
	]}`)
	dst := mustSchemaCollection(t, srv, ws.ID, "Typed Dst", `{"fields":[
		{"key":"note","label":"Note","type":"text"},
		{"key":"n","label":"N","type":"number"}
	]}`)

	item := createItem(t, srv, slug, src.Slug, map[string]interface{}{
		"title": "Movable", "fields": `{"note":"hi"}`,
	})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{
			"target_collection": dst.Slug,
			// A STRING, exactly as the CLI sends it.
			"field_overrides": map[string]interface{}{"n": "3"},
		})
	if rr.Code != http.StatusOK {
		t.Fatalf("move with a numeric override: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var moved models.Item
	parseJSON(t, rr, &moved)
	if want := `"n":3`; !strings.Contains(moved.Fields, want) {
		t.Errorf("moved item fields = %s, want it to contain %s — the server types a declared field, "+
			"which is why the CLI does not need to", moved.Fields, want)
	}
	if bad := `"n":"3"`; strings.Contains(moved.Fields, bad) {
		t.Errorf("the value stayed a string: %s", moved.Fields)
	}
}

// ...and the padded companion: a value that cannot be typed is REFUSED here
// rather than stored as a string. This is the half a caller notices, because
// the remote /mcp door used to trim the padding and succeed.
func TestMoveItem_PaddedOverrideValueRefused(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%s): %v", slug, err)
	}
	src := mustSchemaCollection(t, srv, ws.ID, "Padded Src", `{"fields":[
		{"key":"note","label":"Note","type":"text"}
	]}`)
	dst := mustSchemaCollection(t, srv, ws.ID, "Padded Dst", `{"fields":[
		{"key":"note","label":"Note","type":"text"},
		{"key":"n","label":"N","type":"number"}
	]}`)

	item := createItem(t, srv, slug, src.Slug, map[string]interface{}{
		"title": "Movable", "fields": `{"note":"hi"}`,
	})

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/move",
		map[string]interface{}{
			"target_collection": dst.Slug,
			"field_overrides":   map[string]interface{}{"n": " 3"},
		})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a padded numeric override, got %d: %s", rr.Code, rr.Body.String())
	}

	fresh, err := srv.store.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if fresh.CollectionID != src.ID {
		t.Errorf("item moved despite the 400 — collection is now %q", fresh.CollectionID)
	}
}
