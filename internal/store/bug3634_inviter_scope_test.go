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

// A restricted owner's role change re-checks the target's scope under the
// row lock the write takes: widened since the caller's check, the target's
// role is not changed.
func TestBUG3634_RoleChangeWithinRechecksTheTargetsScope(t *testing.T) {
	s := testStore(t)
	owner, err := s.CreateUser(models.UserCreate{Email: "o-3634r@example.com", Name: "O", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	target, err := s.CreateUser(models.UserCreate{Email: "t-3634r@example.com", Name: "T", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS 3634r", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, target.ID, "viewer"); err != nil {
		t.Fatal(err)
	}
	a, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "A3634", Slug: "a3634"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "B3634", Slug: "b3634"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberCollectionAccess(ws.ID, target.ID, "specific", []string{a.ID}); err != nil {
		t.Fatal(err)
	}
	checked := []string{a.ID}

	// Still within: the change lands.
	if err := s.UpdateWorkspaceMemberRoleWithin(ws.ID, target.ID, "editor", checked); err != nil {
		t.Fatalf("role change within scope: %v", err)
	}
	// Widened to another collection, then to all: refused, role unchanged.
	for _, widen := range []func() error{
		func() error { return s.SetMemberCollectionAccess(ws.ID, target.ID, "specific", []string{a.ID, b.ID}) },
		func() error { return s.SetMemberCollectionAccess(ws.ID, target.ID, "all", nil) },
	} {
		if err := widen(); err != nil {
			t.Fatal(err)
		}
		if err := s.UpdateWorkspaceMemberRoleWithin(ws.ID, target.ID, "owner", checked); !errors.Is(err, ErrMemberScopeChanged) {
			t.Fatalf("role change after a widening: %v, want ErrMemberScopeChanged", err)
		}
		if m, _ := s.GetWorkspaceMember(ws.ID, target.ID); m == nil || m.Role != "editor" {
			t.Fatalf("the widened member's role changed: %+v", m)
		}
	}
}
