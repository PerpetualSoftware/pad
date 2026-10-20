package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3861: the invite door passed any role straight to CreateInvitation, so
// an unknown one reached the invitations CHECK constraint and answered 500.
// It now answers the role-change door's 400 and writes nothing. The known
// roles, and the empty role's editor default, still invite.
func TestBUG3861_InviteRoleMustBeKnown(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		invitePath := "/api/v1/workspaces/" + f.wsSlug + "/members/invite"

		for _, role := range []string{"admin", "Owner", "superuser"} {
			rr := f.do("POST", invitePath, f.ownerTok,
				map[string]any{"email": "alex@example.com", "role": role})
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("role %q: got %d %s, want 400", role, rr.Code, rr.Body.String())
			}
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatal(err)
			}
			if env.Error.Code != "bad_request" || env.Error.Message != "role must be owner, editor or viewer" {
				t.Fatalf("role %q: error %+v, want the role-change door's 400", role, env.Error)
			}
		}
		invs, err := f.srv.store.ListWorkspaceInvitations(f.wsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(invs) != 0 {
			t.Fatalf("a refused role wrote an invitation: %+v", invs)
		}

		// Control: every known role invites, and an empty role defaults to
		// editor, so the check refuses only what the constraint would.
		want := map[string]string{
			"o@example.com": "owner",
			"e@example.com": "editor",
			"v@example.com": "viewer",
			"d@example.com": "",
		}
		for email, role := range want {
			f.must(f.do("POST", invitePath, f.ownerTok,
				map[string]any{"email": email, "role": role}), http.StatusCreated, "invite "+email)
		}
		invs, err = f.srv.store.ListWorkspaceInvitations(f.wsID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, inv := range invs {
			got[inv.Email] = inv.Role
		}
		want["d@example.com"] = "editor"
		for email, role := range want {
			if got[email] != role {
				t.Errorf("%s: invitation role %q, want %q (all: %v)", email, got[email], role, got)
			}
		}
	})
}
