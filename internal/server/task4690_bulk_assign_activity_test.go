package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-4690: a bulk assign records the same "assigned: <before> → <after>"
// change text and assignee id stamp the single PATCH does (TASK-4684), so
// the needs-you assignment time sees a bulk assignment instead of falling
// back to the item's created_at.

func task4690Bulk(t *testing.T, f *nfx, ws *models.Workspace, as *models.User, body map[string]any) {
	t.Helper()
	cookie, err := f.srv.store.CreateSession(as.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(f.srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/items/bulk", body, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST bulk: %d %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Failed []json.RawMessage `json:"failed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse bulk response: %v\n%s", err, rr.Body.String())
	}
	if len(resp.Failed) != 0 {
		t.Fatalf("bulk failed rows: %s", rr.Body.String())
	}
}

// task4690UpdatedMetas returns every "updated" activity's metadata, oldest
// first, whether or not it carries the assignee stamp.
func task4690UpdatedMetas(t *testing.T, srv *Server, itemID string) []map[string]any {
	t.Helper()
	acts, err := srv.store.ListDocumentActivity(itemID, models.ActivityListParams{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for i := len(acts) - 1; i >= 0; i-- {
		if acts[i].Action != "updated" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(acts[i].Metadata), &m); err != nil {
			t.Fatalf("activity metadata %q: %v", acts[i].Metadata, err)
		}
		out = append(out, m)
	}
	return out
}

func TestTASK4690_BulkAssignStampsEachItem(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, bob := f.user("owner"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		a := f.item(ws, tasks, "first", nil, nfxDay(0))
		b := f.item(ws, tasks, "second", nil, nfxDay(0))

		task4690Bulk(t, f, ws, owner, map[string]any{"op": "assign", "ids": []string{a.ID, b.ID}, "assigned_user_id": bob.ID})

		for _, it := range []*models.Item{a, b} {
			metas, _ := task4684AssignmentRows(t, srv, it.ID)
			if len(metas) != 1 {
				t.Fatalf("%s: assignment activities = %v, want one", it.Title, metas)
			}
			if got := metas[0][store.ActivityMetaAssignedUserID]; got != bob.ID {
				t.Fatalf("%s: %s = %v, want %s", it.Title, store.ActivityMetaAssignedUserID, got, bob.ID)
			}
			if got, want := metas[0]["changes"], "assigned: (none) → "+bob.Name; got != want {
				t.Fatalf("%s: changes = %q, want %q", it.Title, got, want)
			}
			if got := metas[0]["bulk_op"]; got != "assign" {
				t.Fatalf("%s: bulk_op = %v, want assign", it.Title, got)
			}
		}
	})
}

func TestTASK4690_BulkUnassignStampsEmpty(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, bob := f.user("owner"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "dropped", bob, nfxDay(0))

		task4690Bulk(t, f, ws, owner, map[string]any{"op": "assign", "ids": []string{it.ID}, "clear_assigned_user": true})

		metas, _ := task4684AssignmentRows(t, srv, it.ID)
		if len(metas) != 1 {
			t.Fatalf("assignment activities = %v, want one", metas)
		}
		if got, ok := metas[0][store.ActivityMetaAssignedUserID]; !ok || got != "" {
			t.Fatalf("%s = %v (present %v), want \"\"", store.ActivityMetaAssignedUserID, got, ok)
		}
		if got, want := metas[0]["changes"], "assigned: "+bob.Name+" → (none)"; got != want {
			t.Fatalf("changes = %q, want %q", got, want)
		}
	})
}

