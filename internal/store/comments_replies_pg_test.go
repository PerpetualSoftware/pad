package store

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3252 on Postgres. The server and MCP tests run on SQLite, so without
// this the conditional DELETE (NOT EXISTS reply) and the zero-row branch were
// never executed against the dialect whose READ COMMITTED snapshots the codex
// r2 fix exists for. comments.parent_id has the same FK there
// (pgmigrations/001_initial.sql), so the pre-fix delete failed the same way.
func TestDeleteCommentWithReplies_Postgres(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, pgURL)
	ws := createTestWorkspace(t, s, "Threads")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Threaded", "")

	parent, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "parent"})
	if err != nil {
		t.Fatal(err)
	}
	reply, err := s.CreateComment(ws.ID, item.ID, "", models.CommentCreate{Body: "reply", ParentID: parent.ID})
	if err != nil {
		t.Fatal(err)
	}

	err = s.DeleteComment(parent.ID)
	hr, ok := AsCommentHasRepliesError(err)
	if !ok || hr.Replies != 1 || hr.CommentID != parent.ID {
		t.Fatalf("delete parent: want CommentHasRepliesError{1}, got %v", err)
	}
	for _, id := range []string{parent.ID, reply.ID} {
		if c, err := s.GetComment(id); err != nil || c == nil {
			t.Fatalf("comment %s did not survive the refusal (err %v)", id, err)
		}
	}

	if err := s.DeleteComment(reply.ID); err != nil {
		t.Fatalf("delete reply: %v", err)
	}
	if err := s.DeleteComment(parent.ID); err != nil {
		t.Fatalf("delete parent after its reply: %v", err)
	}
	if err := s.DeleteComment(parent.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("delete of a missing comment: want sql.ErrNoRows, got %v", err)
	}
}
