package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestAccessEpoch_TracksTheEffectiveSetForItemGrantCallers is the BUG-3347
// repro.
//
// A restricted member holding item grants is served by the EFFECTIVE set: the
// item doors swap their collection filter to guestResourceFilter's
// fullCollIDs, which drops a soft-deleted collection's whole-collection grant
// (BUG-3333). access_epoch hashed the NAV-visible set instead, which keeps
// that grant (it stays revocable). So soft-deleting the granted collection
// removed its rows from both doors and left the epoch where it was, and a
// warm client was never told to drop the rows it had cached.
//
// The fixture is the one that discriminates: a whole-collection grant on C
// (the deleted one) plus a live item grant on D. Without the item grant the
// doors filter by the nav set, the epoch already agrees with what they serve,
// and the test would pass on the old code for the wrong reason.
func TestAccessEpoch_TracksTheEffectiveSetForItemGrantCallers(t *testing.T) {
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "EffectiveEpoch", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	kept, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Kept", Slug: "kept", Prefix: "KEP", Schema: schema})
	if err != nil {
		t.Fatalf("create kept: %v", err)
	}
	doomed, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Doomed", Slug: "doomed", Prefix: "DOO", Schema: schema})
	if err != nil {
		t.Fatalf("create doomed: %v", err)
	}
	other, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Other", Slug: "other", Prefix: "OTH", Schema: schema})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{kept.ID}); err != nil {
		t.Fatalf("set collection access: %v", err)
	}
	if _, err := srv.store.CreateCollectionGrant(ws.ID, doomed.ID, member.ID, "view", owner.ID); err != nil {
		t.Fatalf("grant doomed collection: %v", err)
	}
	cached, err := srv.store.CreateItem(ws.ID, doomed.ID, models.ItemCreate{Title: "Cached row", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("create doomed item: %v", err)
	}
	granted, err := srv.store.CreateItem(ws.ID, other.ID, models.ItemCreate{Title: "Granted", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("create granted item: %v", err)
	}
	if _, err := srv.store.CreateItemGrant(ws.ID, granted.ID, member.ID, "view", owner.ID); err != nil {
		t.Fatalf("create item grant: %v", err)
	}
	token, err := srv.store.CreateSession(member.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	index := func(when string) itemsIndexResponse {
		t.Helper()
		rr := getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-index", token)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s items-index: %d: %s", when, rr.Code, rr.Body.String())
		}
		var out itemsIndexResponse
		parseJSON(t, rr, &out)
		return out
	}
	deltaEpoch := func(when string) string {
		t.Helper()
		rr := getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-changes?since=0", token)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s items-changes: %d: %s", when, rr.Code, rr.Body.String())
		}
		var out itemsChangesResponse
		parseJSON(t, rr, &out)
		return out.AccessEpoch
	}
	sseEpoch := func() string {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+ws.Slug, nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, member))
		return srv.computeSSEVisibility(req, ws.ID).accessEpoch()
	}
	holds := func(items []models.Item, id string) bool {
		for _, it := range items {
			if it.ID == id {
				return true
			}
		}
		return false
	}

	before := index("before delete")
	// Control: the fixture reaches the swapped filter and serves the row the
	// client will go on to cache.
	if !holds(before.Items, cached.ID) || !holds(before.Items, granted.ID) {
		t.Fatalf("baseline index should hold both the doomed row and the granted row")
	}
	if got := deltaEpoch("before delete"); got != before.AccessEpoch {
		t.Errorf("doors disagree before the delete: /items-index %q vs /items-changes %q", before.AccessEpoch, got)
	}
	if got := sseEpoch(); got != before.AccessEpoch {
		t.Errorf("SSE tick and /items-index disagree before the delete: %q vs %q", got, before.AccessEpoch)
	}

	if err := srv.store.DeleteCollection(doomed.ID, ""); err != nil {
		t.Fatalf("soft-delete doomed collection: %v", err)
	}

	after := index("after delete")
	// The mechanism: the door already stops serving the row. True on the old
	// code too; it is what makes an unchanged epoch a stale cache.
	if holds(after.Items, cached.ID) {
		t.Fatalf("index still serves a row from the soft-deleted collection")
	}
	if !holds(after.Items, granted.ID) {
		t.Fatalf("index lost the live granted row")
	}
	// The fix.
	if after.AccessEpoch == before.AccessEpoch {
		t.Errorf("access_epoch unchanged (%q) after the caller's effective set lost a collection: "+
			"a warm client keeps the deleted collection's cached rows", after.AccessEpoch)
	}
	if got := deltaEpoch("after delete"); got != after.AccessEpoch {
		t.Errorf("doors disagree after the delete: /items-index %q vs /items-changes %q", after.AccessEpoch, got)
	}
	if got := sseEpoch(); got != after.AccessEpoch {
		t.Errorf("SSE tick and /items-index disagree after the delete: %q vs %q", got, after.AccessEpoch)
	}
}
