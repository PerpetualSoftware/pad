package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3428 phase 2 (lead ruling, day 86: option A). Deleting a collection
// writes no item rows, and an unrestricted caller's access_epoch was the
// constant "all", so a warm client was never told to drop the deleted
// collection's rows. The epoch now fingerprints the LIVE collection set for
// every caller, and /items-index and /items-changes leave those rows out, so
// the client's existing resync evicts them.

type epochFixture struct {
	srv          *Server
	ws           *models.Workspace
	token        string
	user         *models.User
	live, doomed *models.Item
	doomColl     *models.Collection
}

// newEpochFixture builds a workspace with a live and a doomed collection, one
// item in each, and a session for a member whose access is either "all"
// (unrestricted) or "specific" over both collections (restricted, no grants).
func newEpochFixture(t *testing.T, restricted bool) epochFixture {
	t.Helper()
	srv := testServer(t)
	owner, err := srv.store.CreateUser(models.UserCreate{Email: "owner@example.com", Name: "Owner", Password: "correct-horse-battery-staple", Role: "admin"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	member, err := srv.store.CreateUser(models.UserCreate{Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "LiveSetEpoch", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`
	liveColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Kept", Slug: "kept", Prefix: "KEP", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	doomColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Doomed", Slug: "doomed", Prefix: "DOO", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if restricted {
		if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{liveColl.ID, doomColl.ID}); err != nil {
			t.Fatal(err)
		}
	}
	live, err := srv.store.CreateItem(ws.ID, liveColl.ID, models.ItemCreate{Title: "Live row", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	doomed, err := srv.store.CreateItem(ws.ID, doomColl.ID, models.ItemCreate{Title: "Cached row", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	token, err := srv.store.CreateSession(member.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	return epochFixture{srv: srv, ws: ws, token: token, user: member, live: live, doomed: doomed, doomColl: doomColl}
}

func (f epochFixture) index(t *testing.T) itemsIndexResponse {
	t.Helper()
	rr := getAuthedSession(t, f.srv, "/api/v1/workspaces/"+f.ws.Slug+"/items-index", f.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("items-index: %d: %s", rr.Code, rr.Body.String())
	}
	var out itemsIndexResponse
	parseJSON(t, rr, &out)
	return out
}

func (f epochFixture) delta(t *testing.T) itemsChangesResponse {
	t.Helper()
	rr := getAuthedSession(t, f.srv, "/api/v1/workspaces/"+f.ws.Slug+"/items-changes?since=0", f.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("items-changes: %d: %s", rr.Code, rr.Body.String())
	}
	var out itemsChangesResponse
	parseJSON(t, rr, &out)
	return out
}

func (f epochFixture) sseEpoch() string {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events?workspace="+f.ws.Slug, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, f.user))
	return f.srv.computeSSEVisibility(req, f.ws.ID).accessEpoch()
}

func runLiveSetEpoch(t *testing.T, restricted bool) {
	f := newEpochFixture(t, restricted)
	has := func(items []models.Item, id string) bool {
		for _, it := range items {
			if it.ID == id {
				return true
			}
		}
		return false
	}
	deltaHas := func(d itemsChangesResponse, id string) bool {
		for _, c := range d.Changes {
			if c.ID == id && !c.Deleted && !c.MovedOut {
				return true
			}
		}
		return false
	}

	before := f.index(t)
	if !has(before.Items, f.doomed.ID) || !has(before.Items, f.live.ID) {
		t.Fatalf("control: the index should hold both rows before the delete")
	}
	if d := f.delta(t); d.AccessEpoch != before.AccessEpoch {
		t.Errorf("doors disagree before the delete: %q vs %q", before.AccessEpoch, d.AccessEpoch)
	}
	if e := f.sseEpoch(); e != before.AccessEpoch {
		t.Errorf("SSE tick and index disagree before the delete: %q vs %q", e, before.AccessEpoch)
	}

	if err := f.srv.store.DeleteCollection(f.doomColl.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}

	after := f.index(t)
	if after.AccessEpoch == before.AccessEpoch {
		t.Errorf("access_epoch unchanged (%q) after a collection was soft-deleted: a warm client keeps its rows", after.AccessEpoch)
	}
	if has(after.Items, f.doomed.ID) {
		t.Errorf("/items-index still serves a soft-deleted collection's row")
	}
	if !has(after.Items, f.live.ID) {
		t.Errorf("/items-index lost the live row")
	}
	d := f.delta(t)
	if d.AccessEpoch != after.AccessEpoch {
		t.Errorf("doors disagree after the delete: %q vs %q", after.AccessEpoch, d.AccessEpoch)
	}
	if deltaHas(d, f.doomed.ID) {
		t.Errorf("/items-changes still serves a soft-deleted collection's row as an upsert")
	}
	if !deltaHas(d, f.live.ID) {
		t.Errorf("/items-changes lost the live row")
	}
	if e := f.sseEpoch(); e != after.AccessEpoch {
		t.Errorf("SSE tick and index disagree after the delete: %q vs %q", e, after.AccessEpoch)
	}
}

func TestAccessEpoch_MovesWhenACollectionIsSoftDeleted_Unrestricted(t *testing.T) {
	runLiveSetEpoch(t, false)
}

func TestAccessEpoch_MovesWhenACollectionIsSoftDeleted_Restricted(t *testing.T) {
	runLiveSetEpoch(t, true)
}

// Lead ruling (2): an open child in a soft-deleted collection no longer blocks
// closing its parent. It is gone from the children list and from progress, and
// a parent nobody can close because of an item nobody can see is the worse
// state. The control is the same child blocking the close before the delete.
func TestOpenChildrenGuard_IgnoresChildrenOfSoftDeletedCollections(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("workspace: %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`
	drafts, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Drafts", Slug: "drafts", Prefix: "DRF", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := seedParentAndChildren(t, srv, slug, nil)
	child, err := srv.store.CreateItem(ws.ID, drafts.ID, models.ItemCreate{Title: "Open draft child", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: plan.ID, LinkType: "parent"}, child.ID); err != nil {
		t.Fatal(err)
	}
	closePlan := func() int {
		rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
			"fields": map[string]interface{}{"status": "completed"},
		})
		return rr.Code
	}
	if code := closePlan(); code != http.StatusConflict {
		t.Fatalf("control: the open child should block closing the plan before the delete, got %d", code)
	}
	if err := srv.store.DeleteCollection(drafts.ID, ""); err != nil {
		t.Fatalf("soft-delete collection: %v", err)
	}
	if code := closePlan(); code != http.StatusOK {
		t.Errorf("an open child in a soft-deleted collection still blocks closing its parent: %d", code)
	}
}

// Codex r1: a door must not pair rows read BEFORE a collection's delete with
// the epoch AFTER it. That response would hold the deleted collection's rows
// under the new epoch, every later poll would agree with it, and nothing would
// evict them. The hook commits the delete right after each door's row query;
// the response's epoch must then still be the pre-delete one, so the next poll
// sees a change and resyncs.
func TestItemDoors_EpochIsNotNewerThanTheirRows(t *testing.T) {
	for _, door := range []string{"items-index", "items-changes"} {
		t.Run(door, func(t *testing.T) {
			f := newEpochFixture(t, false)
			pre := f.index(t).AccessEpoch
			fired := false
			f.srv.itemDoorAfterRowsHook = func() {
				if fired {
					return
				}
				fired = true
				if err := f.srv.store.DeleteCollection(f.doomColl.ID, ""); err != nil {
					t.Errorf("delete in hook: %v", err)
				}
			}
			var gotEpoch string
			var heldDoomed bool
			if door == "items-index" {
				resp := f.index(t)
				gotEpoch = resp.AccessEpoch
				for _, it := range resp.Items {
					heldDoomed = heldDoomed || it.ID == f.doomed.ID
				}
			} else {
				resp := f.delta(t)
				gotEpoch = resp.AccessEpoch
				for _, c := range resp.Changes {
					heldDoomed = heldDoomed || (c.ID == f.doomed.ID && !c.Deleted && !c.MovedOut)
				}
			}
			f.srv.itemDoorAfterRowsHook = nil
			if !fired {
				t.Fatalf("control: the hook never ran")
			}
			if !heldDoomed {
				t.Fatalf("control: the rows were read before the delete, so they should hold the doomed row")
			}
			if gotEpoch != pre {
				t.Errorf("the response pairs pre-delete rows with a post-delete epoch (%q, was %q): a warm client would keep the rows forever", gotEpoch, pre)
			}
			if next := f.index(t).AccessEpoch; next == gotEpoch {
				t.Errorf("the next poll did not show an epoch change, so the client would never resync")
			}
		})
	}
}

func TestItemDoorAfterRowsHookIsNilInProduction(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Skipf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if New(s).itemDoorAfterRowsHook != nil {
		t.Fatal("New left the BUG-3428 test-only item-door hook set")
	}
}

// Codex r2: the same invariant for a grant-filtered caller on /items-changes,
// whose epoch reads the live GRANT set too. A restricted member holds a
// whole-collection grant on the doomed collection plus an item grant
// elsewhere; the hook deletes the doomed collection right after the rows.
func TestItemsChanges_GrantFilteredEpochIsNotNewerThanItsRows(t *testing.T) {
	f := newEpochFixture(t, true)
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, f.user.ID, "specific", []string{f.live.CollectionID}); err != nil {
		t.Fatal(err)
	}
	owner, err := f.srv.store.GetUserByEmail("owner@example.com")
	if err != nil || owner == nil {
		t.Fatalf("owner: %v", err)
	}
	if _, err := f.srv.store.CreateCollectionGrant(f.ws.ID, f.doomColl.ID, f.user.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}
	other, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Other", Slug: "other", Prefix: "OTH"})
	if err != nil {
		t.Fatal(err)
	}
	granted, err := f.srv.store.CreateItem(f.ws.ID, other.ID, models.ItemCreate{Title: "Granted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, granted.ID, f.user.ID, "view", owner.ID); err != nil {
		t.Fatal(err)
	}

	pre := f.delta(t).AccessEpoch
	fired := false
	f.srv.itemDoorAfterRowsHook = func() {
		if !fired {
			fired = true
			if err := f.srv.store.DeleteCollection(f.doomColl.ID, ""); err != nil {
				t.Errorf("delete in hook: %v", err)
			}
		}
	}
	resp := f.delta(t)
	f.srv.itemDoorAfterRowsHook = nil
	held := false
	for _, c := range resp.Changes {
		held = held || (c.ID == f.doomed.ID && !c.Deleted && !c.MovedOut)
	}
	if !fired || !held {
		t.Fatalf("control: hook fired=%v, pre-delete rows held the doomed row=%v", fired, held)
	}
	if resp.AccessEpoch != pre {
		t.Errorf("a grant-filtered delta pairs pre-delete rows with a post-delete epoch (%q, was %q)", resp.AccessEpoch, pre)
	}
	if next := f.delta(t).AccessEpoch; next == resp.AccessEpoch {
		t.Errorf("the next poll did not show an epoch change, so the client would never resync")
	}
}
