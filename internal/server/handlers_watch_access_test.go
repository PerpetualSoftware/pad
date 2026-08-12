package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// noBearerRequest returns a bare request with no Authorization header —
// isBearerAuth(r) reports false for it, matching a cookie-session-style
// caller. None of this file's tests exercise an admin user, so the
// bearer-vs-cookie distinction (TASK-2533 codex round 2 finding 2) is
// inert for them either way; this just satisfies
// computeWatchAccessVisibility's signature.
func noBearerRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/api/v1/watches", nil)
}

// TestFilterWatchesByCurrentAccess_ExcludesRevokedMembership covers
// codex round-1 finding 1: a watch row survives a workspace membership
// removal (nothing deletes it), so without this filter a removed user
// would keep seeing / receiving nudges for a workspace they can no
// longer access.
func TestFilterWatchesByCurrentAccess_ExcludesRevokedMembership(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	watches, err := srv.store.ListWatchesForUser(user.ID)
	if err != nil {
		t.Fatalf("ListWatchesForUser: %v", err)
	}
	if len(watches) != 1 {
		t.Fatalf("expected 1 watch before revocation, got %d", len(watches))
	}
	if got := srv.filterWatchesByCurrentAccess(noBearerRequest(), user.ID, watches); len(got) != 1 {
		t.Fatalf("expected the watch to remain visible while still a member, got %d", len(got))
	}

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.RemoveWorkspaceMember(ws.ID, user.ID); err != nil {
		t.Fatalf("RemoveWorkspaceMember: %v", err)
	}

	filtered := srv.filterWatchesByCurrentAccess(noBearerRequest(), user.ID, watches)
	if len(filtered) != 0 {
		t.Fatalf("expected the watch to be excluded after membership removal, got %+v", filtered)
	}
}

// TestFilterWatchesByCurrentAccess_KeepsActiveMembership is the happy-path
// sanity check alongside the revocation test above.
func TestFilterWatchesByCurrentAccess_KeepsActiveMembership(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	_, item, _, user := setupWatchTestUser(t, srv)

	watches := []models.Watch{{UserID: user.ID, ItemID: item.ID, ItemCollectionID: item.CollectionID, WorkspaceID: item.WorkspaceID}}
	got := srv.filterWatchesByCurrentAccess(noBearerRequest(), user.ID, watches)
	if len(got) != 1 {
		t.Fatalf("expected the active member's watch to remain visible, got %d", len(got))
	}
}

