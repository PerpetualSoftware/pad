package server

import (
	"net/http"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3333: an item grant must never become access to its whole collection.
// VisibleCollectionIDsQ promotes the collection of a granted item into the
// member's (nav-only) visible list without checking the collection is live,
// while GuestVisibleResourcesQ drops grants in deleted collections. Once the
// collection was soft-deleted, the member's grant list came back empty, the
// handlers fell back to the visible list, and served EVERY live item in the
// deleted collection. A whole-collection grant (and an assigned collection)
// already gave the whole collection, so those keep their items, as a full
// member keeps a deleted collection's live items.
func TestBUG3333_DeletedCollectionNotVisibleThroughAGrant(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Target"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")

	mkItem := func(coll, title string) *models.Item {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/"+coll+"/items",
			map[string]any{"title": title}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return &it
	}
	mkColl := func(name string) *models.Collection {
		rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections",
			map[string]any{"name": name}, ownerCookie)
		if rr.Code != http.StatusCreated {
			t.Fatalf("create collection %s: %d %s", name, rr.Code, rr.Body.String())
		}
		var c models.Collection
		parseJSON(t, rr, &c)
		return &c
	}
	// Custom collections: the defaults cannot be deleted.
	research := mkColl("Research")
	notes := mkColl("Notes")
	granted := mkItem(research.Slug, "Granted idea")
	hidden := mkItem(research.Slug, "Hidden idea")
	viaColl := mkItem(notes.Slug, "Doc in a granted collection")
	visible := mkItem("tasks", "Task in an assigned collection")
	ideas, docs := research, notes
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")

	m, err := srv.store.CreateUser(models.UserCreate{Email: "restricted@test.com", Name: "Restricted", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, m.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, m.ID, "specific", []string{tasks.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, m.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, docs.ID, m.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	cookie, err := srv.store.CreateSession(m.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}

	listTitles := func() []string {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/items", nil, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("list items: %d %s", rr.Code, rr.Body.String())
		}
		var items []models.Item
		parseJSON(t, rr, &items)
		var titles []string
		for _, it := range items {
			if it.CollectionID == ideas.ID || it.CollectionID == docs.ID || it.CollectionID == tasks.ID {
				titles = append(titles, it.Title)
			}
		}
		sort.Strings(titles)
		return titles
	}
	canOpen := func(it *models.Item) bool {
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug, nil, cookie)
		return rr.Code == http.StatusOK
	}

	// Before deletion: the granted idea, the granted collection's doc, and
	// the assigned collection's task, never the hidden idea.
	before := []string{"Doc in a granted collection", "Granted idea", "Task in an assigned collection"}
	if got := listTitles(); !bug3333Equal(got, before) {
		t.Fatalf("precondition: list %v, want %v", got, before)
	}
	if canOpen(hidden) {
		t.Fatal("precondition: the hidden idea should not open")
	}

	// Soft-delete the collections holding the item grant and the collection
	// grant. Their items stay live in the database.
	for _, c := range []*models.Collection{ideas, docs} {
		rr := doRequestWithCookie(srv, "DELETE", "/api/v1/workspaces/"+ws.Slug+"/collections/"+c.Slug, nil, ownerCookie)
		if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
			t.Fatalf("delete collection %s: %d %s", c.Slug, rr.Code, rr.Body.String())
		}
	}

	after := []string{"Doc in a granted collection", "Task in an assigned collection"}
	if got := listTitles(); !bug3333Equal(got, after) {
		t.Errorf("after deleting the collections the member lists %v, want %v (nothing from the item-granted collection)", got, after)
	}
	for _, it := range []*models.Item{hidden, granted} {
		if canOpen(it) {
			t.Errorf("%q in the deleted item-granted collection still opens for the restricted member", it.Title)
		}
	}
	for _, it := range []*models.Item{visible, viaColl} {
		if !canOpen(it) {
			t.Errorf("control: %q (assigned or whole-collection grant) should still open", it.Title)
		}
	}
}

func bug3333Equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// The delta-sync door uses GuestVisibleResourcesIncludeDeleted, whose
// collection-grant query did not filter deleted collections either: a guest
// with a grant on a since-deleted collection kept receiving its live items as
// upserts from /items-changes.
func TestBUG3333_GuestDeltaSyncDropsDeletedGrantedCollection(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Target"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]any{"name": "Shared"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var shared models.Collection
	parseJSON(t, rr, &shared)
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/"+shared.Slug+"/items", map[string]any{"title": "Shared doc"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var doc models.Item
	parseJSON(t, rr, &doc)

	guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest@test.com", Name: "Guest", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, shared.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	// A second, live grant keeps the guest in the workspace after the shared
	// collection is deleted; without one the middleware answers 403 and
	// this door is never reached.
	rr = doRequestWithCookie(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]any{"title": "Other task"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create task: %d %s", rr.Code, rr.Body.String())
	}
	var task models.Item
	parseJSON(t, rr, &task)
	if _, err := srv.store.CreateItemGrant(ws.ID, task.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	cookie, err := srv.store.CreateSession(guest.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	liveUpsert := func() bool {
		t.Helper()
		rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/items-changes?since=0", nil, cookie)
		if rr.Code != http.StatusOK {
			t.Fatalf("items-changes: %d %s", rr.Code, rr.Body.String())
		}
		var resp itemsChangesResponse
		parseJSON(t, rr, &resp)
		for _, c := range resp.Changes {
			if c.ID == doc.ID && !c.Deleted && !c.MovedOut {
				return true
			}
		}
		return false
	}
	if !liveUpsert() {
		t.Fatal("precondition: the guest should receive the granted collection's doc")
	}
	rr = doRequestWithCookie(srv, "DELETE", "/api/v1/workspaces/"+ws.Slug+"/collections/"+shared.Slug, nil, ownerCookie)
	if rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
		t.Fatalf("delete collection: %d %s", rr.Code, rr.Body.String())
	}
	if liveUpsert() {
		t.Error("after its collection was deleted, the guest still receives the doc as a live upsert from /items-changes")
	}
}
