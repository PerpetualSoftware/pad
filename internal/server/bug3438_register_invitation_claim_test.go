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

// Codex r1: on cloud with email, the signup mints an email-verification token
// before the claim, and that row references the account. The rollback must
// still remove the account, or the email stays held and a retry with a live
// code meets the duplicate-email 409.
func TestRegisterWithCode_InvitationGoneOnCloudStillRollsTheAccountBack(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("admin lookup: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Cloud Invite WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	register := func(code string) *httptest.ResponseRecorder {
		return doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
			"email":           "late@example.com",
			"name":            "Late",
			"password":        "correct-horse-battery-staple",
			"invitation_code": code,
		})
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
	if rr := register(inv.Code); rr.Code != http.StatusNotFound {
		t.Fatalf("signup with an invitation gone before its claim: %d %s, want 404", rr.Code, rr.Body.String())
	}
	srv.registerInvitationPreClaimHook = nil
	if u, _ := srv.store.GetUserByEmail("late@example.com"); u != nil {
		t.Fatal("the refused cloud signup left its account behind, holding the email")
	}
	// The retry the refusal exists to permit: a new invitation, a clean signup.
	again, err := srv.store.CreateInvitation(ws.ID, "late@example.com", "editor", admin.ID)
	if err != nil {
		t.Fatalf("CreateInvitation (again): %v", err)
	}
	if rr := register(again.Code); rr.Code != http.StatusCreated {
		t.Fatalf("retry with a live code: %d %s, want 201", rr.Code, rr.Body.String())
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

// Codex r2: the account exists from CreateUser on, so a sign-in can mint it a
// session before the claim. The rollback must still remove the account.
func TestRegisterWithCode_SessionMintedBeforeTheClaimDoesNotBlockTheRollback(t *testing.T) {
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

// Codex r2: a signup that came through the invitation EMAIL spends its proof
// before the claim. When the claim then fails and the invitation stays
// pending, the proof is restored, so retrying the same emailed link still
// verifies the address.
func TestRegisterWithCode_RefusedSignupKeepsTheProofForTheRetry(t *testing.T) {
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
	body := map[string]string{
		"email": "proved@example.com", "name": "Invitee", "password": "correct-horse-battery-staple",
		"invitation_code": code, "invitation_proof": proof,
	}
	// The workspace fills in the window: the claim is refused, the invitation
	// stays pending.
	srv.registerInvitationPreClaimHook = func(string) { setCap(count()) }
	if rr := doRequest(srv, "POST", "/api/v1/auth/register", body); rr.Code == http.StatusCreated {
		t.Fatalf("control: the signup should be refused at the cap: %s", rr.Body.String())
	}
	srv.registerInvitationPreClaimHook = nil
	setCap(count() + 10)
	if rr := doRequest(srv, "POST", "/api/v1/auth/register", body); rr.Code != http.StatusCreated {
		t.Fatalf("retry with room: %d %s", rr.Code, rr.Body.String())
	}
	if !verified(t, srv, "proved@example.com") {
		t.Error("the retry through the same emailed link did not verify the address: the refused signup spent its proof")
	}
}