// TestFilterWatchesByCurrentAccess_GuestItemGrantKeepsWatch exercises the
// item-level-grant branch: a non-member guest with an explicit item
// grant (not a full collection grant) must still see their watch on
// that specific item.
func TestFilterWatchesByCurrentAccess_GuestItemGrantKeepsWatch(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Grant target", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	guest, err := srv.store.CreateUser(models.UserCreate{
		Email: "guest@example.com", Name: "Guest", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Deliberately NOT added as a workspace member — item-grant-only guest.
	if _, err := srv.store.CreateItemGrant(ws.ID, item.ID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	watches := []models.Watch{{UserID: guest.ID, ItemID: item.ID, ItemCollectionID: item.CollectionID, WorkspaceID: ws.ID}}
	got := srv.filterWatchesByCurrentAccess(noBearerRequest(), guest.ID, watches)
	if len(got) != 1 {
		t.Fatalf("expected the item-granted guest's watch to remain visible, got %d", len(got))
	}

	// A DIFFERENT unrelated user (no membership, no grant) must be denied.
	stranger, err := srv.store.CreateUser(models.UserCreate{
		Email: "stranger@example.com", Name: "Stranger", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser (stranger): %v", err)
	}
	strangerWatches := []models.Watch{{UserID: stranger.ID, ItemID: item.ID, ItemCollectionID: item.CollectionID, WorkspaceID: ws.ID}}
	if got := srv.filterWatchesByCurrentAccess(noBearerRequest(), stranger.ID, strangerWatches); len(got) != 0 {
		t.Fatalf("expected a stranger with no membership or grant to be denied, got %d", len(got))
	}
}

// TestListWatches_ExcludesRevokedAccess is the HTTP-level regression
// test: GET /api/v1/watches must not return a watch on a workspace the
// caller has since been removed from.
func TestListWatches_ExcludesRevokedAccess(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug, item, tok, user := setupWatchTestUser(t, srv)

	rr := bearerCall(t, srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/watch", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("create watch: %d %s", rr.Code, rr.Body.String())
	}

	rr = bearerCall(t, srv, "GET", "/api/v1/watches", tok.Token, nil)
	var before []models.Watch
	parseJSON(t, rr, &before)
	if len(before) != 1 {
		t.Fatalf("expected 1 watch before revocation, got %d", len(before))
	}

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.RemoveWorkspaceMember(ws.ID, user.ID); err != nil {
		t.Fatalf("RemoveWorkspaceMember: %v", err)
	}

	rr = bearerCall(t, srv, "GET", "/api/v1/watches", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list watches after revocation: expected 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	var after []models.Watch
	parseJSON(t, rr, &after)
	if len(after) != 0 {
		t.Fatalf("expected 0 watches after membership removal, got %+v", after)
	}
}

// TestFilterWatchesByCurrentAccess_ItemGrantDoesNotWidenToSiblingItem
// covers TASK-2533 codex round 2 finding 1: VisibleCollectionIDs /
// GuestVisibleCollectionIDs deliberately over-widen for navigation — a
// collection ID is included in their result if the caller has an item
// grant on ANY item inside it, explicitly leaving item-level narrowing
// to the caller (their own doc comments say so). The previous
// computeWatchAccessVisibility used that over-wide set directly as the
// "fully visible" gate, so a guest granted item A was treated as having
// full access to A's WHOLE collection — including a sibling item B they
// were never granted. This asserts the fix: item A's watch stays
// visible, a hypothetical watch on sibling item B does not.
func TestFilterWatchesByCurrentAccess_ItemGrantDoesNotWidenToSiblingItem(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rrA := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Granted item", "fields": `{"status":"open"}`})
	if rrA.Code != http.StatusCreated {
		t.Fatalf("seed item A: %d %s", rrA.Code, rrA.Body.String())
	}
	var itemA models.Item
	parseJSON(t, rrA, &itemA)

	rrB := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items",
		map[string]interface{}{"title": "Sibling item", "fields": `{"status":"open"}`})
	if rrB.Code != http.StatusCreated {
		t.Fatalf("seed item B: %d %s", rrB.Code, rrB.Body.String())
	}
	var itemB models.Item
	parseJSON(t, rrB, &itemB)

	if itemA.CollectionID != itemB.CollectionID {
		t.Fatalf("test setup bug: items must share a collection, got %q and %q", itemA.CollectionID, itemB.CollectionID)
	}

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	guest, err := srv.store.CreateUser(models.UserCreate{
		Email: "sibling-guest@example.com", Name: "Sibling Guest", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Grant ONLY item A — item B is an ungranted sibling in the SAME collection.
	if _, err := srv.store.CreateItemGrant(ws.ID, itemA.ID, guest.ID, "view", guest.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	watches := []models.Watch{
		{UserID: guest.ID, ItemID: itemA.ID, ItemCollectionID: itemA.CollectionID, WorkspaceID: ws.ID},
		{UserID: guest.ID, ItemID: itemB.ID, ItemCollectionID: itemB.CollectionID, WorkspaceID: ws.ID},
	}
	got := srv.filterWatchesByCurrentAccess(noBearerRequest(), guest.ID, watches)

	visible := make(map[string]bool, len(got))
	for _, w := range got {
		visible[w.ItemID] = true
	}
	if !visible[itemA.ID] {
		t.Errorf("expected the granted item A's watch to remain visible")
	}
	if visible[itemB.ID] {
		t.Errorf("expected the UNGRANTED sibling item B's watch to be denied — item grant must not widen to the whole collection")
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 visible watch (item A only), got %d: %+v", len(got), got)
	}
}
