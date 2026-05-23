package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestGetUserWorkspacesDetailed seeds a workspace owned by a user with:
//   - two user-facing collections, one system collection (counted as 0)
//   - three items: one open (status=open), one done (terminal), one different collection
//   - one attachment
//   - one extra member
//
// And verifies all six aggregations land correctly.
// PLAN-1542 / TASK-1545.
func TestGetUserWorkspacesDetailed(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner@example.com", "Owner", "password123")
	guest := createTestUser(t, s, "guest@example.com", "Guest", "password123")

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Acme", Slug: "acme", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// In production the handler adds the owner to workspace_members at
	// creation time (see internal/server/handlers_workspaces.go:256). Mimic
	// that here so the membership-based query under test returns the row.
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner as member: %v", err)
	}

	// Two user-facing collections. createTestCollection seeds a select-status schema.
	tasks := createTestCollection(t, s, ws.ID, "Tasks")
	createTestCollection(t, s, ws.ID, "Ideas")

	// System collection (should be excluded from collections_count).
	if _, err := s.db.Exec(s.q(`
		INSERT INTO collections (id, workspace_id, name, slug, prefix, icon, description, schema, settings, sort_order, is_default, is_system, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), newID(), ws.ID, "Conventions", "conventions", "CONVE", "", "", `{"fields":[]}`, `{}`, 0, false, true, now(), now()); err != nil {
		t.Fatalf("seed system collection: %v", err)
	}

	// Three items in the Tasks collection: open + done + a second open one
	// just to verify the "open" predicate filters terminals.
	_, _ = s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Open A", Fields: `{"status":"open"}`})
	_, _ = s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Open B", Fields: `{"status":"open"}`})
	// "done" is in the hardcoded terminal set; this counts as items_total but not items_open.
	_, _ = s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "Done", Fields: `{"status":"done"}`})

	// Workspace_members: owner is auto-membered by CreateWorkspace; add guest.
	if err := s.AddWorkspaceMember(ws.ID, guest.ID, "editor"); err != nil {
		t.Fatalf("add guest member: %v", err)
	}

	// Attachment row: 250 bytes.
	if _, err := s.db.Exec(s.q(`
		INSERT INTO attachments (id, workspace_id, item_id, uploaded_by, storage_key, content_hash, mime_type, size_bytes, filename, created_at)
		VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?)
	`), newID(), ws.ID, owner.ID, "k", "h", "image/png", int64(250), "x.png", now()); err != nil {
		t.Fatalf("seed attachment: %v", err)
	}

	// As owner: should see one workspace with the right aggregations.
	got, err := s.GetUserWorkspacesDetailed(owner.ID)
	if err != nil {
		t.Fatalf("GetUserWorkspacesDetailed: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 workspace; got %d", len(got))
	}
	w := got[0]
	if w.CollectionsCount != 2 {
		t.Errorf("collections_count (excl system): want 2 got %d", w.CollectionsCount)
	}
	if w.ItemsOpen != 2 {
		t.Errorf("items_open (excl terminals): want 2 got %d", w.ItemsOpen)
	}
	if w.ItemsTotal != 3 {
		t.Errorf("items_total: want 3 got %d", w.ItemsTotal)
	}
	if w.MembersCount != 2 {
		t.Errorf("members_count (owner + guest): want 2 got %d", w.MembersCount)
	}
	if w.StorageBytes != 250 {
		t.Errorf("storage_bytes: want 250 got %d", w.StorageBytes)
	}
	if w.LastActivityAt == "" {
		t.Errorf("last_activity_at: want non-empty (items exist), got empty")
	}

	// Guest sees the same workspace via membership.
	got, err = s.GetUserWorkspacesDetailed(guest.ID)
	if err != nil {
		t.Fatalf("guest view: %v", err)
	}
	if len(got) != 1 || got[0].WorkspaceID != ws.ID {
		t.Fatalf("guest should see acme; got %+v", got)
	}

	// User with no memberships gets [].
	loner := createTestUser(t, s, "loner@example.com", "Loner", "password123")
	got, err = s.GetUserWorkspacesDetailed(loner.ID)
	if err != nil {
		t.Fatalf("loner view: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("loner should see no workspaces; got %d", len(got))
	}
}
