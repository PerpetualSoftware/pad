package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3770: a pending invitation is accepted only while its inviter can
// still grant it. An inviter removed from the workspace (there is no
// self-leave door: leaving is a removal by an owner) or demoted from owner
// no longer can, so every accept door refuses with inviter_scope_changed,
// and the doors that offer an Accept (the pending list, the /join preview)
// stop offering one.

type bug3770Loss struct {
	name string
	lose func(f *accessFixture, inviter *models.User)
}

var bug3770Losses = []bug3770Loss{
	{"removed", func(f *accessFixture, u *models.User) {
		f.must(f.do("DELETE", "/api/v1/workspaces/"+f.wsSlug+"/members/"+u.ID, f.ownerTok, nil), http.StatusNoContent, "remove the inviter")
	}},
	{"demoted to editor", func(f *accessFixture, u *models.User) {
		f.must(f.do("PATCH", "/api/v1/workspaces/"+f.wsSlug+"/members/"+u.ID, f.ownerTok, map[string]any{"role": "editor"}), http.StatusOK, "demote the inviter")
	}},
	{"demoted to viewer", func(f *accessFixture, u *models.User) {
		f.must(f.do("PATCH", "/api/v1/workspaces/"+f.wsSlug+"/members/"+u.ID, f.ownerTok, map[string]any{"role": "viewer"}), http.StatusOK, "demote the inviter")
	}},
}

// bug3770Inviter adds a second, unrestricted owner who sends an invitation,
// then applies loss (nil: keeps the authority).
func bug3770Inviter(t *testing.T, f *accessFixture, email string, loss func(*accessFixture, *models.User)) *models.WorkspaceInvitation {
	t.Helper()
	co := mkUser(t, f.srv, "co-owner-3770@example.com")
	if err := f.srv.store.AddWorkspaceMember(f.wsID, co.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	// Through the store, which returns the plaintext code (only its hash is
	// kept); the HTTP invite door is BUG-3634's and unchanged here.
	inv, err := f.srv.store.CreateInvitation(f.wsID, email, "editor", co.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loss != nil {
		loss(f, co)
	}
	return inv
}

func TestBUG3770_EveryAcceptDoorRefusesAnInviterWhoLostTheAuthority(t *testing.T) {
	type door struct {
		name string
		// run tries the door as invitee (nil for the register door, which
		// creates the account) and returns the status and error code.
		run func(t *testing.T, f *accessFixture, inv *models.WorkspaceInvitation, invitee *models.User) (int, string)
	}
	errorCode := func(t *testing.T, raw []byte) string {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &body)
		return body.Error.Code
	}
	doors := []door{
		{"by id from the pending list", func(t *testing.T, f *accessFixture, inv *models.WorkspaceInvitation, invitee *models.User) (int, string) {
			if ids := myInvitationIDs(t, f, f.token(invitee)); len(ids) != 0 {
				t.Errorf("the pending list still offers %d invitation(s) the accept refuses", len(ids))
			}
			rr := f.do("POST", "/api/v1/me/invitations/"+inv.ID+"/accept", f.token(invitee), nil)
			return rr.Code, errorCode(t, rr.Body.Bytes())
		}},
		{"by code", func(t *testing.T, f *accessFixture, inv *models.WorkspaceInvitation, invitee *models.User) (int, string) {
			rr := f.do("POST", "/api/v1/invitations/"+inv.Code+"/accept", f.token(invitee), nil)
			return rr.Code, errorCode(t, rr.Body.Bytes())
		}},
		{"register through the code", func(t *testing.T, f *accessFixture, inv *models.WorkspaceInvitation, _ *models.User) (int, string) {
			rr := doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
				"email": inv.Email, "name": "Newcomer",
				"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
			})
			return rr.Code, errorCode(t, rr.Body.Bytes())
		}},
	}
	for _, loss := range bug3770Losses {
		for _, dr := range doors {
			t.Run(loss.name+"/"+dr.name, func(t *testing.T) {
				forEachDialect(t, func(t *testing.T, d store.DriverType) {
					f := newAccessFixture(t, d)
					email := "invitee-3770@example.com"
					var invitee *models.User
					if dr.name != "register through the code" {
						invitee = mkUser(t, f.srv, email)
					}
					inv := bug3770Inviter(t, f, email, loss.lose)

					// The preview says so, and still finds the code.
					rr := f.do("GET", "/api/v1/invitations/"+inv.Code+"/preview", "", nil)
					f.must(rr, http.StatusOK, "preview")
					var preview struct {
						Found           bool  `json:"found"`
						InviterCanGrant *bool `json:"inviter_can_grant"`
					}
					parseJSON(t, rr, &preview)
					if !preview.Found || preview.InviterCanGrant == nil || *preview.InviterCanGrant {
						t.Errorf("preview found=%v inviter_can_grant=%v, want found and false", preview.Found, preview.InviterCanGrant)
					}

					code, errCode := dr.run(t, f, inv, invitee)
					if code != http.StatusForbidden || errCode != inviterScopeChangedCode {
						t.Fatalf("accept: %d %q, want 403 %q", code, errCode, inviterScopeChangedCode)
					}
					u, _ := f.srv.store.GetUserByEmail(email)
					if invitee == nil && u != nil {
						t.Error("the refused signup left its account behind (the invitation was its admission)")
					}
					if u != nil {
						if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, u.ID); m != nil {
							t.Fatalf("the invitee joined as %q", m.Role)
						}
					}
					if got, _ := f.srv.store.GetInvitation(inv.ID); got == nil || got.AcceptedAt != nil {
						t.Errorf("the refused invitation is not left pending: %+v", got)
					}
				})
			})
		}
	}
}

// Control, per door: an inviter who is still an owner is offered and accepted
// as before, so the refusal above is the loss and not the fixture.
func TestBUG3770_AnInviterStillAnOwnerIsAcceptedAtEveryDoor(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		for _, dr := range []string{"by id", "by code", "register"} {
			f := newAccessFixture(t, d)
			email := "kept-3770@example.com"
			var invitee *models.User
			if dr != "register" {
				invitee = mkUser(t, f.srv, email)
			}
			inv := bug3770Inviter(t, f, email, nil)
			rr := f.do("GET", "/api/v1/invitations/"+inv.Code+"/preview", "", nil)
			var preview struct {
				InviterCanGrant *bool `json:"inviter_can_grant"`
			}
			parseJSON(t, rr, &preview)
			if preview.InviterCanGrant == nil || !*preview.InviterCanGrant {
				t.Errorf("%s: preview inviter_can_grant=%v, want true", dr, preview.InviterCanGrant)
			}
			switch dr {
			case "by id":
				if ids := myInvitationIDs(t, f, f.token(invitee)); len(ids) != 1 {
					t.Fatalf("pending list: %d, want 1", len(ids))
				}
				f.must(f.do("POST", "/api/v1/me/invitations/"+inv.ID+"/accept", f.token(invitee), nil), http.StatusOK, "accept by id")
			case "by code":
				f.must(f.do("POST", "/api/v1/invitations/"+inv.Code+"/accept", f.token(invitee), nil), http.StatusOK, "accept by code")
			case "register":
				rr := doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
					"email": email, "name": "Kept", "password": "correct-horse-battery-staple", "invitation_code": inv.Code,
				})
				f.must(rr, http.StatusCreated, "register through the code")
			}
			u, _ := f.srv.store.GetUserByEmail(email)
			if u == nil {
				t.Fatalf("%s: no account", dr)
			}
			if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, u.ID); m == nil || m.Role != "editor" {
				t.Fatalf("%s: membership %+v, want editor", dr, m)
			}
		}
	})
}
