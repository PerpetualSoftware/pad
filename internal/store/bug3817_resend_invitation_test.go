package store

import (
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// bug3817Env makes a workspace with an owner, an admin who resends, and one
// pending invitation the owner sent.
func bug3817Env(t *testing.T, s *Store) (ws *models.Workspace, admin *models.User, inv *models.WorkspaceInvitation) {
	t.Helper()
	owner, err := s.CreateUser(models.UserCreate{
		Email: "owner-3817@example.com", Username: "owner3817", Name: "Owner",
		Password: "correcthorsebattery", Role: "member",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	admin, err = s.CreateUser(models.UserCreate{
		Email: "admin-3817@example.com", Username: "admin3817", Name: "Admin",
		Password: "correcthorsebattery", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	// A second admin, so disabling the first is not refused as the last
	// active admin (BUG-3811).
	if _, err := s.CreateUser(models.UserCreate{
		Email: "keeper-3817@example.com", Username: "keeper3817", Name: "Keeper",
		Password: "correcthorsebattery", Role: "admin",
	}); err != nil {
		t.Fatalf("create keeper admin: %v", err)
	}
	ws, err = s.CreateWorkspace(models.WorkspaceCreate{Name: "Resend 3817", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// The inviter must still be an owner when the invitee accepts (BUG-3770).
	if err := s.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner member: %v", err)
	}
	inv, err = s.CreateInvitation(ws.ID, "invitee-3817@example.com", "editor", owner.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}
	return ws, admin, inv
}

// bug3817Pending lists the workspace's pending invitations by id.
func bug3817Pending(t *testing.T, s *Store, workspaceID string) []string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT id FROM workspace_invitations WHERE workspace_id = ? AND accepted_at IS NULL`), workspaceID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// The control: a resend replaces the invitation with a fresh one for the
// same address and role, and the old code stops being pending.
func TestBUG3817_ResendReplacesTheInvitation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, admin, old := bug3817Env(t, s)

	inv, err := s.ResendInvitationAdmin(old.ID, admin.ID)
	if err != nil {
		t.Fatalf("resend: %v", err)
	}
	if inv.ID == old.ID || inv.Code == "" || inv.Proof == "" {
		t.Errorf("resend returned id %q code %q proof %q: want a fresh invitation with its secrets", inv.ID, inv.Code, inv.Proof)
	}
	if inv.Email != old.Email || inv.Role != old.Role || inv.InvitedBy != admin.ID {
		t.Errorf("resend wrote %s/%s by %s, want %s/%s by %s", inv.Email, inv.Role, inv.InvitedBy, old.Email, old.Role, admin.ID)
	}
	if got := bug3817Pending(t, s, ws.ID); len(got) != 1 || got[0] != inv.ID {
		t.Errorf("pending = %v, want only the new %s", got, inv.ID)
	}
	if _, err := s.ResendInvitationAdmin(old.ID, admin.ID); !errors.Is(err, ErrInvitationNotPending) {
		t.Errorf("a second resend of the replaced invitation err = %v, want ErrInvitationNotPending", err)
	}
}

// A resend of an invitation that was accepted is refused, and the accepted
// row and the membership stay as they were (BUG-3817 r1 M2 condition).
func TestBUG3817_AResendOfAnAcceptedInvitationIsRefused(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, admin, old := bug3817Env(t, s)
	invitee, err := s.CreateUser(models.UserCreate{
		Email: old.Email, Username: "invitee3817", Name: "Invitee",
		Password: "correcthorsebattery", Role: "member",
	})
	if err != nil {
		t.Fatalf("create invitee: %v", err)
	}
	if _, _, err := s.AcceptWorkspaceInvitation(old.ID, ws.ID, invitee.ID, old.Role); err != nil {
		t.Fatalf("accept: %v", err)
	}

	if _, err := s.ResendInvitationAdmin(old.ID, admin.ID); !errors.Is(err, ErrInvitationNotPending) {
		t.Errorf("resend of an accepted invitation err = %v, want ErrInvitationNotPending", err)
	}
	if got := bug3817Pending(t, s, ws.ID); len(got) != 0 {
		t.Errorf("pending = %v, want none", got)
	}
	if got, err := s.GetInvitation(old.ID); err != nil || got == nil || got.AcceptedAt == nil {
		t.Errorf("accepted invitation after the refused resend = %+v, %v; want it kept, accepted", got, err)
	}
	if m, err := s.GetWorkspaceMember(ws.ID, invitee.ID); err != nil || m == nil {
		t.Errorf("membership after the refused resend = %+v, %v; want it kept", m, err)
	}
}

// A create refused after the revoke must not lose the original: the two are
// one transaction (BUG-3817 item 2). The refusal here is the disabled
// inviter's (BUG-3349).
func TestBUG3817_ARefusedResendKeepsTheOriginal(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, admin, old := bug3817Env(t, s)
	if err := s.DisableUserAndRevokeAccess(admin.ID); err != nil {
		t.Fatalf("disable admin: %v", err)
	}

	if _, err := s.ResendInvitationAdmin(old.ID, admin.ID); err == nil {
		t.Fatal("a disabled inviter's resend succeeded")
	}
	if got := bug3817Pending(t, s, ws.ID); len(got) != 1 || got[0] != old.ID {
		t.Errorf("pending = %v, want the original %s kept", got, old.ID)
	}
}

// A workspace deleted before the resend's transaction gets no new invitation,
// and its original stays as it was (BUG-3817 item 1). CreateInvitation, the
// member invite's door, refuses the same way.
func TestBUG3817_ADeletedWorkspaceGetsNoInvitation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws, admin, old := bug3817Env(t, s)
	if err := s.DeleteWorkspace(ws.Slug); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	if _, err := s.ResendInvitationAdmin(old.ID, admin.ID); !errors.Is(err, ErrInvitationWorkspaceDeleted) {
		t.Errorf("resend err = %v, want ErrInvitationWorkspaceDeleted", err)
	}
	if _, err := s.CreateInvitation(ws.ID, "late-3817@example.com", "editor", admin.ID); !errors.Is(err, ErrInvitationWorkspaceDeleted) {
		t.Errorf("create err = %v, want ErrInvitationWorkspaceDeleted", err)
	}
	if got := bug3817Pending(t, s, ws.ID); len(got) != 1 || got[0] != old.ID {
		t.Errorf("pending = %v, want only the original %s", got, old.ID)
	}
}

// A resend racing a workspace deletion: the deletion's update of the row is
// held uncommitted, the resend is observed waiting on the row lock, and once
// the deletion commits the resend must refuse instead of minting a code for
// the deleted workspace (BUG-3817). Before the fix the resend did not lock
// or read the workspace at all inside its writes.
//
// Postgres only: on SQLite the open transaction holds the database write
// lock, so the resend cannot start until the deletion commits.
func TestBUG3817_AResendRacingADeletionMintsNothing(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("Postgres only: SQLite's single writer serializes the pair")
	}
	ws, admin, old := bug3817Env(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ts := now()
	if _, err := tx.Exec(s.q(`UPDATE workspaces SET deleted_at = ?, updated_at = ? WHERE id = ?`), ts, ts, ws.ID); err != nil {
		t.Fatalf("hold the deletion: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.ResendInvitationAdmin(old.ID, admin.ID)
		done <- err
	}()

	bug3817AwaitLiveCheckWait(t, s, done)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the deletion: %v", err)
	}
	var resendErr error
	select {
	case resendErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the resend never returned after the deletion committed")
	}

	if !errors.Is(resendErr, ErrInvitationWorkspaceDeleted) {
		t.Errorf("resend err = %v, want ErrInvitationWorkspaceDeleted", resendErr)
	}
	if got := bug3817Pending(t, s, ws.ID); len(got) != 1 || got[0] != old.ID {
		t.Errorf("pending = %v, want only the original %s: a code was minted for a deleted workspace", got, old.ID)
	}
}

// bug3817AwaitLiveCheckWait returns once a backend is waiting on a lock while
// running createInvitationTx's FOR SHARE liveness read, so a test proves the
// resend reached that statement and blocked there, not that some other query
// in the database is waiting (codex r1 Medium). It fails if done delivers
// first: the resend did not wait.
func bug3817AwaitLiveCheckWait(t *testing.T, s *Store, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%FROM workspaces WHERE id = $1 AND deleted_at IS NULL FOR SHARE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("the resend returned (err %v) before the other transaction committed: it did not wait on the workspace row", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the resend never waited on the workspace row's liveness read")
		}
	}
}

// A resend racing the deletion of the account that owns the workspace and
// sent the invitation. Account deletion locks the user, soft-deletes their
// workspaces, then deletes the invitations they sent (DeleteAccountAtomic's
// order, replayed here step by step so the resend can be caught between
// them). A resend that took the invitation row before the workspace row
// deadlocked with it (codex r1 High); now it waits holding no invitation
// row, the deletion finishes, and the resend refuses.
//
// Postgres only, as above.
func TestBUG3817_AResendRacingTheOwnersAccountDeletionDoesNotDeadlock(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("Postgres only: SQLite's single writer serializes the pair")
	}
	ws, admin, old := bug3817Env(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(s.q(`SELECT id FROM users WHERE id = ? FOR NO KEY UPDATE`), old.InvitedBy); err != nil {
		t.Fatalf("lock the owner: %v", err)
	}
	ts := now()
	if _, err := tx.Exec(s.q(`UPDATE workspaces SET deleted_at = ?, updated_at = ? WHERE id = ?`), ts, ts, ws.ID); err != nil {
		t.Fatalf("soft-delete the owned workspace: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.ResendInvitationAdmin(old.ID, admin.ID)
		done <- err
	}()
	bug3817AwaitLiveCheckWait(t, s, done)

	if _, err := tx.Exec(s.q(`DELETE FROM workspace_invitations WHERE invited_by = ?`), old.InvitedBy); err != nil {
		t.Fatalf("the deletion's invitation step failed while the resend waited (a deadlock is 40P01): %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the deletion: %v", err)
	}
	var resendErr error
	select {
	case resendErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the resend never returned after the deletion committed")
	}
	if !errors.Is(resendErr, ErrInvitationWorkspaceDeleted) {
		t.Errorf("resend err = %v, want ErrInvitationWorkspaceDeleted", resendErr)
	}
	if got := bug3817Pending(t, s, ws.ID); len(got) != 0 {
		t.Errorf("pending = %v, want none: the deletion removed the owner's invitations and the resend minted nothing", got)
	}
}
