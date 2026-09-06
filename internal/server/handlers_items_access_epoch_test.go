package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestItemsChanges_AccessEpochChangesOnRevocation is the IDEA-2898 repro.
//
// The defect: a member's collection access is narrowed with NO accompanying
// item write. No row changes, so /items-changes has nothing to say — and the
// moved-out tombstone path (BUG-1675) only fires when an ITEM moves, not when
// the CALLER's access set moves. The client's local index therefore keeps
// serving the revoked collection's rows from its warm cache, which is what
// ItemPicker lists.
//
// The assertion this test exists for is the SECOND half: the response must
// carry a per-caller access fingerprint that CHANGES when the caller's
// effective visible set changes, so the client can trigger the authoritative
// resync it already implements (localIndex.svelte.ts resyncProjectionScope).
//
// The first half — that the delta itself carries no eviction signal — is
// asserted too, and it is TRUE BOTH BEFORE AND AFTER the fix. It is the
// mechanism, not the fix; it is here so a future reader can see why the
// fingerprint is the signal rather than a tombstone.
func TestItemsChanges_AccessEpochChangesOnRevocation(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "AccessEpochWS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add admin member: %v", err)
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

	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	// Both collections visible to start.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{kept.ID, revoked.ID}); err != nil {
		t.Fatalf("set member collection access: %v", err)
	}
	token, err := srv.store.CreateSession(member.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	item, err := srv.store.CreateItem(ws.ID, revoked.ID, models.ItemCreate{Title: "Secret title", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Baseline: the member can see the item, and the response carries an
	// access fingerprint.
	rr := getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-changes?since=0", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("baseline items-changes: %d: %s", rr.Code, rr.Body.String())
	}
	var baseline itemsChangesResponse
	parseJSON(t, rr, &baseline)
	seen := false
	for _, c := range baseline.Changes {
		if c.ID == item.ID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("member should see the item at baseline")
	}
	if baseline.AccessEpoch == "" {
		t.Fatalf("baseline response carries no access_epoch")
	}
	baseCursor := baseline.Cursor
	baseEpoch := baseline.AccessEpoch

	// Revoke access to the collection holding the item. NOTHING is written to
	// any item: this is the ordinary shape of revocation, and it is exactly
	// the case the delta stream cannot express.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{kept.ID}); err != nil {
		t.Fatalf("revoke collection access: %v", err)
	}

	rr = getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-changes?since="+baseCursor, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("post-revocation items-changes: %d: %s", rr.Code, rr.Body.String())
	}
	var delta itemsChangesResponse
	parseJSON(t, rr, &delta)

	// The mechanism, true before AND after the fix: no row-level signal exists
	// for this revocation. Asserting it is what makes the fingerprint's job
	// legible — there is nothing else in this response that could evict.
	for _, c := range delta.Changes {
		if c.ID == item.ID {
			t.Errorf("unexpected row-level eviction signal for the revoked item: moved_out=%v deleted=%v", c.MovedOut, c.Deleted)
		}
	}

	// The fix: the fingerprint must have moved.
	if delta.AccessEpoch == "" {
		t.Fatalf("post-revocation response carries no access_epoch")
	}
	if delta.AccessEpoch == baseEpoch {
		t.Errorf("access_epoch unchanged across a revocation (%q) — the client has no signal to resync", delta.AccessEpoch)
	}

	// Both doors must agree, or a cold bootstrap and a delta poll would
	// disagree about the caller's scope and flap the resync.
	rr = getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-index", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("items-index: %d: %s", rr.Code, rr.Body.String())
	}
	var index itemsIndexResponse
	parseJSON(t, rr, &index)
	if index.AccessEpoch != delta.AccessEpoch {
		t.Errorf("access_epoch disagrees across doors: /items-index %q vs /items-changes %q", index.AccessEpoch, delta.AccessEpoch)
	}
	for _, it := range index.Items {
		if it.ID == item.ID {
			t.Errorf("revoked item still returned by /items-index")
		}
	}
}
