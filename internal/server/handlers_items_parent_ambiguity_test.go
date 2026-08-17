package server

// BUG-2594 — one PATCH carrying BOTH a hierarchy write in fields /
// fields_patch ("parent" or its "plan" alias, any value including the
// empty-string clear) AND a top-level "parent_id" used to apply both:
// extractParentLink staged the item_links write while ItemUpdate.ParentID
// stamped the column unconditionally in the same transaction — clearing
// the link while re-parenting the column, silently inconsistent. The
// shape is raw-HTTP-only (no first-party client sends top-level
// parent_id on update), and it is now REFUSED, not silently resolved,
// per the clear_parent contract family's standing rule (v0.19).

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func seedParentAndChild(t *testing.T, srv *Server, slug string) (parent, child models.Item) {
	t.Helper()
	parent = createTaskWithFields(t, srv, slug, "Ambiguity parent", `{"status":"open"}`)
	child = createTaskWithFields(t, srv, slug, "Ambiguity child", `{"status":"open"}`)
	return parent, child
}

func assertAmbiguityRefused(t *testing.T, rr interface {
	// httptest.ResponseRecorder shape without importing it here
	Result() *http.Response
}, code int, body string) {
	t.Helper()
	if code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", code, body)
	}
	// The refusal is the standard validation envelope and must NAME the
	// competing keys — both the canonical "parent" and its "plan" alias —
	// so a raw-HTTP client can fix its request without spelunking. (The
	// body is JSON-encoded, so quoted keys arrive as \"parent\".)
	if !strings.Contains(body, `"code":"validation_error"`) {
		t.Errorf("refusal should use the validation_error code, got: %s", body)
	}
	if !strings.Contains(body, "parent_id") ||
		!strings.Contains(body, `\"parent\"`) ||
		!strings.Contains(body, `\"plan\"`) {
		t.Errorf("refusal should name both competing keys (and the plan alias), got: %s", body)
	}
}

func TestPatchItem_ParentClearPlusParentID_Refused(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	parent, child := seedParentAndChild(t, srv, slug)

	// The filed shape: fields_patch clears the link while parent_id
	// re-stamps the column.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"parent": ""},
		"parent_id":    parent.ID,
	})
	assertAmbiguityRefused(t, rr, rr.Code, rr.Body.String())
}

func TestPatchItem_ParentSetPlusParentID_Refused(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	parent, child := seedParentAndChild(t, srv, slug)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"parent": parent.ID},
		"parent_id":    parent.ID,
	})
	assertAmbiguityRefused(t, rr, rr.Code, rr.Body.String())
}

func TestPatchItem_PlanAliasPlusParentID_Refused(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	parent, child := seedParentAndChild(t, srv, slug)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"plan": parent.ID},
		"parent_id":    parent.ID,
	})
	assertAmbiguityRefused(t, rr, rr.Code, rr.Body.String())
}

func TestPatchItem_FullFieldsParentPlusParentID_Refused(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	parent, child := seedParentAndChild(t, srv, slug)

	// The full-`fields` sibling path shares extractParentLink and gets
	// the same refusal.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Slug, map[string]interface{}{
		"fields":    `{"status":"open","parent":"` + parent.ID + `"}`,
		"parent_id": parent.ID,
	})
	assertAmbiguityRefused(t, rr, rr.Code, rr.Body.String())
}

// Controls: each hierarchy write ALONE keeps working exactly as before —
// the guard must refuse the pair, not either member.
func TestPatchItem_ParentViaFieldsPatchAlone_StillWorks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	parent, child := seedParentAndChild(t, srv, slug)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Slug, map[string]interface{}{
		"fields_patch": map[string]interface{}{"parent": parent.ID},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("fields_patch parent alone: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestPatchItem_TopLevelParentIDAlone_StillWorks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	parent, child := seedParentAndChild(t, srv, slug)

	// Pre-existing raw-HTTP behavior deliberately unchanged (the audit
	// found no first-party sender; solo parent_id is out of this bug's
	// scope — see BUG-2379 for the adjacent undeclared-override family).
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Slug, map[string]interface{}{
		"parent_id": parent.ID,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("parent_id alone: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
}
