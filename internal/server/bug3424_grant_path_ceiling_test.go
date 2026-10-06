package server

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3424: a caller holding item grants is filtered by guestResourceFilter's
// fullCollIDs and grantedItemIDs instead of by visibleCollectionIDs, and only
// visibleCollectionIDs applied an app request's read ceiling (SPEC-6 U6a). So
// an app principal with item grants would have been served rows outside its
// ceiling on every door that takes the grant path. guestResourceFilter now
// narrows both lists to the ceiling.
//
// Not reachable through a route today (app tokens are refused on /api/v1 and
// the app router exposes none of the grant-path doors), so the test drives the
// filter the doors share, with the app context a request would carry.
func TestBug3424_ItemGrantPathKeepsTheAppCeiling(t *testing.T) {
	srv := testServer(t)
	mk := func(email, role string) *models.User {
		u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple", Role: role})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner, member := mk("owner@example.com", "admin"), mk("member@example.com", "member")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Ceiling", OwnerID: owner.ID})
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
	// inside: in the app's ceiling. outside: not. granted: a collection the
	// member holds a whole-collection grant on, outside the ceiling.
	inside, outside, granted := coll("Inside", "inside", "INS"), coll("Outside", "outside", "OUT"), coll("Granted", "granted", "GRA")
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{inside.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, granted.ID, member.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	item := func(c *models.Collection, title string) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, c.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := srv.store.CreateItemGrant(ws.ID, it.ID, member.ID, "view", owner.ID); err != nil {
			t.Fatal(err)
		}
		return it
	}
	inItem, outItem := item(inside, "granted, inside the ceiling"), item(outside, "granted, outside the ceiling")

	filter := func(ac *appContext) ([]string, []string) {
		t.Helper()
		r := httptest.NewRequest("GET", "/", nil)
		ctx := WithCurrentUser(r.Context(), member)
		if ac != nil {
			ctx = context.WithValue(ctx, appCtxKey{}, ac)
		}
		full, items, err := srv.guestResourceFilter(r.WithContext(ctx), ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return full, items
	}

	// Control: with no app context the member's own grants all apply, so the
	// fixture really does put rows outside the ceiling on this path.
	full, items := filter(nil)
	if !slices.Contains(full, granted.ID) || !slices.Contains(items, outItem.ID) || !slices.Contains(items, inItem.ID) {
		t.Fatalf("control: the member's own filter = full %v, items %v; want the granted collection and both granted items", full, items)
	}

	// The same member through an app whose ceiling is the inside collection.
	full, items = filter(&appContext{WorkspaceID: ws.ID, ReadCeiling: []string{inside.ID}})
	for _, id := range full {
		if id != inside.ID {
			t.Errorf("app request's full collections include %s, outside its ceiling %v", id, []string{inside.ID})
		}
	}
	if slices.Contains(items, outItem.ID) {
		t.Errorf("app request's granted items include %s, whose collection is outside its ceiling", outItem.ID)
	}
	if !slices.Contains(items, inItem.ID) {
		t.Errorf("app request's granted items %v lost %s, which is inside its ceiling", items, inItem.ID)
	}
	if items == nil {
		t.Errorf("granted items came back nil, which reads as \"no grant filtering\"")
	}
}
