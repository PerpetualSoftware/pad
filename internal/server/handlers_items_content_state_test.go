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

// TestPublicShareDoorsCarryTheMarkerBothWays covers the two public-share doors
// (BUG-3000), and covers them in BOTH directions on the same item.
//
// These two are the only item-body doors that build an explicit ALLOW-LIST rather
// than serialising models.Item, so neither inherits the marker — and the single-item
// one was folded in while the COLLECTION one was missed, which is the whole reason
// this test names both. They are also the doors whose reader can least help
// themselves: an anonymous viewer has no editor, no op-log, and nothing to compare
// the body against.
//
// The absence direction is not decoration. "Only when set" is what keeps the key set
// byte-identical for a current row, which is what the existing exact-shape pin on
// the item DTO depends on; a marker that were always present would satisfy every
// positive assertion here and break that instead.
func TestPublicShareDoorsCarryTheMarkerBothWays(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Shared body", `{"status":"open"}`)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("get workspace: %v", err)
	}
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "share-owner@test.com", Name: "Owner", Password: "pw-owner",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	coll, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || coll == nil {
		t.Fatalf("get collection: %v", err)
	}

	itemLink, err := srv.store.CreateShareLink(ws.ID, "item", item.ID, "view", owner.ID, nil)
	if err != nil {
		t.Fatalf("create item share link: %v", err)
	}
	collLink, err := srv.store.CreateShareLink(ws.ID, "collection", coll.ID, "view", owner.ID, nil)
	if err != nil {
		t.Fatalf("create collection share link: %v", err)
	}

	// itemState reads the single-item share; collState reads the shared item's row
	// out of the collection share. Both return "" when the key is absent, which is
	// the state the absence direction asserts.
	itemState := func(t *testing.T) string {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/s/"+itemLink.Token, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("resolve item share: %d: %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Item map[string]any `json:"item"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode item share: %v", err)
		}
		if _, ok := resp.Item["content"]; !ok {
			t.Fatal("the item share carried no content at all; this door measures nothing")
		}
		s, _ := resp.Item["content_state"].(string)
		return s
	}
	collState := func(t *testing.T) string {
		t.Helper()
		rr := doRequest(srv, "GET", "/api/v1/s/"+collLink.Token, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("resolve collection share: %d: %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode collection share: %v", err)
		}
		for _, row := range resp.Items {
			if row["ref"] != item.Ref {
				continue
			}
			if _, ok := row["content"]; !ok {
				t.Fatal("the collection share carried no content at all; this door measures nothing")
			}
			s, _ := row["content_state"].(string)
			return s
		}
		t.Fatalf("the collection share did not include %s; this door measured nothing", item.Ref)
		return ""
	}

	// ABSENCE FIRST, so the presence assertions below cannot be read as an
	// always-on marker.
	if got := itemState(t); got != "" {
		t.Fatalf("item share marks a current row %q", got)
	}
	if got := collState(t); got != "" {
		t.Fatalf("collection share marks a current row %q", got)
	}

	// Put the document ahead of the row.
	if _, err := srv.store.AppendYjsUpdate(item.ID, []byte{1, 2, 3}, "1"); err != nil {
		t.Fatalf("AppendYjsUpdate: %v", err)
	}

	if got := itemState(t); got != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("item share content_state = %q, want %q", got, models.ContentOutcomeAppliedPendingFlush)
	}
	if got := collState(t); got != models.ContentOutcomeAppliedPendingFlush {
		t.Errorf("collection share content_state = %q, want %q — an anonymous viewer is served a "+
			"stale body with no signal", got, models.ContentOutcomeAppliedPendingFlush)
	}
}
