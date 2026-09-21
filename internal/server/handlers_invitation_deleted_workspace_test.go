package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3104: an invitation that outlives its workspace (DeleteAccountAtomic
// removes only the invitations the departing user SENT) must read as an
// unknown code at every door, not as a live invitation — and not as "expired",
// which tells the invitee to ask for a new one. The store lookup is pinned by
// internal/store/invitation_deleted_workspace_test.go; these legs pin that each
// DOOR answers with the shape it already uses for an unknown code. Measured
// unfixed on THIS fixture (cloud mode, workspace soft-deleted, owner still
// present): the accept answered 200 and added the member, and the register
// answered 201 and created the account. The bug body's 500 ("owner not found"
// from the member-cap check) is the account-deletion variant, where the owner
// row is gone; the store test drives that path.
//
// Each refused leg has a live-workspace control on the same fixture, so a door
// that refused EVERY invitation cannot pass (CONVE-12).

func deletedWorkspaceEnv(t *testing.T, deleteIt bool) (*acceptLimitEnv, *models.WorkspaceInvitation) {
	t.Helper()
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "invitee-3104@example.com")
	if deleteIt {
		if err := e.srv.store.DeleteWorkspace(e.home.Slug); err != nil {
			t.Fatalf("DeleteWorkspace: %v", err)
		}
	}
	return e, inv
}

func TestAcceptInvitation_DeletedWorkspace_NotFound(t *testing.T) {
	t.Run("CONTROL: live workspace accepts", func(t *testing.T) {
		e, inv := deletedWorkspaceEnv(t, false)
		rr := e.acceptAs(t, "invitee-3104@example.com", inv)
		if rr.Code != http.StatusOK {
			t.Fatalf("live accept = %d %s; want 200", rr.Code, rr.Body.String())
		}
	})
	t.Run("deleted workspace answers 404 not_found and adds no member", func(t *testing.T) {
		e, inv := deletedWorkspaceEnv(t, true)
		before := e.members(t)
		rr := e.acceptAs(t, "invitee-3104@example.com", inv)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("accept = %d %s; want 404", rr.Code, rr.Body.String())
		}
		if got := errorCode(t, rr); got != "not_found" {
			t.Fatalf("code = %q; want not_found", got)
		}
		if after := e.members(t); after != before {
			t.Fatalf("members %d -> %d; a deleted workspace gained a member", before, after)
		}
	})
}

func TestRegisterWithInvitation_DeletedWorkspace_Invalid(t *testing.T) {
	t.Run("CONTROL: live workspace registers", func(t *testing.T) {
		e, inv := deletedWorkspaceEnv(t, false)
		rr := e.register("invitee-3104@example.com", inv)
		if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
			t.Fatalf("live register = %d %s; want success", rr.Code, rr.Body.String())
		}
	})
	t.Run("deleted workspace answers 400 invalid_invitation and creates no account", func(t *testing.T) {
		e, inv := deletedWorkspaceEnv(t, true)
		rr := e.register("invitee-3104@example.com", inv)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("register = %d %s; want 400", rr.Code, rr.Body.String())
		}
		if got := errorCode(t, rr); got != "invalid_invitation" {
			t.Fatalf("code = %q; want invalid_invitation", got)
		}
		if e.accountExists(t, "invitee-3104@example.com") {
			t.Fatal("an account was created from an invitation to a deleted workspace")
		}
	})
}

// Admin resend reads by ID (GetInvitation, deliberately left generic), so it
// needs its own check: without one it re-mints a live code for a deleted
// workspace and emails the invitee about it.
func TestAdminResendInvitation_DeletedWorkspace_NotFound(t *testing.T) {
	adminSession := func(t *testing.T, e *acceptLimitEnv) string {
		t.Helper()
		admin, err := e.srv.store.CreateUser(models.UserCreate{
			Email: "admin-3104@example.com", Name: "Admin", Password: "pw-admin-3104-xyz", Role: "admin",
		})
		if err != nil {
			t.Fatalf("CreateUser(admin): %v", err)
		}
		tok, err := e.srv.store.CreateSession(admin.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		return tok
	}
	pendingFor := func(t *testing.T, e *acceptLimitEnv) int {
		t.Helper()
		return e.countIn(t, `SELECT COUNT(*) FROM workspace_invitations WHERE workspace_id = ? AND accepted_at IS NULL`)
	}

	t.Run("CONTROL: live workspace resends", func(t *testing.T) {
		e, inv := deletedWorkspaceEnv(t, false)
		rr := doRequestWithCookie(e.srv, "POST", "/api/v1/admin/invitations/"+inv.ID+"/resend", nil, adminSession(t, e))
		if rr.Code != http.StatusOK {
			t.Fatalf("live resend = %d %s; want 200", rr.Code, rr.Body.String())
		}
	})
	t.Run("deleted workspace answers 404 and mints nothing", func(t *testing.T) {
		e, inv := deletedWorkspaceEnv(t, true)
		before := pendingFor(t, e)
		rr := doRequestWithCookie(e.srv, "POST", "/api/v1/admin/invitations/"+inv.ID+"/resend", nil, adminSession(t, e))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("resend = %d %s; want 404", rr.Code, rr.Body.String())
		}
		if after := pendingFor(t, e); after != before {
			t.Fatalf("pending invitations %d -> %d; resend re-minted for a deleted workspace", before, after)
		}
	})
}
