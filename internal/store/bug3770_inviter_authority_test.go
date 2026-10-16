package store

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3770: a pending invitation is accepted only while its inviter still
// holds the authority to invite, read at accept time. An inviter removed
// (there is no self-leave door, so leaving is a removal) or demoted from
// owner no longer does.
func TestBUG3770_InvitationDiesWithItsInvitersAuthority(t *testing.T) {
	cases := []struct {
		name string
		// lose takes the inviter's authority away; nil leaves it.
		lose     func(s *Store, wsID, inviterID string) error
		admin    bool
		restore  func(s *Store, wsID, inviterID string) error
		accepted bool
	}{
		{name: "still an owner", accepted: true},
		{name: "removed", lose: func(s *Store, ws, u string) error { return s.RemoveWorkspaceMember(ws, u) }},
		{name: "removed with grants", lose: func(s *Store, ws, u string) error { return s.RemoveWorkspaceMemberAndRevokeGrants(ws, u) }},
		{name: "demoted to editor", lose: func(s *Store, ws, u string) error { return s.UpdateWorkspaceMemberRole(ws, u, "editor") }},
		{name: "demoted to viewer", lose: func(s *Store, ws, u string) error { return s.UpdateWorkspaceMemberRole(ws, u, "viewer") }},
		{
			name:     "demoted then made owner again",
			lose:     func(s *Store, ws, u string) error { return s.UpdateWorkspaceMemberRole(ws, u, "editor") },
			restore:  func(s *Store, ws, u string) error { return s.UpdateWorkspaceMemberRole(ws, u, "owner") },
			accepted: true,
		},
		{
			name:     "removed then re-added as owner",
			lose:     func(s *Store, ws, u string) error { return s.RemoveWorkspaceMember(ws, u) },
			restore:  func(s *Store, ws, u string) error { return s.AddWorkspaceMember(ws, u, "owner") },
			accepted: true,
		},
		{
			name:     "instance admin removed",
			lose:     func(s *Store, ws, u string) error { return s.RemoveWorkspaceMember(ws, u) },
			admin:    true,
			accepted: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			mk := func(email string) *models.User {
				u, err := s.CreateUser(models.UserCreate{Email: email, Name: "U", Password: "pw-test-12345"})
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			owner := mk("owner-3770@example.com")
			inviter := mk("inviter-3770@example.com")
			invitee := mk("invitee-3770@example.com")
			role := "member"
			if tc.admin {
				role = "admin"
			}
			if err := s.SetUserRole(inviter.ID, role); err != nil {
				t.Fatal(err)
			}
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS 3770", OwnerID: owner.ID})
			if err != nil {
				t.Fatal(err)
			}
			// CreateWorkspace records owner_id but writes no membership; a
			// workspace keeps at least one owner member, so the inviter's
			// loss below needs another.
			if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if err := s.AddWorkspaceMember(ws.ID, inviter.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			inv, err := s.CreateInvitation(ws.ID, invitee.Email, "editor", inviter.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.lose != nil {
				if err := tc.lose(s, ws.ID, inviter.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.restore != nil {
				if err := tc.restore(s, ws.ID, inviter.ID); err != nil {
					t.Fatal(err)
				}
			}

			added, held, err := s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, inv.Role)
			m, merr := s.GetWorkspaceMember(ws.ID, invitee.ID)
			if merr != nil && m != nil {
				t.Fatal(merr)
			}
			got, gerr := s.GetInvitation(inv.ID)
			if gerr != nil {
				t.Fatal(gerr)
			}
			if tc.accepted {
				if err != nil || !added || held != "editor" {
					t.Fatalf("accept: added=%v held=%q err=%v, want a new editor", added, held, err)
				}
				if m == nil || m.Role != "editor" {
					t.Fatalf("invitee membership after accept: %+v", m)
				}
				return
			}
			if !errors.Is(err, ErrInviterScopeTooNarrow) {
				t.Fatalf("accept: %v, want ErrInviterScopeTooNarrow", err)
			}
			if m != nil {
				t.Fatalf("a refused accept made a member: %+v", m)
			}
			if got == nil || got.AcceptedAt != nil {
				t.Fatalf("a refused accept did not leave the invitation pending: %+v", got)
			}
		})
	}
}

// BUG-3770 on Postgres: the inviter's role is read in the same share-locked
// select as its scope, so a demotion or a removal committing alongside an
// accept is ordered against it: the accept waits on the uncommitted change,
// then reads it and refuses.
func TestBUG3770_AcceptWaitsOnAConcurrentInviterLoss_Postgres(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("Postgres-only: FOR SHARE serialization")
	}
	for _, tc := range []struct{ name, sql string }{
		{"demotion", `UPDATE workspace_members SET role = 'editor' WHERE workspace_id = ? AND user_id = ?`},
		{"removal", `DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			mk := func(email string) *models.User {
				u, err := s.CreateUser(models.UserCreate{Email: email, Name: "U", Password: "pw-test-12345"})
				if err != nil {
					t.Fatal(err)
				}
				return u
			}
			owner := mk("owner-3770pg@example.com")
			inviter := mk("inviter-3770pg@example.com")
			invitee := mk("invitee-3770pg@example.com")
			if err := s.SetUserRole(inviter.ID, "member"); err != nil {
				t.Fatal(err)
			}
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "WS 3770pg", OwnerID: owner.ID})
			if err != nil {
				t.Fatal(err)
			}
			// CreateWorkspace records owner_id but writes no membership; a
			// workspace keeps at least one owner member, so the inviter's
			// loss below needs another.
			if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			if err := s.AddWorkspaceMember(ws.ID, inviter.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			inv, err := s.CreateInvitation(ws.ID, invitee.Email, "editor", inviter.ID)
			if err != nil {
				t.Fatal(err)
			}

			loss, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = loss.Rollback() }()
			if _, err := loss.Exec(s.q(tc.sql), ws.ID, inviter.ID); err != nil {
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
					WHERE wait_event_type = 'Lock' AND query LIKE '%SELECT role, collection_access FROM workspace_members%FOR SHARE%'`).Scan(&n); err != nil {
					t.Fatal(err)
				}
				waiting = n > 0
				if !waiting {
					time.Sleep(20 * time.Millisecond)
				}
			}
			if !waiting {
				t.Fatalf("the accept did not wait on the inviter's pending %s", tc.name)
			}
			if err := loss.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrInviterScopeTooNarrow) {
					t.Fatalf("accept after the %s committed: %v, want ErrInviterScopeTooNarrow", tc.name, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the accept never finished")
			}
			if m, _ := s.GetWorkspaceMember(ws.ID, invitee.ID); m != nil {
				t.Fatalf("the invitee joined as %q", m.Role)
			}
		})
	}
}
