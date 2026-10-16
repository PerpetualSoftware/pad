package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3796: a grant-only caller (a restricted member or a guest whose only
// access to an item is an item grant) must receive that item's archive through
// GET /changes, as /items-changes delivers it (TASK-1354). The grant list
// dropped soft-deleted items, and the visible-collection list drops a granted
// item's collection once the item is archived (BUG-3333 keeps it nav-only and
// live-only), so the row was filtered out twice and the client kept showing
// the item live until a full refresh.
func TestBUG3796_ArchivedGrantedItemReachesGranteeThroughChanges(t *testing.T) {
	srv := testServer(t)
	ownerCookie := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Archive 3796"}, ownerCookie)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	owner, _ := srv.store.GetUserByEmail("owner@test.com")
	tasks, _ := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	ideas, _ := srv.store.GetCollectionBySlug(ws.ID, "ideas")
	if tasks == nil || ideas == nil {
		t.Fatal("precondition: startup collections")
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
	mk := func(collID, title string) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, collID, models.ItemCreate{Title: title})
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	research := mkColl("Research") // custom: the defaults cannot be deleted

	// The collection-level grant state is set up BEFORE since, so the only
	// rows after it are the items below.
	since := time.Now().Add(-2 * time.Second)

	archivedGranted := mk(ideas.ID, "Granted idea, then archived")
	liveGranted := mk(ideas.ID, "Granted idea, still live")
	archivedHidden := mk(ideas.ID, "Hidden idea, archived")
	deletedCollGranted := mk(research.ID, "Granted idea in a deleted collection")

	member, err := srv.store.CreateUser(models.UserCreate{Email: "restricted@test.com", Name: "Restricted", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{tasks.ID}); err != nil {
		t.Fatal(err)
	}
	guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest@test.com", Name: "Guest", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []*models.User{member, guest} {
		for _, it := range []*models.Item{archivedGranted, liveGranted, deletedCollGranted} {
			if _, err := srv.store.CreateItemGrant(ws.ID, it.ID, u.ID, "view", owner.ID); err != nil {
				t.Fatal(err)
			}
		}
	}

	if err := srv.store.DeleteItem(archivedGranted.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.DeleteItem(archivedHidden.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.DeleteItem(deletedCollGranted.ID); err != nil {
		t.Fatal(err)
	}
	rc, _ := srv.store.GetCollection(research.ID)
	if err := srv.store.DeleteCollection(research.ID, rc.UpdatedAt.UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("delete collection: %v", err)
	}

	for _, tc := range []struct {
		name string
		user *models.User
	}{{"restricted member", member}, {"guest", guest}} {
		t.Run(tc.name, func(t *testing.T) {
			cookie, err := srv.store.CreateSession(tc.user.ID, "web-test", "192.0.2.1", "", webSessionTTL)
			if err != nil {
				t.Fatal(err)
			}
			got := bug3791Get(t, srv, fmt.Sprintf("/api/v1/workspaces/%s/changes?since=%d", ws.Slug, since.UnixMilli()), cookie)
			deleted := map[string]bool{}
			for _, id := range got.Deleted {
				deleted[id] = true
			}
			rows := map[string]*models.Item{}
			for i := range got.Updated {
				rows[got.Updated[i].ID] = &got.Updated[i]
			}

			if !deleted[archivedGranted.ID] || rows[archivedGranted.ID] == nil {
				t.Errorf("the archived granted item is missing: in deleted %v, row present %v", deleted[archivedGranted.ID], rows[archivedGranted.ID] != nil)
			}
			if r := rows[liveGranted.ID]; r == nil || r.DeletedAt != nil || deleted[liveGranted.ID] {
				t.Errorf("the live granted item is not a live row (row %v, in deleted %v)", r != nil, deleted[liveGranted.ID])
			}
			if deleted[archivedHidden.ID] || rows[archivedHidden.ID] != nil {
				t.Error("an archived item the caller was never granted reached it")
			}
			if deleted[deletedCollGranted.ID] || rows[deletedCollGranted.ID] != nil {
				t.Error("a granted item in a deleted collection reached the caller (BUG-3333)")
			}
		})
	}
}
