package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3535: the admin items_open count leaves out the open items of a
// REFERENCE collection, as every other open-work count does, and still
// counts them in items_total.
func TestPLAN3535_AdminItemsOpenLeavesOutReference(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := createTestUser(t, s, "owner3535@example.com", "Owner", "password123")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Acme", Slug: "acme3535", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	tasks := createTestCollection(t, s, ws.ID, "Tasks")
	notes := createTestCollection(t, s, ws.ID, "Notes")
	for _, c := range []string{tasks.ID, notes.ID} {
		if _, err := s.CreateItem(ws.ID, c, models.ItemCreate{Title: "Open", Fields: `{"status":"open"}`}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() AdminUserWorkspaceDetail {
		t.Helper()
		got, err := s.GetUserWorkspacesDetailed(owner.ID)
		if err != nil || len(got) != 1 {
			t.Fatalf("GetUserWorkspacesDetailed: %v (%d rows)", err, len(got))
		}
		return got[0]
	}
	if w := read(); w.ItemsOpen != 2 || w.ItemsTotal != 2 {
		t.Fatalf("control, both work: open=%d total=%d, want 2/2", w.ItemsOpen, w.ItemsTotal)
	}
	no := false
	if _, err := s.UpdateCollection(notes.ID, models.CollectionUpdate{TracksWork: &no}); err != nil {
		t.Fatal(err)
	}
	if w := read(); w.ItemsOpen != 1 || w.ItemsTotal != 2 {
		t.Fatalf("notes reference: open=%d total=%d, want 1/2", w.ItemsOpen, w.ItemsTotal)
	}
}
