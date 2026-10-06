package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
