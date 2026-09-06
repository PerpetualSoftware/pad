package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestSSEVisibilityAccessEpoch_TracksNarrowingAndMatchesTheItemDoors covers the
// two things the 60s revalidation tick's sync_required emission rests on
// (IDEA-2898).
//
//  1. The epoch MOVES when the caller's collection access narrows. Without that
//     the tick is silent and the client's warm index keeps the revoked rows
//     until it next bootstraps.
//  2. The epoch AGREES with what the item doors send for the same caller. These
//     are two values on two wires — one from an SSE tick, one from
//     /items-changes — and the client compares the second against what it
//     stored. If the two sides could disagree about what "the same set" means,
//     the tick would announce a change the delta then denied, or stay silent on
//     one it made. The tick tells the client to go and reconcile; the delta is
//     what decides. They have to be one definition, and this is the only place
//     that can catch them drifting apart.
func TestSSEVisibilityAccessEpoch_TracksNarrowingAndMatchesTheItemDoors(t *testing.T) {
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "EpochVis", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	kept, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Kept", Slug: "kept", Prefix: "KEP", Schema: schema})
	if err != nil {
		t.Fatalf("create kept: %v", err)
	}
	revoked, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Revoked", Slug: "revoked", Prefix: "REV", Schema: schema})
	if err != nil {
		t.Fatalf("create revoked: %v", err)
	}

	mkReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
		return req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, user))
	}
	// The item doors' own view of the same caller, through the same helper
	// they use — not a re-derivation, which would let this test agree with a
	// wrong answer.
	doorEpoch := func() string {
		visibleIDs, err := srv.visibleCollectionIDs(mkReq(), ws.ID)
		if err != nil {
			t.Fatalf("visibleCollectionIDs: %v", err)
		}
		_, grantedItemIDs, err := srv.guestResourceFilter(mkReq(), ws.ID)
		if err != nil {
			t.Fatalf("guestResourceFilter: %v", err)
		}
		return computeAccessEpoch(visibleIDs, grantedItemIDs)
	}

	if err := srv.store.SetMemberCollectionAccess(ws.ID, user.ID, "specific", []string{kept.ID, revoked.ID}); err != nil {
		t.Fatalf("grant both collections: %v", err)
	}
	before := srv.computeSSEVisibility(mkReq(), ws.ID).accessEpoch()
	if before != doorEpoch() {
		t.Errorf("SSE tick and item doors disagree before revocation: %q vs %q", before, doorEpoch())
	}

	if err := srv.store.SetMemberCollectionAccess(ws.ID, user.ID, "specific", []string{kept.ID}); err != nil {
		t.Fatalf("revoke one collection: %v", err)
	}
	after := srv.computeSSEVisibility(mkReq(), ws.ID).accessEpoch()
	if after == before {
		t.Errorf("epoch unchanged across a narrowing (%q) — the tick would stay silent", after)
	}
	if after != doorEpoch() {
		t.Errorf("SSE tick and item doors disagree after revocation: %q vs %q", after, doorEpoch())
	}

	// Widening back is a change too, and the tick should announce it: the
	// client's cache is missing rows it may now see, and the same resync is
	// what fetches them.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, user.ID, "all", nil); err != nil {
		t.Fatalf("restore full access: %v", err)
	}
	widened := srv.computeSSEVisibility(mkReq(), ws.ID).accessEpoch()
	if widened == after {
		t.Errorf("epoch unchanged across a widening (%q)", widened)
	}
	if widened != doorEpoch() {
		t.Errorf("SSE tick and item doors disagree after widening: %q vs %q", widened, doorEpoch())
	}
}
