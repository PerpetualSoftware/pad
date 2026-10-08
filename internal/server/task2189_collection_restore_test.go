package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2189: archiving a collection hides it and its items; nothing could
// bring them back. GET archived-collections lists what is archived and POST
// .../restore brings one back, items included (the archive never deleted them).

func archivedList(t *testing.T, srv *Server, ws string) []models.ArchivedCollection {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/archived-collections", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list archived: %d %s", rr.Code, rr.Body.String())
	}
	var out []models.ArchivedCollection
	parseJSON(t, rr, &out)
	return out
}

func seedCollectionWithItems(t *testing.T, srv *Server, ws, name string, n int) models.Collection {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{"name": name})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	for i := 0; i < n; i++ {
		irr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug+"/items", map[string]any{"title": name + " item"})
		if irr.Code != http.StatusCreated {
			t.Fatalf("create item: %d %s", irr.Code, irr.Body.String())
		}
	}
	return coll
}

func itemCount(t *testing.T, srv *Server, ws, collSlug string) (int, int) {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/collections/"+collSlug+"/items", nil)
	if rr.Code != http.StatusOK {
		return rr.Code, -1
	}
	var items []models.Item
	parseJSON(t, rr, &items)
	return rr.Code, len(items)
}

func TestRestoreCollection_TASK2189_RoundTripWithItems(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	coll := seedCollectionWithItems(t, srv, ws, "Field Notes", 3)

	if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("archive: %d %s", rr.Code, rr.Body.String())
	}
	if code, _ := itemCount(t, srv, ws, coll.Slug); code != http.StatusNotFound {
		t.Fatalf("archived collection's items answered %d, want 404", code)
	}
	list := archivedList(t, srv, ws)
	if len(list) != 1 || list[0].ID != coll.ID || list[0].Slug != coll.Slug || list[0].ItemCount != 3 || list[0].ArchivedAt == "" {
		t.Fatalf("archived list = %+v, want the collection with 3 items", list)
	}

	// The sync epoch fingerprints the live collection set: a restore must move
	// it, so delta clients resync and receive the restored items (BUG-3428).
	epoch := func() string {
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/items-changes?since=0", nil)
		var body struct {
			AccessEpoch string `json:"access_epoch"`
		}
		parseJSON(t, rr, &body)
		return body.AccessEpoch
	}
	before := epoch()

	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/archived-collections/"+coll.Slug+"/restore", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore by slug: %d %s", rr.Code, rr.Body.String())
	}
	if code, n := itemCount(t, srv, ws, coll.Slug); code != http.StatusOK || n != 3 {
		t.Fatalf("after restore: %d with %d items, want 200 with 3", code, n)
	}
	if len(archivedList(t, srv, ws)) != 0 {
		t.Fatal("a restored collection is still listed as archived")
	}
	if after := epoch(); after == before {
		t.Fatalf("the item-sync epoch did not move on restore (%q): delta clients would never see the items", after)
	}

	// By id, too; and a second restore of a live collection is a 404.
	if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+ws+"/collections/"+coll.Slug, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("re-archive: %d", rr.Code)
	}
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/archived-collections/"+coll.ID+"/restore", nil); rr.Code != http.StatusOK {
		t.Fatalf("restore by id: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/archived-collections/"+coll.Slug+"/restore", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("restoring a LIVE collection answered %d, want 404", rr.Code)
	}
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/archived-collections/no-such-thing/restore", nil); rr.Code != http.StatusNotFound {
		t.Fatalf("restoring an unknown ref answered %d, want 404", rr.Code)
	}
}

// A newer collection with the archived one's name took `x-2`, so a restore
// brings the old one back beside it, both reachable.
func TestRestoreCollection_TASK2189_BesideANewerSameNamedCollection(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	old := seedCollectionWithItems(t, srv, ws, "Notes", 1)
	if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+ws+"/collections/"+old.Slug, nil); rr.Code != http.StatusNoContent {
		t.Fatalf("archive: %d", rr.Code)
	}
	newer := seedCollectionWithItems(t, srv, ws, "Notes", 2)
	if newer.Slug == old.Slug {
		t.Fatalf("the newer collection reused the archived slug %q", old.Slug)
	}
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/archived-collections/"+old.Slug+"/restore", nil); rr.Code != http.StatusOK {
		t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
	}
	if _, n := itemCount(t, srv, ws, old.Slug); n != 1 {
		t.Fatalf("restored collection has %d items, want 1", n)
	}
	if _, n := itemCount(t, srv, ws, newer.Slug); n != 2 {
		t.Fatalf("newer collection has %d items, want 2", n)
	}
}

// A restricted owner can neither list nor restore an archived collection they
// could not see live; the visible one works.
func TestRestoreCollection_TASK2189_RestrictedOwnerVisibility(t *testing.T) {
	f := newRestrictedOwnerVisibilityFixture(t)
	for _, c := range []*models.Collection{f.hiddenColl, f.visibleColl} {
		if err := f.srv.store.DeleteCollection(c.ID, ""); err != nil {
			t.Fatalf("archive %s: %v", c.Slug, err)
		}
	}
	rr := doRequestWithHeaders(f.srv, "GET", "/api/v1/workspaces/"+f.ws.Slug+"/archived-collections", nil, f.bearerHeaders())
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var list []models.ArchivedCollection
	parseJSON(t, rr, &list)
	if len(list) != 1 || list[0].ID != f.visibleColl.ID {
		t.Fatalf("restricted owner's archived list = %+v, want only the visible collection", list)
	}
	hidden := doRequestWithHeaders(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/archived-collections/"+f.hiddenColl.ID+"/restore", nil, f.bearerHeaders())
	if hidden.Code != http.StatusNotFound {
		t.Fatalf("restoring a hidden collection answered %d, want 404", hidden.Code)
	}
	still, err := f.srv.store.ListArchivedCollections(f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	hiddenStillArchived := false
	for _, a := range still {
		hiddenStillArchived = hiddenStillArchived || a.ID == f.hiddenColl.ID
	}
	if !hiddenStillArchived {
		t.Fatal("the hidden collection was restored by a caller who cannot see it")
	}
	visible := doRequestWithHeaders(f.srv, "POST", "/api/v1/workspaces/"+f.ws.Slug+"/archived-collections/"+f.visibleColl.ID+"/restore", nil, f.bearerHeaders())
	if visible.Code != http.StatusOK {
		t.Fatalf("restoring the visible collection answered %d: %s", visible.Code, visible.Body.String())
	}
}
