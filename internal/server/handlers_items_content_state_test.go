package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3000 at the HTTP door. The store test proves the predicate; this proves the
// marker actually REACHES a caller, which is a separate claim — a field can be
// computed correctly and dropped by a projection on the way out, and this codebase
// has a summary projection that deliberately drops `content`.
//
// The population is enumerated on the BUG-3000 trail. The doors driven here are the
// ones a caller reaches for first: the single-item read and the list.
func TestContentStateReachesTheHTTPReadDoors(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Stale body", `{"status":"open"}`)

	decodeItem := func(t *testing.T, path string) models.Item {
		t.Helper()
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d: %s", path, rr.Code, rr.Body.String())
		}
		var got models.Item
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return got
	}

	itemPath := "/api/v1/workspaces/" + slug + "/items/" + item.Slug

	// CONTROL FIRST, so the assertion below cannot be read as an always-on marker.
	if got := decodeItem(t, itemPath); got.ContentState != "" {
		t.Fatalf("a fresh item is marked %q over HTTP; everything below would prove nothing", got.ContentState)
	}

	// Put the document ahead of the row — the state an applier-path write leaves.
	if _, err := srv.store.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}

	got := decodeItem(t, itemPath)
	if got.ContentState != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("GET item content_state = %q, want %q — the caller is served a stale body with no signal",
			got.ContentState, models.ContentOutcomeAppliedPendingFlush)
	}

	// The raw JSON must carry the key, not merely the decoded struct: a Go struct
	// round-trip would pass even if the field were never serialised.
	rr := doRequest(srv, "GET", itemPath, nil)
	var raw map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	if raw["content_state"] != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("wire JSON content_state = %v, want %q", raw["content_state"],
			models.ContentOutcomeAppliedPendingFlush)
	}

	// The list door must agree. A marker on one door and not another is exactly the
	// failure the enumeration exists to prevent.
	listRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/tasks/items", nil)
	if listRR.Code != http.StatusOK {
		t.Fatalf("list: %d: %s", listRR.Code, listRR.Body.String())
	}
	var listed []models.Item
	if err := json.Unmarshal(listRR.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	var seen bool
	for _, li := range listed {
		if li.ID != item.ID {
			continue
		}
		seen = true
		if li.ContentState != models.ContentOutcomeAppliedPendingFlush {
			t.Errorf("list content_state = %q, want %q", li.ContentState,
				models.ContentOutcomeAppliedPendingFlush)
		}
	}
	if !seen {
		t.Fatal("the item did not come back from the list door; that leg measured nothing")
	}
}
