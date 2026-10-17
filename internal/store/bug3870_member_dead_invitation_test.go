package store

import (
	"database/sql"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// bug3870Fixture: a workspace with a second owner (the inviter), an invitee,
// and a pending editor invitation from the inviter to the invitee.
func bug3870Fixture(t *testing.T, s *Store, tag string) (ws *models.Workspace, inviter, invitee *models.User, inv *models.WorkspaceInvitation) {
	t.Helper()
	mk := func(email string) *models.User {
		u, err := s.CreateUser(models.UserCreate{Email: email, Name: "U", Password: "pw-test-12345"})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	owner := mk("owner-3870" + tag + "@example.com")
	inviter = mk("inviter-3870" + tag + "@example.com")
	invitee = mk("invitee-3870" + tag + "@example.com")
	if err := s.SetUserRole(inviter.ID, "member"); err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS 3870" + tag, OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddWorkspaceMember(ws.ID, inviter.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	inv, err = s.CreateInvitation(ws.ID, invitee.Email, "editor", inviter.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ws, inviter, invitee, inv
}

// BUG-3870: an invitee who is ALREADY a member, accepting an invitation whose
// inviter can no longer grant it, gets BUG-3281's idempotent success (marked
// accepted, role unchanged, nothing emitted), not inviter_scope_changed. A
// non-member accepting the same dead invitation is still refused.
func TestBUG3870_AMemberAcceptingADeadInvitationIsAlreadyIn(t *testing.T) {
	losses := []struct {
		name string
		lose func(s *Store, ws, u string) error
	}{
		{"inviter removed", func(s *Store, ws, u string) error { return s.RemoveWorkspaceMember(ws, u) }},
		{"inviter demoted to editor", func(s *Store, ws, u string) error { return s.UpdateWorkspaceMemberRole(ws, u, "editor") }},
		{"inviter restricted to specific collections", func(s *Store, ws, u string) error {
			return s.SetMemberCollectionAccess(ws, u, "specific", nil)
		}},
	}
	for _, l := range losses {
		t.Run(l.name+"/already a member", func(t *testing.T) {
			s := testStore(t)
			ws, inviter, invitee, inv := bug3870Fixture(t, s, "m")
			// The invitee is already in, as a viewer: the accept must keep that
			// role, not raise it to the invitation's editor.
			if err := s.AddWorkspaceMember(ws.ID, invitee.ID, "viewer"); err != nil {
				t.Fatal(err)
			}
			if err := l.lose(s, ws.ID, inviter.ID); err != nil {
				t.Fatal(err)
			}
			var eventsBefore int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_outbox`).Scan(&eventsBefore); err != nil {
				t.Fatal(err)
			}

			added, held, err := s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, inv.Role)
			if err != nil {
				t.Fatalf("accept: %v, want the idempotent success", err)
			}
			if added || held != "viewer" {
				t.Fatalf("accept: added=%v held=%q, want added=false held=viewer", added, held)
			}
			if m, _ := s.GetWorkspaceMember(ws.ID, invitee.ID); m == nil || m.Role != "viewer" {
				t.Fatalf("membership after accept: %+v, want the viewer row unchanged", m)
			}
			got, err := s.GetInvitation(inv.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.AcceptedAt == nil {
				t.Fatalf("invitation after accept: %+v, want accepted", got)
			}
			var eventsAfter int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM event_outbox`).Scan(&eventsAfter); err != nil {
				t.Fatal(err)
			}
			if eventsAfter != eventsBefore {
				t.Fatalf("event_outbox grew by %d; an accept that changes no membership emits nothing", eventsAfter-eventsBefore)
			}
		})
		t.Run(l.name+"/not a member (control)", func(t *testing.T) {
			s := testStore(t)
			ws, inviter, invitee, inv := bug3870Fixture(t, s, "c")
			if err := l.lose(s, ws.ID, inviter.ID); err != nil {
				t.Fatal(err)
			}
			_, _, err := s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, inv.Role)
			if !errors.Is(err, ErrInviterScopeTooNarrow) {
				t.Fatalf("accept: %v, want ErrInviterScopeTooNarrow", err)
			}
			if m, _ := s.GetWorkspaceMember(ws.ID, invitee.ID); m != nil {
				t.Fatalf("a refused accept made a member: %+v", m)
			}
			if got, _ := s.GetInvitation(inv.ID); got == nil || got.AcceptedAt != nil {
				t.Fatalf("a refused accept did not leave the invitation pending: %+v", got)
			}
		})
	}
}

// BUG-3870 on Postgres: the invitee's membership is read share-locked after
// the inviter check refuses, so a removal of the invitee committing alongside
// the accept is ordered against it.
func TestBUG3870_MemberAcceptAndInviteeRemoval_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: FOR SHARE serialization")
	}
	const removeInvitee = `DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`

	t.Run("removal committed first: refused", func(t *testing.T) {
		s := testStore(t)
		ws, inviter, invitee, inv := bug3870Fixture(t, s, "pg1")
		if err := s.AddWorkspaceMember(ws.ID, invitee.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveWorkspaceMember(ws.ID, inviter.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(s.q(removeInvitee), ws.ID, invitee.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, inv.Role); !errors.Is(err, ErrInviterScopeTooNarrow) {
			t.Fatalf("accept: %v, want ErrInviterScopeTooNarrow", err)
		}
	})

	t.Run("accept holds the share lock: the removal waits", func(t *testing.T) {
		s := testStore(t)
		ws, inviter, invitee, inv := bug3870Fixture(t, s, "pg2")
		if err := s.AddWorkspaceMember(ws.ID, invitee.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
		if err := s.RemoveWorkspaceMember(ws.ID, inviter.ID); err != nil {
			t.Fatal(err)
		}
		// Hold the accept open after its membership read: the commit seam
		// runs at the end of the accept's transaction, with its locks held.
		release := make(chan struct{})
		reached := make(chan struct{})
		s.commitAddWorkspaceMember = func(tx *sql.Tx) error {
			close(reached)
			<-release
			return tx.Commit()
		}
		done := make(chan error, 1)
		go func() {
			_, _, err := s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, inv.Role)
			done <- err
		}()
		select {
		case <-reached:
		case <-time.After(10 * time.Second):
			t.Fatal("the accept never reached its commit")
		}
		removed := make(chan error, 1)
		go func() {
			_, err := s.db.Exec(s.q(removeInvitee), ws.ID, invitee.ID)
			removed <- err
		}()
		waiting := false
		for i := 0; i < 100 && !waiting; i++ {
			var n int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
				WHERE wait_event_type = 'Lock' AND query LIKE 'DELETE FROM workspace_members%'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			waiting = n > 0
			if !waiting {
				time.Sleep(20 * time.Millisecond)
			}
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatalf("accept: %v, want the idempotent success", err)
		}
		if err := <-removed; err != nil {
			t.Fatal(err)
		}
		if !waiting {
			t.Fatal("the invitee's removal did not wait on the accept's membership read")
		}
		if m, _ := s.GetWorkspaceMember(ws.ID, invitee.ID); m != nil {
			t.Fatalf("after both committed the invitee is still a %q", m.Role)
		}
		if got, _ := s.GetInvitation(inv.ID); got == nil || got.AcceptedAt == nil {
			t.Fatalf("invitation: %+v, want accepted", got)
		}
	})
}
