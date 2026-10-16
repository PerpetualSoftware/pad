package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
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
	docs, _ := srv.store.GetCollectionBySlug(ws.ID, "docs")
	if tasks == nil || ideas == nil || docs == nil {
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

	// The archived grant is the ONLY grant in its collection, so once it is
	// archived nothing keeps that collection in the caller's visible list:
	// the case the grant-list swap alone does not fix.
	archivedGranted := mk(ideas.ID, "Granted idea, then archived")
	liveGranted := mk(docs.ID, "Granted doc, still live")
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
	// A restricted member assigned NO collection whose only grant is the
	// archived item: its visible list is empty, and the store must still run
	// the query for its grant. (A guest in that position loses the workspace
	// itself: a 403 before /changes runs.)
	soloMember, err := srv.store.CreateUser(models.UserCreate{Email: "solo@test.com", Name: "Solo", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, soloMember.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, soloMember.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, archivedGranted.ID, soloMember.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
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
		live bool // holds the live grant too
	}{{"restricted member", member, true}, {"guest", guest, true}, {"member granted only the archived item", soloMember, false}} {
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
			if r := rows[liveGranted.ID]; tc.live && (r == nil || r.DeletedAt != nil || deleted[liveGranted.ID]) {
				t.Errorf("the live granted item is not a live row (row %v, in deleted %v)", r != nil, deleted[liveGranted.ID])
			} else if !tc.live && r != nil {
				t.Error("a caller never granted the live item received it")
			}
			if deleted[archivedHidden.ID] || rows[archivedHidden.ID] != nil {
				t.Error("an archived item the caller was never granted reached it")
			}
			if deleted[deletedCollGranted.ID] || rows[deletedCollGranted.ID] != nil {
				t.Error("a granted item in a deleted collection reached the caller (BUG-3333)")
			}

			// Paged one row at a time, the walk delivers the same two rows
			// (hunter r1 Q1): the window and cursor are over visible rows,
			// and the archived granted row is one of them.
			var walked []string
			for _, p := range bug3791Walk(t, srv, ws.Slug, cookie, since.UnixMilli(), 1, nil) {
				for _, it := range p.Updated {
					walked = append(walked, it.ID)
				}
			}
			slices.Sort(walked)
			want := []string{archivedGranted.ID}
			if tc.live {
				want = append(want, liveGranted.ID)
			}
			slices.Sort(want)
			if !slices.Equal(walked, want) {
				t.Errorf("limit=1 walk returned %v, want exactly the caller's granted items %v", walked, want)
			}
		})
	}
}

// A granted item is no longer tested against the visible-collection list
// (BUG-3796), so for an app request the grant list's own ceiling is the only
// one that applies to it. The include-deleted variant must keep an archived
// granted item inside the ceiling and drop one outside it (hunter r1 Q1). Not
// reachable through a route today (app tokens are refused on /api/v1), so this
// drives the filter /changes calls, as BUG-3424's test does.
func TestBUG3796_ArchivedGrantKeepsTheAppCeiling(t *testing.T) {
	srv := testServer(t)
	mkUser := func(email, role string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, member := mkUser("owner@example.com", "admin"), mkUser("member@example.com", "member")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Ceiling 3796", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ id, role string }{{owner.ID, "owner"}, {member.ID, "editor"}} {
		if err := srv.store.AddWorkspaceMember(ws.ID, m.id, m.role); err != nil {
			t.Fatal(err)
		}
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	coll := func(name, slug, prefix string) *models.Collection {
		c, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: name, Slug: slug, Prefix: prefix, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	inside, outside := coll("Inside", "inside", "INS"), coll("Outside", "outside", "OUT")
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}
	archived := func(c *models.Collection, title string) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.CreateItemGrant(ws.ID, it.ID, member.ID, "view", owner.ID); err != nil {
			t.Fatal(err)
		}
		if err := srv.store.DeleteItem(it.ID); err != nil {
			t.Fatal(err)
		}
		return it
	}
	inItem, outItem := archived(inside, "archived, inside the ceiling"), archived(outside, "archived, outside the ceiling")

	filter := func(ac *appContext) []string {
		t.Helper()
		r := httptest.NewRequest("GET", "/", nil)
		ctx := WithCurrentUser(r.Context(), member)
		if ac != nil {
			ctx = context.WithValue(ctx, appCtxKey{}, ac)
		}
		_, items, err := srv.guestResourceFilterIncludeDeletedItems(r.WithContext(ctx), ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	// Control: without an app context both archived grants are in the list.
	if items := filter(nil); !slices.Contains(items, inItem.ID) || !slices.Contains(items, outItem.ID) {
		t.Fatalf("control: granted items %v; want both archived grants", items)
	}
	items := filter(&appContext{WorkspaceID: ws.ID, ReadCeiling: []string{inside.ID}})
	if slices.Contains(items, outItem.ID) {
		t.Errorf("an app request's include-deleted grants carry %s, archived outside its ceiling", outItem.ID)
	}
	if !slices.Contains(items, inItem.ID) {
		t.Errorf("an app request's include-deleted grants %v lost %s, archived inside its ceiling", items, inItem.ID)
	}
}
