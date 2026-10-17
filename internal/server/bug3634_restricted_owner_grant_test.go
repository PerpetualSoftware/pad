package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3634: an owner restricted to some collections cannot grant more access
// than it holds. An invitation carries no scope, and accepting one makes a
// member with collection_access 'all'; a role change raises what the target
// may do across ITS scope.

// bug3634Restricted adds a second owner restricted to the tasks collection.
func bug3634Restricted(t *testing.T, f *accessFixture) (*models.User, string, string) {
	t.Helper()
	tasks, err := f.srv.store.GetCollectionBySlug(f.wsID, "tasks")
	if err != nil || tasks == nil {
		t.Fatalf("tasks collection: %v", err)
	}
	co := mkUser(t, f.srv, "restricted-owner@example.com")
	if err := f.srv.store.AddWorkspaceMember(f.wsID, co.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.wsID, co.ID, "specific", []string{tasks.ID}); err != nil {
		t.Fatal(err)
	}
	return co, f.token(co), tasks.ID
}

func TestBUG3634_RestrictedOwnerCannotInvite(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		_, coTok, _ := bug3634Restricted(t, f)
		for _, role := range []string{"owner", "editor", "viewer"} {
			rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", coTok,
				map[string]any{"email": "second-" + role + "@example.com", "role": role})
			if rr.Code != http.StatusForbidden {
				t.Errorf("restricted owner invites a %s: %d %s, want 403", role, rr.Code, rr.Body.String())
			}
		}
		// Control: the unrestricted owner still invites.
		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "control@example.com", "role": "editor"})
		f.must(rr, http.StatusCreated, "unrestricted owner invites")
	})
}

// An invitation a restricted owner created before this fix, or created
// while unrestricted and restricted since, grants nothing when accepted.
func TestBUG3634_InvitationFromARestrictedInviterJoinsNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		co, _, _ := bug3634Restricted(t, f)
		invitee := mkUser(t, f.srv, "second-account@example.com")
		inv, err := f.srv.store.CreateInvitation(f.wsID, invitee.Email, "owner", co.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Not offered (BUG-3770): the list hides an invitation the accept
		// refuses. The id still reaches the door, which refuses it.
		if ids := myInvitationIDs(t, f, f.token(invitee)); len(ids) != 0 {
			t.Fatalf("invitee is offered %d invitations, want 0", len(ids))
		}
		rr := f.do("POST", "/api/v1/me/invitations/"+inv.ID+"/accept", f.token(invitee), nil)
		if rr.Code == http.StatusOK {
			t.Errorf("accept of a restricted inviter's invitation: 200 %s", rr.Body.String())
		}
		if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, invitee.ID); m != nil {
			t.Fatalf("the invitee joined with role %q access %q", m.Role, m.CollectionAccess)
		}
		// Control: the unrestricted owner's invitation still joins.
		other := mkUser(t, f.srv, "control-accept@example.com")
		if _, err := f.srv.store.CreateInvitation(f.wsID, other.Email, "editor", f.owner.ID); err != nil {
			t.Fatal(err)
		}
		ids := myInvitationIDs(t, f, f.token(other))
		rr = f.do("POST", "/api/v1/me/invitations/"+ids[0]+"/accept", f.token(other), nil)
		f.must(rr, http.StatusOK, "accept an unrestricted owner's invitation")
	})
}

