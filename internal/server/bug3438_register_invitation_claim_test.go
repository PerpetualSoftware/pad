package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3438: register-with-code read the invitation, created the account,
// added the membership and then marked the invitation accepted without
// checking that the row was still there. An invitation declined, cancelled
// or replaced inside that window still let the new account join. The signup
// must claim the invitation in the transaction that adds the membership, and
// a claim that finds no row refuses the signup and rolls the account back,
// as the two accept doors do since BUG-2136.
func TestRegisterWithCode_InvitationGoneBeforeClaimJoinsNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		register := func(email, code string) *httptest.ResponseRecorder {
			return doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
				"email":           email,
				"name":            "Invitee",
				"password":        "correct-horse-battery-staple",
				"invitation_code": code,
			})
		}
		joined := func(email string) bool {
			t.Helper()
			u, err := f.srv.store.GetUserByEmail(email)
			if err != nil {
				t.Fatalf("GetUserByEmail: %v", err)
			}
			if u == nil {
				return false
			}
			m, err := f.srv.store.GetWorkspaceMember(f.wsID, u.ID)
			if err != nil {
				t.Fatalf("GetWorkspaceMember: %v", err)
			}
			return m != nil
		}

		// CONTROL: nothing happens in the window; the signup joins.
		ctl := f.invite("control@example.com", "editor")
		if rr := register("control@example.com", ctl.Code); rr.Code != http.StatusCreated {
			t.Fatalf("control signup: %d %s", rr.Code, rr.Body.String())
		}
		if !joined("control@example.com") {
			t.Fatal("control signup did not join")
		}

		for _, tc := range []struct {
			name  string
			email string
			gone  func(invID string)
		}{
			{"declined or cancelled", "declined@example.com", func(invID string) {
				if err := f.srv.store.DeleteInvitation(f.wsID, invID); err != nil {
					t.Fatalf("DeleteInvitation: %v", err)
				}
			}},
			{"replaced by a re-invite", "replaced@example.com", func(string) {
				f.invite("replaced@example.com", "viewer")
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				inv := f.invite(tc.email, "editor")
				f.srv.registerInvitationPreClaimHook = tc.gone
				defer func() { f.srv.registerInvitationPreClaimHook = nil }()

				rr := register(tc.email, inv.Code)
				if rr.Code != http.StatusNotFound {
					t.Errorf("signup with an invitation gone before its claim: %d %s, want 404", rr.Code, rr.Body.String())
				}
				if joined(tc.email) {
					t.Error("the signup joined the workspace through an invitation that was gone")
				}
				// Rolled back, so the invitee can sign up again with a live code.
				if u, _ := f.srv.store.GetUserByEmail(tc.email); u != nil {
					t.Error("the refused signup left its account behind, holding the email")
				}
			})
		}
	})
}