func TestTASK4690_BulkAssignToSameUserStampsNothing(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, bob := f.user("owner"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "already", bob, nfxDay(0))

		task4690Bulk(t, f, ws, owner, map[string]any{"op": "assign", "ids": []string{it.ID}, "assigned_user_id": bob.ID})

		metas := task4690UpdatedMetas(t, srv, it.ID)
		if len(metas) != 1 {
			t.Fatalf("updated activities = %v, want the one bulk row", metas)
		}
		if _, ok := metas[0][store.ActivityMetaAssignedUserID]; ok {
			t.Fatalf("a no-op bulk assign stamped %v", metas[0])
		}
		if _, ok := metas[0]["changes"]; ok {
			t.Fatalf("a no-op bulk assign wrote changes %v", metas[0])
		}
		if got := metas[0]["bulk_op"]; got != "assign" {
			t.Fatalf("bulk_op = %v, want assign", got)
		}
	})
}

func TestTASK4690_BulkAssignSetsNeedsYouWaitingSince(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, bob := f.user("owner"), f.user("bob")
		f.user("outsider") // a real user, but not a member of the workspace
		ws := f.workspace("One", owner)
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "handed over in bulk", nil, nfxDay(0))

		task4690Bulk(t, f, ws, owner, map[string]any{"op": "assign", "ids": []string{it.ID}, "assigned_user_id": bob.ID})
		metas, ats := task4684AssignmentRows(t, srv, it.ID)
		if len(metas) != 1 || metas[0][store.ActivityMetaAssignedUserID] != bob.ID {
			t.Fatalf("assignment activity = %v, want one stamped %s", metas, bob.ID)
		}
		assignedAt := ats[0]
		if !assignedAt.After(nfxDay(0)) {
			t.Fatalf("assignment time %v is not after created_at %v", assignedAt, nfxDay(0))
		}
		// An unanswerable mention makes the item a candidate that falls to the
		// assignee, as in TestTASK4684_AssignmentTimeSurvivesRename.
		f.comment(ws, it, owner, "@outsider-nx can you look?")

		resp := f.need(bob, "")
		wantTitles(t, "bob", resp.Rows, "handed over in bulk")
		got := resp.Rows[0].WaitingSince
		if got.Equal(nfxDay(0)) {
			t.Fatalf("WaitingSince fell back to created_at %v after a bulk assign", got)
		}
		if !got.Equal(assignedAt) {
			t.Fatalf("WaitingSince = %v, want the bulk assignment time %v", got, assignedAt)
		}
	})
}

func TestTASK4690_BulkAssignRecordsRoleChange(t *testing.T) {
	t.Parallel()
	needsBackends(t, func(t *testing.T, srv *Server) {
		f := newNfx(t, srv)
		owner, bob := f.user("owner"), f.user("bob")
		ws := f.workspace("One", owner)
		f.member(ws, bob, "editor")
		tasks := f.collection(ws, "Tasks", false)
		it := f.item(ws, tasks, "roled", nil, nfxDay(0))
		role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Reviewer"})
		if err != nil {
			t.Fatal(err)
		}

		task4690Bulk(t, f, ws, owner, map[string]any{"op": "assign", "ids": []string{it.ID}, "assigned_user_id": bob.ID, "agent_role_id": role.ID})

		metas, _ := task4684AssignmentRows(t, srv, it.ID)
		if len(metas) != 1 {
			t.Fatalf("assignment activities = %v, want one", metas)
		}
		want := "role: (none) → Reviewer; assigned: (none) → " + bob.Name
		if got := metas[0]["changes"]; got != want {
			t.Fatalf("changes = %q, want %q", got, want)
		}

		// A role-only bulk assign writes the role text and no assignee stamp.
		other := f.item(ws, tasks, "role only", bob, nfxDay(0))
		task4690Bulk(t, f, ws, owner, map[string]any{"op": "assign", "ids": []string{other.ID}, "agent_role_id": role.ID})
		ometas := task4690UpdatedMetas(t, srv, other.ID)
		if len(ometas) != 1 {
			t.Fatalf("updated activities = %v, want one", ometas)
		}
		if got := ometas[0]["changes"]; got != "role: (none) → Reviewer" {
			t.Fatalf("role-only changes = %q", got)
		}
		if _, ok := ometas[0][store.ActivityMetaAssignedUserID]; ok {
			t.Fatalf("a role-only bulk assign stamped the assignee: %v", ometas[0])
		}
	})
}
