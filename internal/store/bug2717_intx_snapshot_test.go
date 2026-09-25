package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2717: restore and move returned a POST-COMMIT re-read of the item, so a
// concurrent write landing between the commit and that read was handed back to
// the caller as though this mutation had produced it. Both now return the
// snapshot they read inside their own transaction, as the update path does
// (BUG-2264).
//
// The hook lands a real write in exactly that window. The assertion is on what
// the WRONG behaviour returns: the concurrent writer's title and seq. An
// end-state check could not tell the two apart, because without the hook both
// versions return the same row.

func bug2717Fixture(t *testing.T) (*Store, *models.Item, *models.Collection) {
	t.Helper()
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Snap"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	a, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Alpha", Slug: "alpha", Prefix: "ALP", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection alpha: %v", err)
	}
	b, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Beta", Slug: "beta", Prefix: "BET", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection beta: %v", err)
	}
	it, err := s.CreateItem(ws.ID, a.ID, models.ItemCreate{Title: "mine", Fields: `{}`})
	if err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return s, it, b
}

// concurrentRetitle arms the seam to retitle the item once, from "another
// writer", in the window after the mutation's commit.
func concurrentRetitle(t *testing.T, s *Store) {
	t.Helper()
	s.afterItemRestoreOrMoveCommit = func(itemID string) {
		s.afterItemRestoreOrMoveCommit = nil // fire once
		title := "theirs"
		if _, err := s.UpdateItem(itemID, models.ItemUpdate{Title: &title}); err != nil {
			t.Errorf("concurrent retitle: %v", err)
		}
	}
	t.Cleanup(func() { s.afterItemRestoreOrMoveCommit = nil })
}

func assertOwnSnapshot(t *testing.T, s *Store, got *models.Item, what string) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s returned nil", what)
	}
	now, err := s.GetItem(got.ID)
	if err != nil || now == nil {
		t.Fatalf("GetItem: %v", err)
	}
	// Precondition: the concurrent write really landed, or this test proves
	// nothing about the window.
	if now.Title != "theirs" {
		t.Fatalf("control: the concurrent retitle did not land (title %q)", now.Title)
	}
	if got.Title != "mine" {
		t.Errorf("%s returned title %q — the concurrent writer's row, not the one this mutation committed", what, got.Title)
	}
	if got.Seq >= now.Seq {
		t.Errorf("%s returned seq %d, not below the concurrent write's %d", what, got.Seq, now.Seq)
	}
}

func TestRestoreReturnsItsOwnCommittedSnapshot(t *testing.T) {
	s, it, _ := bug2717Fixture(t)
	if err := s.DeleteItem(it.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	concurrentRetitle(t, s)
	got, err := s.RestoreItem(it.ID)
	if err != nil {
		t.Fatalf("RestoreItem: %v", err)
	}
	assertOwnSnapshot(t, s, got, "RestoreItem")
}

func TestMoveReturnsItsOwnCommittedSnapshot(t *testing.T) {
	s, it, target := bug2717Fixture(t)
	concurrentRetitle(t, s)
	got, err := s.MoveItemWithPreCheck(it.ID, target.ID, "{}", nil)
	if err != nil {
		t.Fatalf("MoveItemWithPreCheck: %v", err)
	}
	assertOwnSnapshot(t, s, got, "MoveItemWithPreCheck")
	if got.CollectionID != target.ID {
		t.Errorf("moved item collection %q, want %q", got.CollectionID, target.ID)
	}
}
