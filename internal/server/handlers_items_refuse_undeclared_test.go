package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3156: `refuse_undeclared_fields` is a strict opt-in on item update. With
// it, a write that would store a key the collection does not declare is
// refused and writes nothing; without it, nothing changes (BUG-2850's
// accept-and-warn).

// notesWithItem makes a collection declaring only `status`, and one item in it.
func notesWithItem(t *testing.T, srv *Server) (slug, itemSlug string) {
	t.Helper()
	slug = createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Notes",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"]}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/notes/items", map[string]interface{}{
		"title": "A note", "fields": `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return slug, item.Slug
}

func storedFields(t *testing.T, srv *Server, slug, itemSlug string) map[string]any {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+itemSlug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	out := map[string]any{}
	_ = json.Unmarshal([]byte(item.Fields), &out)
	return out
}

func TestRefuseUndeclaredFieldsRefusesAPatchedUndeclaredKey(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, itemSlug := notesWithItem(t, srv)
	path := "/api/v1/workspaces/" + slug + "/items/" + itemSlug

	// The refused request also carries a title and a body: a refusal must
	// stop EVERY write in the request, not just the fields blob (codex
	// round 1 on #1463: a guard moved below those paths would otherwise
	// still pass).
	rr := doRequest(srv, "PATCH", path, map[string]interface{}{
		"fields_patch":             map[string]any{"status": "done", "priority": "high"},
		"title":                    "Renamed by a refused write",
		"content":                  "Body from a refused write",
		"refuse_undeclared_fields": true,
	})
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "validation_error") {
		t.Fatalf("want 400 validation_error, got %d %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `\"priority\"`) && !strings.Contains(rr.Body.String(), `"priority"`) {
		t.Errorf("the refusal must name the undeclared key: %s", rr.Body.String())
	}
	// Nothing was written, not even the declared half of the patch.
	got := storedFields(t, srv, slug, itemSlug)
	if got["status"] != "open" {
		t.Errorf("status = %v, want open: a refused write must write nothing", got["status"])
	}
	if _, ok := got["priority"]; ok {
		t.Errorf("priority was stored despite the refusal: %v", got)
	}
	rr = doRequest(srv, "GET", path, nil)
	var after models.Item
	parseJSON(t, rr, &after)
	if after.Title != "A note" {
		t.Errorf("title = %q: a refused write renamed the item", after.Title)
	}
	if after.Content != "" {
		t.Errorf("content = %q: a refused write stored a body", after.Content)
	}

	// Positive control: the same flag on a declared-only patch is a normal write.
	rr = doRequest(srv, "PATCH", path, map[string]interface{}{
		"fields_patch":             map[string]any{"status": "done"},
		"refuse_undeclared_fields": true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("declared-only patch with the flag: %d %s", rr.Code, rr.Body.String())
	}
	if got := storedFields(t, srv, slug, itemSlug); got["status"] != "done" {
		t.Errorf("status = %v, want done", got["status"])
	}
}

// The lead's condition (3): WITHOUT the flag the write is exactly what it was
// before BUG-3156, accepted with the key named in warnings.undeclared_fields.
// This is also the skew behaviour: a server that predates the flag ignores it,
// which lands here.
func TestWithoutRefuseUndeclaredFieldsAnUndeclaredKeyIsStillAcceptedAndWarned(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, itemSlug := notesWithItem(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+itemSlug, map[string]interface{}{
		"fields_patch": map[string]any{"priority": "high"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Warnings struct {
			UndeclaredFields []string `json:"undeclared_fields"`
		} `json:"warnings"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Warnings.UndeclaredFields) != 1 || resp.Warnings.UndeclaredFields[0] != "priority" {
		t.Errorf("warnings.undeclared_fields = %v, want [priority]", resp.Warnings.UndeclaredFields)
	}
	if got := storedFields(t, srv, slug, itemSlug); got["priority"] != "high" {
		t.Errorf("priority = %v, want high (accepted)", got["priority"])
	}
}

// On a full `fields` write the flag refuses what the warning names there: the
// whole blob. Documented, so a caller who sets it on a full write of an item
// already carrying a stray key knows it will be refused.
func TestRefuseUndeclaredFieldsOnAFullFieldsWrite(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, itemSlug := notesWithItem(t, srv)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+itemSlug, map[string]interface{}{
		"fields":                   `{"status":"done","priority":"high"}`,
		"refuse_undeclared_fields": true,
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d %s", rr.Code, rr.Body.String())
	}
	if got := storedFields(t, srv, slug, itemSlug); got["status"] != "open" {
		t.Errorf("status = %v, want open: nothing written", got["status"])
	}
}