// notJoinedBody is the 201 of a signup kept without its invitation.
type notJoinedBody struct {
	Token     string         `json:"token"`
	Accepted  map[string]any `json:"accepted_invitation"`
	NotJoined *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"invitation_not_joined"`
}

// Lead ruling on #1834: the account is rolled back only when the invitation
// was the signup's ADMISSION. Where registration is open anyway (Pad Cloud
// with a deliverable verification email, the self-serve path), the account is
// KEPT, nothing is joined, and the response says the invitation is no longer
// valid. The self-hosted legs above are the admission case and roll back.
func TestRegisterWithCode_OpenRegistrationKeepsTheAccountWhenTheInvitationIsGone(t *testing.T) {
	srv, mails := newCloudEmailServer(t)
	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("admin lookup: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Cloud Invite WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "late@example.com", "editor", admin.ID)
	if err != nil {
		t.Fatalf("CreateInvitation: %v", err)
	}
	srv.registerInvitationPreClaimHook = func(invID string) {
		if err := srv.store.DeleteInvitation(ws.ID, invID); err != nil {
			t.Fatalf("DeleteInvitation: %v", err)
		}
	}
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "late@example.com", "name": "Late",
		"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("open-registration signup with a gone invitation: %d %s, want 201 (account kept)", rr.Code, rr.Body.String())
	}
	var body notJoinedBody
	parseJSON(t, rr, &body)
	if body.Token == "" {
		t.Error("the kept account was not signed in")
	}
	if body.Accepted != nil {
		t.Errorf("a signup that joined nothing reported accepted_invitation %v", body.Accepted)
	}
	if body.NotJoined == nil || body.NotJoined.Code != "invitation_gone" || body.NotJoined.Message == "" {
		t.Errorf("invitation_not_joined = %+v, want code invitation_gone with a message", body.NotJoined)
	}
	u, _ := srv.store.GetUserByEmail("late@example.com")
	if u == nil {
		t.Fatal("open registration: the account was rolled back")
	}
	if member, _ := srv.store.IsWorkspaceMember(ws.ID, u.ID); member {
		t.Error("the kept account joined through an invitation that was gone")
	}
	// Kept like any self-serve signup: unverified, with its link sent.
	if u.IsEmailVerified() {
		t.Error("the kept account is verified, though nothing proved the address")
	}
	select {
	case m := <-mails:
		if m.to != "late@example.com" {
			t.Errorf("verification mail went to %q", m.to)
		}
	case <-time.After(5 * time.Second):
		t.Error("the kept account was sent no verification link")
	}
}

// Codex r1: the claim now commits with the membership, so a membership alone no
// longer proves the claim landed. A failed claim beside a membership that came
// from elsewhere must not be reported as a joined signup.
func TestRegisterWithCode_FailedClaimBesideAMembershipIsNotAJoin(t *testing.T) {
	e := newAcceptLimitEnv(t)
	inv := e.invite(t, "beside@example.com")
	// The claim fails (the workspace reaches its cap in the window), and the
	// membership read finds a row, as an admin's concurrent add would leave.
	e.srv.registerInvitationPreClaimHook = func(string) { e.setMemberCap(t, e.members(t)) }
	e.srv.membershipCheck = func(workspaceID, userID string) (*models.WorkspaceMember, error) {
		return &models.WorkspaceMember{WorkspaceID: workspaceID, UserID: userID, Role: "editor"}, nil
	}
	rr := e.register("beside@example.com", inv)
	if rr.Code == http.StatusCreated {
		t.Fatalf("a signup whose claim failed was reported joined: %s", rr.Body.String())
	}
	if !e.stillPending(t, inv) {
		t.Error("the invitation was consumed by a claim that failed")
	}
}

// Codex r2/r3: the account exists from CreateUser on, so a sign-in can mint it
// a session, and /forgot-password a reset token, before the claim. The
// rollback must still remove the account.
func TestRegisterWithCode_CredentialsMintedBeforeTheClaimDoNotBlockTheRollback(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		inv := f.invite("signedin@example.com", "editor")
		f.srv.registerInvitationPreClaimHook = func(invID string) {
			u, err := f.srv.store.GetUserByEmail("signedin@example.com")
			if err != nil || u == nil {
				t.Fatalf("lookup the new account: %v", err)
			}
			if _, err := f.srv.store.CreateSession(u.ID, "go-test", "192.0.2.1", "", time.Hour); err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			// And a public /forgot-password, which mints a reset token (codex r3).
			if _, err := f.srv.store.CreatePasswordReset(u.ID); err != nil {
				t.Fatalf("CreatePasswordReset: %v", err)
			}
			if err := f.srv.store.DeleteInvitation(f.wsID, invID); err != nil {
				t.Fatalf("DeleteInvitation: %v", err)
			}
		}
		rr := doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
			"email": "signedin@example.com", "name": "Invitee",
			"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
		})
		if rr.Code != http.StatusNotFound {
			t.Fatalf("signup: %d %s, want 404", rr.Code, rr.Body.String())
		}
		if u, _ := f.srv.store.GetUserByEmail("signedin@example.com"); u != nil {
			t.Fatal("a session minted in the window kept the refused signup's account")
		}
	})
}

// Open registration, a claim refused for another reason (the workspace filled
// in the window): the account is kept and verified by its emailed proof,
// nothing is joined, the invitation stays pending, and the kept account
// accepts it from its own list once there is room.
func TestRegisterWithCode_OpenRegistrationKeepsTheAccountWhenTheClaimIsRefused(t *testing.T) {
	srv, mails, adminTok, ws := newProofFixture(t)
	_, code, proof := inviteByEmail(t, srv, mails, adminTok, ws.Slug, "proved@example.com")
	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("admin lookup: %v", err)
	}
	count := func() int {
		members, err := srv.store.ListWorkspaceMembers(ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		return len(members)
	}
	setCap := func(n int) {
		if err := srv.store.SetUserPlanOverrides(admin.ID, fmt.Sprintf(`{"members_per_workspace":%d}`, n)); err != nil {
			t.Fatalf("SetUserPlanOverrides: %v", err)
		}
	}
	srv.registerInvitationPreClaimHook = func(string) { setCap(count()) }
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "proved@example.com", "name": "Invitee", "password": "correct-horse-battery-staple",
		"invitation_code": code, "invitation_proof": proof,
	})
	srv.registerInvitationPreClaimHook = nil
	if rr.Code != http.StatusCreated {
		t.Fatalf("open-registration signup refused at the cap: %d %s, want 201 (account kept)", rr.Code, rr.Body.String())
	}
	var body notJoinedBody
	parseJSON(t, rr, &body)
	if body.Accepted != nil || body.NotJoined == nil || body.NotJoined.Code != "invitation_not_applied" {
		t.Errorf("accepted_invitation = %v, invitation_not_joined = %+v; want none and code invitation_not_applied", body.Accepted, body.NotJoined)
	}
	if !verified(t, srv, "proved@example.com") {
		t.Error("the kept account is not verified, though its emailed proof was spent on it")
	}
	pending, err := srv.store.GetInvitationByCode(code)
	if err != nil || pending == nil {
		t.Fatalf("the invitation should stay pending: %v %v", pending, err)
	}
	// With room, the kept account accepts it from its own list.
	setCap(count() + 10)
	rr = doRequestWithCookie(srv, "POST", "/api/v1/me/invitations/"+pending.ID+"/accept", nil, body.Token)
	if rr.Code != http.StatusOK {
		t.Fatalf("accepting from the list afterwards: %d %s", rr.Code, rr.Body.String())
	}
}

// Codex r5: the claim keeps a membership that already exists (an admin's add
// in the window) at ITS role, and the signup must report that role, as the
// accept doors do, not the invitation's.
func TestRegisterWithCode_ReportsTheRoleTheAccountHolds(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		inv := f.invite("added@example.com", "editor")
		f.srv.registerInvitationPreClaimHook = func(string) {
			u, err := f.srv.store.GetUserByEmail("added@example.com")
			if err != nil || u == nil {
				t.Fatalf("lookup the new account: %v", err)
			}
			if err := f.srv.store.AddWorkspaceMember(f.wsID, u.ID, "viewer"); err != nil {
				t.Fatalf("AddWorkspaceMember: %v", err)
			}
		}
		rr := doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
			"email": "added@example.com", "name": "Invitee",
			"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("signup: %d %s", rr.Code, rr.Body.String())
		}
		var body struct {
			Accepted struct {
				Role string `json:"role"`
			} `json:"accepted_invitation"`
		}
		parseJSON(t, rr, &body)
		if body.Accepted.Role != "viewer" {
			t.Errorf("accepted_invitation.role = %q, want the role held (viewer)", body.Accepted.Role)
		}
	})
}

// Codex r9: a claim the store refused BEFORE its commit is definitely not
// landed, so a failing membership read must not turn the open-registration
// outcome (account kept, told) into a 500 that leaves the account behind with
// no session and a retry that meets the duplicate-email 409.
func TestRegisterWithCode_OpenRegistrationRefusedClaimNeedsNoReconcileRead(t *testing.T) {
	srv, mails, adminTok, ws := newProofFixture(t)
	_, code, _ := inviteByEmail(t, srv, mails, adminTok, ws.Slug, "unread@example.com")
	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("admin lookup: %v", err)
	}
	srv.registerInvitationPreClaimHook = func(string) {
		members, err := srv.store.ListWorkspaceMembers(ws.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.store.SetUserPlanOverrides(admin.ID, fmt.Sprintf(`{"members_per_workspace":%d}`, len(members))); err != nil {
			t.Fatalf("SetUserPlanOverrides: %v", err)
		}
	}
	srv.membershipCheck = func(string, string) (*models.WorkspaceMember, error) {
		return nil, fmt.Errorf("injected: membership read failed")
	}
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "unread@example.com", "name": "Invitee",
		"password": "correct-horse-battery-staple", "invitation_code": code,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("open-registration signup refused before commit, with an unreadable membership: %d %s, want 201", rr.Code, rr.Body.String())
	}
	var body notJoinedBody
	parseJSON(t, rr, &body)
	if body.NotJoined == nil || body.NotJoined.Code != "invitation_not_applied" || body.Token == "" {
		t.Errorf("invitation_not_joined = %+v, token set = %v; want invitation_not_applied and a session", body.NotJoined, body.Token != "")
	}
}

// Codex r9: when the invitation was the admission and the account rollback
// FAILS, the signup must not answer as if it had been rolled back: the
// account still holds the email.
func TestRegisterWithCode_FailedRollbackIsNotReportedAsARollback(t *testing.T) {
	f := newAccessFixture(t, store.DriverSQLite)
	inv := f.invite("stuck@example.com", "editor")
	f.srv.registerInvitationPreClaimHook = func(invID string) {
		if err := f.srv.store.DeleteInvitation(f.wsID, invID); err != nil {
			t.Fatalf("DeleteInvitation: %v", err)
		}
	}
	f.srv.signupRollbackDelete = func(string) error { return fmt.Errorf("injected: rollback failed") }
	rr := doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "stuck@example.com", "name": "Invitee",
		"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
	})
	if rr.Code == http.StatusNotFound || rr.Code == http.StatusCreated {
		t.Fatalf("a failed rollback answered %d %s; want a server error, not the rolled-back 404 nor a success", rr.Code, rr.Body.String())
	}
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500", rr.Code)
	}
}
