package store

import (
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
