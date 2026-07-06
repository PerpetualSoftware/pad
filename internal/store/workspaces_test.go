package store

import (
	"database/sql"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestRestoreWorkspace_ResurfacesIntact proves the core PLAN-1969 fact:
// DeleteWorkspace only soft-deletes the WORKSPACE row — items and
// collections underneath are never touched, just transitively hidden.
// RestoreWorkspace clears deleted_at and everything comes back intact.
func TestRestoreWorkspace_ResurfacesIntact(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "owner@test.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Recoverable", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Keep Me", "body")

	// Soft-delete the workspace.
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}

	// The workspace is now hidden from the normal resolver...
	if got, err := s.GetWorkspaceBySlug(ws.Slug); err != nil {
		t.Fatalf("GetWorkspaceBySlug after delete: %v", err)
	} else if got != nil {
		t.Fatalf("expected soft-deleted workspace to be hidden from GetWorkspaceBySlug, got %+v", got)
	}

	// ...but its collection and item were NEVER soft-deleted — only the
	// workspace row carries deleted_at.
	if n := s.countRows(t, `SELECT COUNT(*) FROM collections WHERE id = ? AND deleted_at IS NULL`, coll.ID); n != 1 {
		t.Errorf("collection should remain live (deleted_at NULL) after workspace delete, got %d", n)
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM items WHERE id = ? AND deleted_at IS NULL`, item.ID); n != 1 {
		t.Errorf("item should remain live (deleted_at NULL) after workspace delete, got %d", n)
	}

	// Restore.
	if err := s.RestoreWorkspace(ws.Slug); err != nil {
		t.Fatalf("RestoreWorkspace: %v", err)
	}

	restored, err := s.GetWorkspaceBySlug(ws.Slug)
	if err != nil {
		t.Fatalf("GetWorkspaceBySlug after restore: %v", err)
	}
	if restored == nil {
		t.Fatal("expected workspace to be visible again after restore")
	}
	if restored.DeletedAt != nil {
		t.Errorf("restored workspace deleted_at should be nil, got %v", restored.DeletedAt)
	}

	// The item is retrievable through the normal getter again.
	gotItem, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem after restore: %v", err)
	}
	if gotItem == nil || gotItem.Title != "Keep Me" {
		t.Errorf("expected item to re-surface intact, got %+v", gotItem)
	}
	if n := s.countRows(t, `SELECT COUNT(*) FROM collections WHERE id = ? AND deleted_at IS NULL`, coll.ID); n != 1 {
		t.Errorf("collection should still be live after restore, got %d", n)
	}
}

// TestRestoreWorkspace_ErrNoRowsWhenNothingToRestore covers the
// idempotent-ish 404 semantics: restoring a live workspace, restoring
// twice, and restoring an unknown slug all return sql.ErrNoRows.
func TestRestoreWorkspace_ErrNoRowsWhenNothingToRestore(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Email: "owner@test.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Live One", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	// Restoring a live (never-deleted) workspace matches no soft-deleted row.
	if err := s.RestoreWorkspace(ws.Slug); err != sql.ErrNoRows {
		t.Errorf("restore of live workspace: expected sql.ErrNoRows, got %v", err)
	}

	// Delete, then the first restore succeeds.
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}
	if err := s.RestoreWorkspace(ws.Slug); err != nil {
		t.Fatalf("first RestoreWorkspace should succeed: %v", err)
	}

	// A second restore (now live again) is a no-op → ErrNoRows.
	if err := s.RestoreWorkspace(ws.Slug); err != sql.ErrNoRows {
		t.Errorf("double restore: expected sql.ErrNoRows, got %v", err)
	}

	// Unknown slug → ErrNoRows.
	if err := s.RestoreWorkspace("does-not-exist"); err != sql.ErrNoRows {
		t.Errorf("restore of unknown slug: expected sql.ErrNoRows, got %v", err)
	}
}

// TestListDeletedWorkspaces_WindowAndOwnerScope pins the 30-day restore
// window boundary (29d IN, 31d OUT — the inverse of the purge boundary),
// owner-scoping, live exclusion, and most-recently-deleted ordering.
func TestListDeletedWorkspaces_WindowAndOwnerScope(t *testing.T) {
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{Email: "owner@test.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(models.UserCreate{Email: "other@test.com", Name: "Other", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}

	// Owner's workspaces.
	inRecent := createWSWithDeletedAt(t, s, owner, "In Recent", time.Now().Add(-2*24*time.Hour))
	inOld := createWSWithDeletedAt(t, s, owner, "In Old", time.Now().Add(-29*24*time.Hour))
	out := createWSWithDeletedAt(t, s, owner, "Out", time.Now().Add(-31*24*time.Hour))
	liveWS, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Live", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create live workspace: %v", err)
	}

	// Another user's soft-deleted workspace — must never leak into the
	// owner's list (owner-scoping; also the account-deleted-no-owner case).
	otherWS := createWSWithDeletedAt(t, s, other, "Other Deleted", time.Now().Add(-1*24*time.Hour))

	cutoff := time.Now().Add(-30 * 24 * time.Hour)
	got, err := s.ListDeletedWorkspaces(owner.ID, cutoff)
	if err != nil {
		t.Fatalf("ListDeletedWorkspaces(owner): %v", err)
	}

	ids := map[string]bool{}
	for _, w := range got {
		ids[w.ID] = true
		if w.DeletedAt == nil {
			t.Errorf("deleted workspace %s should carry deleted_at", w.Slug)
		}
	}
	if !ids[inRecent] {
		t.Errorf("workspace deleted 2d ago should be restorable")
	}
	if !ids[inOld] {
		t.Errorf("workspace deleted 29d ago should be restorable (boundary IN)")
	}
	if ids[out] {
		t.Errorf("workspace deleted 31d ago must NOT be restorable (boundary OUT)")
	}
	if ids[liveWS.ID] {
		t.Errorf("live workspace must NEVER appear in the deleted list")
	}
	if ids[otherWS] {
		t.Errorf("another user's deleted workspace must not leak (owner-scoping)")
	}
	if len(got) != 2 {
		t.Fatalf("expected exactly 2 restorable workspaces for owner, got %d", len(got))
	}

	// Ordering: most-recently-deleted first.
	if got[0].ID != inRecent || got[1].ID != inOld {
		t.Errorf("expected DESC deleted_at ordering [inRecent, inOld], got [%s, %s]", got[0].ID, got[1].ID)
	}

	// The other user sees only their own deleted workspace.
	gotOther, err := s.ListDeletedWorkspaces(other.ID, cutoff)
	if err != nil {
		t.Fatalf("ListDeletedWorkspaces(other): %v", err)
	}
	if len(gotOther) != 1 || gotOther[0].ID != otherWS {
		t.Errorf("other user should see exactly their own deleted workspace, got %+v", gotOther)
	}
}
