package store

import (
	"errors"
	"sync"
	"testing"
)

// BUG-2136: a re-invite replaces the pending invitation for its (workspace,
// address). Concurrent invites of one address must still leave exactly one
// live invitation: CreateInvitation takes the workspace lock before its
// delete-then-insert, so two of them cannot both see "nothing pending" and
// both insert. On SQLite the write lock serializes them anyway; the leg that
// can fail without the workspace lock is Postgres (make test-pg).
func TestCreateInvitation_ConcurrentReinvitesLeaveOnePending(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner-2136@example.com", "Owner", "password123")
	ws := createTestWorkspace(t, s, "Reinvite race")
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.CreateInvitation(ws.ID, "raced@example.com", "editor", owner.ID); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("CreateInvitation: %v", err)
	}

	pending, err := s.ListWorkspaceInvitations(ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending invitations after %d concurrent invites of one address, want exactly 1", len(pending), n)
	}
}

// Codex r1: accept must not land after the invitation was declined (or
// replaced). The handler reads the invitation before the store transaction,
// so a decline can delete the row in between; the accept must then add no
// membership and report the invitation gone. Modelled by deleting the row
// between the read and AcceptWorkspaceInvitation.
func TestAcceptWorkspaceInvitation_RowGoneAddsNoMembership(t *testing.T) {
	s := testStore(t)
	owner := createTestUser(t, s, "owner-2136b@example.com", "Owner", "password123")
	invitee := createTestUser(t, s, "late@example.com", "Late", "password123")
	ws := createTestWorkspace(t, s, "Accept after decline")
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	inv, err := s.CreateInvitation(ws.ID, "late@example.com", "editor", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Control: a live invitation accepts.
	ctl, err := s.CreateInvitation(ws.ID, "control@example.com", "editor", owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctlUser := createTestUser(t, s, "control@example.com", "Control", "password123")
	if _, _, err := s.AcceptWorkspaceInvitation(ctl.ID, ws.ID, ctlUser.ID, "editor"); err != nil {
		t.Fatalf("control accept: %v", err)
	}

	if err := s.DeleteInvitationAdmin(inv.ID); err != nil { // the decline lands first
		t.Fatal(err)
	}
	_, _, err = s.AcceptWorkspaceInvitation(inv.ID, ws.ID, invitee.ID, "editor")
	if !errors.Is(err, ErrInvitationGone) {
		t.Fatalf("accept of a declined invitation: err = %v, want ErrInvitationGone", err)
	}
	if member, _ := s.IsWorkspaceMember(ws.ID, invitee.ID); member {
		t.Errorf("an accept that lost to a decline still added the member")
	}
	// A second accept of an ALREADY ACCEPTED invitation stays idempotent
	// (BUG-3281): the loser of two concurrent accepts.
	if _, _, err := s.AcceptWorkspaceInvitation(ctl.ID, ws.ID, ctlUser.ID, "editor"); err != nil {
		t.Errorf("re-accepting an accepted invitation must stay idempotent, got %v", err)
	}
}
