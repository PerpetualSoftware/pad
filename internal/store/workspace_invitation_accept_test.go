package store

import (
	"os"
	"testing"
)

// BUG-3281: AcceptWorkspaceInvitation shares addWorkspaceMemberTx with
// AddWorkspaceMember. An existing membership is an idempotent success that
// keeps its role; AddWorkspaceMember still refuses one.

func acceptFixture(t *testing.T, s *Store) (wsID, userID, invID string) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Accept WS")
	u := createTestUser(t, s, "invitee@example.com", "Invitee", "pw-invitee-12345")
	inv, err := s.CreateInvitation(ws.ID, u.Email, "editor", u.ID)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	return ws.ID, u.ID, inv.ID
}

func memberRole(t *testing.T, s *Store, wsID, userID string) string {
	t.Helper()
	m, err := s.GetWorkspaceMember(wsID, userID)
	if err != nil || m == nil {
		t.Fatalf("GetWorkspaceMember = %+v, %v", m, err)
	}
	return m.Role
}

func acceptedAt(t *testing.T, s *Store, invID string) string {
	t.Helper()
	var at *string
	if err := s.db.QueryRow(s.q(`SELECT accepted_at FROM workspace_invitations WHERE id = ?`), invID).Scan(&at); err != nil {
		t.Fatalf("read accepted_at: %v", err)
	}
	if at == nil {
		return ""
	}
	return *at
}

func TestAcceptWorkspaceInvitation_NewAndExistingMember(t *testing.T) {
	s := testStore(t)
	wsID, userID, invID := acceptFixture(t, s)

	added, role, err := s.AcceptWorkspaceInvitation(invID, wsID, userID, "editor")
	if err != nil || !added || role != "editor" {
		t.Fatalf("first accept = %v, %q, %v; want added editor", added, role, err)
	}
	first := acceptedAt(t, s, invID)
	if first == "" {
		t.Fatal("the invitation was not marked accepted")
	}

	// Accepting again as a member at a DIFFERENT role neither errors nor
	// changes the role, and keeps the first accept's timestamp.
	added, role, err = s.AcceptWorkspaceInvitation(invID, wsID, userID, "viewer")
	if err != nil || added || role != "editor" {
		t.Fatalf("repeat accept = %v, %q, %v; want not-added, role editor kept", added, role, err)
	}
	if got := memberRole(t, s, wsID, userID); got != "editor" {
		t.Fatalf("role = %q after an accept at viewer, want editor unchanged", got)
	}
	if got := acceptedAt(t, s, invID); got != first {
		t.Fatalf("accepted_at moved from %q to %q", first, got)
	}

	// AddWorkspaceMember's contract is unchanged: an existing member refuses.
	if err := s.AddWorkspaceMember(wsID, userID, "viewer"); err == nil {
		t.Fatal("AddWorkspaceMember accepted an existing member; it must still fail on the key")
	}
}

// The ON CONFLICT branch: a second accept that read NO row, because the
// first had not committed, must not fail on the key and must report the
// winner's role. Only Postgres can put a second writer past the read while
// the first holds the key (SQLite's BEGIN IMMEDIATE serializes the whole
// transaction), so this is the leg that pins the race; the server test runs
// both doors concurrently on both dialects.
func TestAcceptWorkspaceInvitation_ConcurrentLoserReadsWinner(t *testing.T) {
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set — only Postgres lets the loser past the read")
	}
	s := testStorePostgres(t, pgURL)
	wsID, userID, invID := acceptFixture(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin winner: %v", err)
	}
	defer tx.Rollback()
	added, _, err := s.addWorkspaceMemberTx(tx, wsID, userID, "viewer", mintOptions{}, true)
	if err != nil || !added {
		t.Fatalf("winner insert = %v, %v", added, err)
	}

	type result struct {
		added bool
		role  string
		err   error
	}
	res := make(chan result, 1)
	done := make(chan error, 1)
	go func() {
		a, r, err := s.AcceptWorkspaceInvitation(invID, wsID, userID, "editor")
		res <- result{a, r, err}
		done <- err
	}()

	// The loser has read no row and is now waiting on the winner's key.
	waitForLockWait(t, s, "INSERT INTO workspace_members", done)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit winner: %v", err)
	}

	got := <-res
	if got.err != nil {
		t.Fatalf("loser = %v; it must not fail on the key", got.err)
	}
	if got.added || got.role != "viewer" {
		t.Fatalf("loser = added %v, role %q; want not-added and the winner's viewer", got.added, got.role)
	}
	if acceptedAt(t, s, invID) == "" {
		t.Fatal("the loser did not mark the invitation accepted")
	}
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), wsID, userID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("membership rows = %d, %v; want 1", n, err)
	}
	if memberRole(t, s, wsID, userID) != "viewer" {
		t.Fatal("the loser changed the winner's role")
	}
}
