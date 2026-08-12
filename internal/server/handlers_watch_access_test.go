package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

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
	if got := srv.filterWatchesByCurrentAccess(user.ID, watches); len(got) != 1 {
		t.Fatalf("expected the watch to remain visible while still a member, got %d", len(got))
	}

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	if err := srv.store.RemoveWorkspaceMember(ws.ID, user.ID); err != nil {
		t.Fatalf("RemoveWorkspaceMember: %v", err)
	}

	filtered := srv.filterWatchesByCurrentAccess(user.ID, watches)
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
	got := srv.filterWatchesByCurrentAccess(user.ID, watches)
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
	got := srv.filterWatchesByCurrentAccess(guest.ID, watches)
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
	if got := srv.filterWatchesByCurrentAccess(stranger.ID, strangerWatches); len(got) != 0 {
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
