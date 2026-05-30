package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestDeleteAccountAtomic_DetachesAuthoredComments is a regression test for the
// FK that became live when TASK-1663 started populating comments.user_id:
// deleting an account that authored comments must NULL those references rather
// than fail on the comments.user_id -> users(id) foreign key. The comments
// survive (with their display-name author preserved) and just become
// admin-only to edit. testStore enables PRAGMA foreign_keys, so without the
// detach step this fails with a FK violation.
func TestDeleteAccountAtomic_DetachesAuthoredComments(t *testing.T) {
	s := testStore(t)

	user, err := s.CreateUser(models.UserCreate{
		Email:    "author@test.com",
		Name:     "Author",
		Password: "correct-horse-battery-staple",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	ws := createTestWorkspace(t, s, "Owned")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Item", "")

	comment, err := s.CreateComment(ws.ID, item.ID, user.ID, models.CommentCreate{
		Body:      "authored",
		Author:    "Author",
		CreatedBy: "user",
		Source:    "web",
	})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	if comment.UserID != user.ID {
		t.Fatalf("expected comment.user_id=%s, got %q", user.ID, comment.UserID)
	}

	// Must not fail on the comments.user_id FK.
	if err := s.DeleteAccountAtomic(user.ID, []string{ws.Slug}); err != nil {
		t.Fatalf("DeleteAccountAtomic: %v", err)
	}

	// The comment survives, with its author reference nulled.
	got, err := s.GetComment(comment.ID)
	if err != nil {
		t.Fatalf("get comment after deletion: %v", err)
	}
	if got == nil {
		t.Fatal("comment was deleted; expected it to survive with null user_id")
	}
	if got.UserID != "" {
		t.Errorf("expected user_id cleared after account deletion, got %q", got.UserID)
	}
}
