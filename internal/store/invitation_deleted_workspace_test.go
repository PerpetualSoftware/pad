package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestGetInvitationByCode_SoftDeletedWorkspace is BUG-3104's reproduction.
//
// DeleteAccountAtomic soft-deletes the departing owner's workspaces but removes
// only the invitations that user SENT. An invitation sent by ANOTHER owner-role
// member therefore survives, pointing at a workspace that is gone — and every
// door that reads it (accept, register-with-invitation, preview) goes through
// this lookup. It must answer "no such invitation" for a deleted workspace,
// exactly as it does for an unknown code.
//
// The CONTROL leg reads the same invitation before the deletion: without it, a
// lookup that returned nil for every code would pass the second assertion.
func TestGetInvitationByCode_SoftDeletedWorkspace(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	owner, err := s.CreateUser(models.UserCreate{
		Email: "owner-3104@example.com", Username: "owner3104", Name: "Owner",
		Password: "correcthorsebattery", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	coOwner, err := s.CreateUser(models.UserCreate{
		Email: "coowner-3104@example.com", Username: "coowner3104", Name: "Co-owner",
		Password: "correcthorsebattery", Role: "member",
	})
	if err != nil {
		t.Fatalf("create co-owner: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Doomed", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, coOwner.ID, "owner"); err != nil {
		t.Fatalf("add co-owner: %v", err)
	}

	// Sent by the CO-OWNER, so the owner's account deletion does not remove it.
	inv, err := s.CreateInvitation(ws.ID, "invitee-3104@example.com", "editor", coOwner.ID)
	if err != nil {
		t.Fatalf("create invitation: %v", err)
	}

	// A LEGACY row too: plaintext code, no hash, so only the fallback lookup
	// can find it. That branch has its own query and its own join.
	legacyCode := "legacy-3104-" + newID()
	if _, err := s.db.Exec(s.q(`
		INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`), newID(), ws.ID, "legacy-3104@example.com", "editor", coOwner.ID, legacyCode, now()); err != nil {
		t.Fatalf("insert legacy invitation: %v", err)
	}
	if got, err := s.GetInvitationByCode(legacyCode); err != nil || got == nil {
		t.Fatalf("CONTROL: the legacy invitation must be found through the fallback; got %+v, err %v", got, err)
	}

	before, err := s.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("lookup before deletion: %v", err)
	}
	if before == nil || before.ID != inv.ID {
		t.Fatalf("CONTROL: a live workspace's invitation must be found; got %+v", before)
	}

	if err := s.DeleteAccountAtomic(owner.ID); err != nil {
		t.Fatalf("DeleteAccountAtomic: %v", err)
	}
	if got, err := s.GetWorkspaceByID(ws.ID); err != nil || got != nil {
		t.Fatalf("precondition: workspace should read as deleted; got %+v, err %v", got, err)
	}

	after, err := s.GetInvitationByCode(inv.Code)
	if err != nil {
		t.Fatalf("lookup after deletion: %v", err)
	}
	if after != nil {
		t.Fatalf("an invitation to a soft-deleted workspace was returned as live: %+v", after)
	}
	if got, err := s.GetInvitationByCode(legacyCode); err != nil || got != nil {
		t.Fatalf("a LEGACY invitation to a soft-deleted workspace was returned as live: %+v, err %v", got, err)
	}
}