func TestBUG3634_RestrictedOwnerChangesRolesOnlyWithinItsScope(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		_, coTok, tasksID := bug3634Restricted(t, f)
		wide := mkUser(t, f.srv, "wide-viewer@example.com")
		if err := f.srv.store.AddWorkspaceMember(f.wsID, wide.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
		narrow := mkUser(t, f.srv, "narrow-viewer@example.com")
		if err := f.srv.store.AddWorkspaceMember(f.wsID, narrow.ID, "viewer"); err != nil {
			t.Fatal(err)
		}
		if err := f.srv.store.SetMemberCollectionAccess(f.wsID, narrow.ID, "specific", []string{tasksID}); err != nil {
			t.Fatal(err)
		}
		base := "/api/v1/workspaces/" + f.wsSlug + "/members/"
		rr := f.do("PATCH", base+wide.ID, coTok, map[string]any{"role": "owner"})
		if rr.Code == http.StatusOK {
			t.Errorf("restricted owner promoted an unrestricted viewer: 200 %s", rr.Body.String())
		}
		if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, wide.ID); m == nil || m.Role != "viewer" {
			t.Fatalf("the unrestricted viewer's role changed: %+v", m)
		}
		// Within its scope it still manages roles.
		rr = f.do("PATCH", base+narrow.ID, coTok, map[string]any{"role": "editor"})
		f.must(rr, http.StatusOK, "restricted owner promotes a member inside its scope")
		// Control: the unrestricted owner promotes anyone.
		rr = f.do("PATCH", base+wide.ID, f.ownerTok, map[string]any{"role": "editor"})
		f.must(rr, http.StatusOK, "unrestricted owner promotes")
	})
}

// The register-with-code door refuses the same invitation. Self-hosted, the
// invitation is the signup's admission, so the account is rolled back.
func TestBUG3634_RegisterWithARestrictedInvitersCodeIsRefused(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		co, _, _ := bug3634Restricted(t, f)
		inv, err := f.srv.store.CreateInvitation(f.wsID, "newcomer@example.com", "owner", co.ID)
		if err != nil {
			t.Fatal(err)
		}
		rr := doRequest(f.srv, "POST", "/api/v1/auth/register", map[string]string{
			"email": "newcomer@example.com", "name": "Newcomer",
			"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
		})
		if rr.Code != http.StatusForbidden {
			t.Fatalf("signup with a restricted inviter's code: %d %s, want 403", rr.Code, rr.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		parseJSON(t, rr, &body)
		if body.Error.Code != inviterScopeChangedCode {
			t.Errorf("refusal code %q, want %q: %s", body.Error.Code, inviterScopeChangedCode, rr.Body.String())
		}
		if u, _ := f.srv.store.GetUserByEmail("newcomer@example.com"); u != nil {
			t.Error("the refused signup left its account behind")
		}
	})
}

// Where registration is open, the account is kept and joins nothing, and
// the response says why.
func TestBUG3634_OpenRegistrationKeepsTheAccountAndSaysWhy(t *testing.T) {
	srv, _ := newCloudEmailServer(t)
	admin, err := srv.store.GetUserByEmail("admin@pad.test")
	if err != nil || admin == nil {
		t.Fatalf("admin lookup: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Cloud 3634", OwnerID: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	co := mkUser(t, srv, "cloud-restricted@example.com")
	if err := srv.store.AddWorkspaceMember(ws.ID, co.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, co.ID, "specific", []string{}); err != nil {
		t.Fatal(err)
	}
	inv, err := srv.store.CreateInvitation(ws.ID, "open@example.com", "owner", co.ID)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
		"email": "open@example.com", "name": "Open",
		"password": "correct-horse-battery-staple", "invitation_code": inv.Code,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("open-registration signup: %d %s, want 201 (account kept)", rr.Code, rr.Body.String())
	}
	var body notJoinedBody
	parseJSON(t, rr, &body)
	if body.NotJoined == nil || body.NotJoined.Code != inviterScopeChangedCode {
		t.Errorf("invitation_not_joined = %+v, want code %q", body.NotJoined, inviterScopeChangedCode)
	}
	u, _ := srv.store.GetUserByEmail("open@example.com")
	if u == nil {
		t.Fatal("the account was not kept")
	}
	if m, _ := srv.store.GetWorkspaceMember(ws.ID, u.ID); m != nil {
		t.Errorf("the kept account joined with access %q", m.CollectionAccess)
	}
}
