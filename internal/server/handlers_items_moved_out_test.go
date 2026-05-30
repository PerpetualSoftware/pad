package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// getAuthedSession issues a GET as a logged-in user via a session
// cookie (GET needs no CSRF). The UA + IP match CreateSession so the
// session-binding middleware accepts it.
func getAuthedSession(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("User-Agent", testSessionUA)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

// TestItemsChanges_MovedOutTombstoneForRestrictedMember is the
// end-to-end proof for BUG-1675: a member who can see the source
// collection but not the target receives a moved_out tombstone on
// /items-changes when an item moves out of their visibility, so their
// local cache evicts it instead of keeping a now-unauthorized row.
func TestItemsChanges_MovedOutTombstoneForRestrictedMember(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "MovedOutWS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "owner"); err != nil {
		t.Fatalf("add admin member: %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	visible, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Visible", Slug: "visible", Prefix: "VIS", Schema: schema})
	if err != nil {
		t.Fatalf("create visible: %v", err)
	}
	hidden, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Hidden", Slug: "hidden", Prefix: "HID", Schema: schema})
	if err != nil {
		t.Fatalf("create hidden: %v", err)
	}

	// Restricted member: editor, but collection access limited to "visible".
	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "member@example.com", Name: "Member", Password: "correct-horse-battery-staple", Role: "member",
	})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{visible.ID}); err != nil {
		t.Fatalf("set member collection access: %v", err)
	}
	token, err := srv.store.CreateSession(member.ID, "test", "192.0.2.1", testSessionUA, webSessionTTL)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	item, err := srv.store.CreateItem(ws.ID, visible.ID, models.ItemCreate{Title: "Will move out", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	// Baseline delta: the member sees the item in the visible collection.
	rr := getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-changes?since=0", token)
	if rr.Code != http.StatusOK {
		t.Fatalf("baseline items-changes: %d: %s", rr.Code, rr.Body.String())
	}
	var baseline itemsChangesResponse
	parseJSON(t, rr, &baseline)
	foundBaseline := false
	for _, c := range baseline.Changes {
		if c.ID == item.ID {
			foundBaseline = true
			if c.MovedOut {
				t.Error("baseline row should not be moved_out")
			}
		}
	}
	if !foundBaseline {
		t.Fatalf("member should see the item in their visible collection at baseline")
	}
	baseCursor := baseline.Cursor

	// Move the item into the hidden collection + log the move activity
	// (mirrors what the move/bulk handlers do).
	moved, err := srv.store.MoveItem(item.ID, hidden.ID, `{"status":"open"}`)
	if err != nil {
		t.Fatalf("move item: %v", err)
	}
	if _, err := srv.store.CreateActivity(models.Activity{
		WorkspaceID: ws.ID, DocumentID: item.ID, Action: "moved",
		Metadata: `{"from_collection":"visible","to_collection":"hidden"}`,
	}); err != nil {
		t.Fatalf("log move activity: %v", err)
	}

	// Delta since the baseline cursor: the member must get a moved_out
	// tombstone (id + seq only, no destination data).
	rr = getAuthedSession(t, srv, "/api/v1/workspaces/"+ws.Slug+"/items-changes?since="+baseCursor, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("delta items-changes: %d: %s", rr.Code, rr.Body.String())
	}
	var delta itemsChangesResponse
	parseJSON(t, rr, &delta)

	var tombstone *itemChangeRow
	for i := range delta.Changes {
		if delta.Changes[i].ID == item.ID {
			tombstone = &delta.Changes[i]
		}
	}
	if tombstone == nil {
		t.Fatalf("expected a moved_out tombstone for the item; changes=%+v", delta.Changes)
	}
	if !tombstone.MovedOut {
		t.Errorf("row should be marked moved_out")
	}
	if tombstone.Seq != moved.Seq {
		t.Errorf("tombstone seq = %d, want %d (post-move)", tombstone.Seq, moved.Seq)
	}
	// No destination data leaked.
	if tombstone.Title != "" || tombstone.CollectionSlug != "" {
		t.Errorf("moved_out row must not carry title/collection (leak): title=%q coll=%q", tombstone.Title, tombstone.CollectionSlug)
	}
}
