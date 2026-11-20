package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4684: the item update activity records the assignee id, so the
// needs-you assignment time survives the assignee renaming themselves and a
// reassignment between two people with the same display name is recorded.

func task4684Patch(t *testing.T, f *nfx, ws *models.Workspace, it *models.Item, as *models.User, body map[string]any) {
	t.Helper()
	cookie, err := f.srv.store.CreateSession(as.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(f.srv, "PATCH", "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug, body, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("PATCH %s: %d %s", it.Slug, rr.Code, rr.Body.String())
	}
}

// assignmentActivities returns the item's "updated" activity metadata that
// mention an assignment, oldest first, with their timestamps.
func task4684AssignmentRows(t *testing.T, srv *Server, itemID string) ([]map[string]any, []time.Time) {
	t.Helper()
	acts, err := srv.store.ListDocumentActivity(itemID, models.ActivityListParams{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var metas []map[string]any
	var ats []time.Time
	for i := len(acts) - 1; i >= 0; i-- { // newest first -> oldest first
		a := acts[i]
		if a.Action != "updated" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(a.Metadata), &m) != nil {
			continue
		}
		if _, ok := m[store.ActivityMetaAssignedUserID]; !ok {
			continue
		}
		metas = append(metas, m)
		ats = append(ats, a.CreatedAt)
	}
	return metas, ats
}

func TestTASK4684_AssignmentTimeSurvivesRename(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, alice := f.user("owner"), f.user("alice")
		f.user("outsider") // a real user, but not a member of the workspace
		ws := f.workspace("One", owner)
		f.member(ws, alice, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "handed over", nil, nfxDay(0))

		task4684Patch(t, f, ws, it, owner, map[string]any{"assigned_user_id": alice.ID})
		metas, ats := task4684AssignmentRows(t, srv, it.ID)
		if len(metas) != 1 || metas[0][store.ActivityMetaAssignedUserID] != alice.ID {
			t.Fatalf("assignment activity = %v, want one stamped %s", metas, alice.ID)
		}
		assignedAt := ats[0]
		// A mention nobody in the workspace can answer makes the item a
		// candidate, and it falls to the assignee (as in
		// TestNeedsMe_RoutingRulesAndWaitingSince). It comes after the
		// assignment, so the assignment is the earliest claim.
		f.comment(ws, it, owner, "@outsider-nx can you look?")

		renamed := "Alice Renamed"
		if _, err := srv.store.UpdateUser(alice.ID, models.UserUpdate{Name: &renamed}); err != nil {
			t.Fatal(err)
		}

		resp := f.need(alice, "")
		wantTitles(t, "alice", resp.Rows, "handed over")
		got := resp.Rows[0].WaitingSince
		if got.Equal(nfxDay(0)) {
			t.Fatalf("WaitingSince fell back to created_at %v after the rename", got)
		}
		if !got.Equal(assignedAt) {
			t.Fatalf("WaitingSince = %v, want the assignment time %v", got, assignedAt)
		}
	})
}

func TestTASK4684_SameNameReassignIsRecorded(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner := f.user("owner")
		mk := func(username string) *models.User {
			u, err := srv.store.CreateUser(models.UserCreate{Email: username + "@needs.test", Name: "Sam Same", Username: username, Password: "pw-test-12345"})
			if err != nil {
				t.Fatal(err)
			}
			return u
		}
		sam1, sam2 := mk("sam-one"), mk("sam-two")
		ws := f.workspace("One", owner)
		f.member(ws, sam1, "editor")
		f.member(ws, sam2, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "namesake", sam1, nfxDay(0))

		task4684Patch(t, f, ws, it, owner, map[string]any{"assigned_user_id": sam2.ID})
		metas, _ := task4684AssignmentRows(t, srv, it.ID)
		if len(metas) != 1 {
			t.Fatalf("assignment activities = %v, want one", metas)
		}
		if got := metas[0][store.ActivityMetaAssignedUserID]; got != sam2.ID {
			t.Fatalf("%s = %v, want %s", store.ActivityMetaAssignedUserID, got, sam2.ID)
		}
		if got := metas[0]["changes"]; got != "assigned: Sam Same → Sam Same" {
			t.Fatalf("changes = %q", got)
		}
		// A write that leaves the assignee alone stamps nothing (its own item,
		// so the debounce cannot fold it into the row above).
		kept := f.item(ws, tasks, "kept", sam1, nfxDay(0))
		task4684Patch(t, f, ws, kept, owner, map[string]any{"title": "kept renamed"})
		if metas, _ := task4684AssignmentRows(t, srv, kept.ID); len(metas) != 0 {
			t.Fatalf("an update that kept the assignee stamped %v", metas)
		}
	})
}
