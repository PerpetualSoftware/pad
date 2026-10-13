package store

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3634: an accept re-reads the inviter's scope, share-locked, so a
// restriction committing alongside it is ordered against it.
func TestBUG3634_AcceptWaitsOnAConcurrentInviterRestriction_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: FOR SHARE serialization")
	}
	s := testStore(t)
	mk := func(email string) *models.User {
		u, err := s.CreateUser(models.UserCreate{Email: email, Name: "U", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner := mk("owner-3634@example.com")
	inviter := mk("inviter-3634@example.com")
	invitee := mk("invitee-3634@example.com")
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS 3634", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, inviter.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateInvitation(ws.ID, invitee.Email, "owner", inviter.ID)
	if err != nil {
		t.Fatal(err)
	}

	restrict, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = restrict.Rollback() }()
	if _, err := restrict.Exec(s.q(`UPDATE workspace_members SET collection_access = 'specific' WHERE workspace_id = ? AND user_id = ?`),
		ws.ID, inviter.ID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, inv.Role)
		done <- err
	}()
	waiting := false
	for i := 0; i < 100 && !waiting; i++ {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND query LIKE '%collection_access FROM workspace_members%FOR SHARE%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		waiting = n > 0
		if !waiting {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !waiting {
		t.Fatal("the accept did not wait on the inviter's pending restriction")
	}
	if err := restrict.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrInviterScopeTooNarrow) {
			t.Fatalf("accept after the restriction committed: %v, want ErrInviterScopeTooNarrow", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the accept never finished")
	}
}
