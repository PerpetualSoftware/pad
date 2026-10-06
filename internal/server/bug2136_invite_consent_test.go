package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-2136 (lead ruling, day 87): an existing user invited to a workspace is
// asked, not added. Every invite creates a pending invitation the invitee can
// accept or decline; a re-invite replaces the pending one; decline deletes it
// and leaves an audit event.

type inviteBody struct {
	Added   bool   `json:"added"`
	Invited bool   `json:"invited"`
	Code    string `json:"code"`
	UserID  string `json:"user_id"`
	Name    string `json:"name"`
}

func myInvitationIDs(t *testing.T, f *accessFixture, tok string) []string {
	t.Helper()
	rr := f.do("GET", "/api/v1/me/invitations", tok, nil)
	f.must(rr, http.StatusOK, "list my invitations")
	var body struct {
		Invitations []struct {
			ID string `json:"id"`
		} `json:"invitations"`
	}
	parseJSON(t, rr, &body)
	ids := []string{}
	for _, inv := range body.Invitations {
		ids = append(ids, inv.ID)
	}
	return ids
}

func TestBUG2136_InviteAsksAVerifiedExistingUser(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		invitee := mkUser(t, f.srv, "existing@example.com")

		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "existing@example.com", "role": "editor"})
		f.must(rr, http.StatusCreated, "invite existing user")
		var body inviteBody
		parseJSON(t, rr, &body)
		if body.Added || !body.Invited || body.Code == "" {
			t.Fatalf("invite of an existing user: got %s, want a pending invitation", rr.Body.String())
		}
		// The response has the invitation's shape, with no direct-add keys.
		var keys map[string]any
		parseJSON(t, rr, &keys)
		for _, k := range []string{"added", "user_id", "name"} {
			if _, ok := keys[k]; ok {
				t.Errorf("invite response carries the direct-add key %q: %s", k, rr.Body.String())
			}
		}
		if member, err := f.srv.store.IsWorkspaceMember(f.wsID, invitee.ID); err != nil || member {
			t.Fatalf("the invitee became a member without accepting (member=%v err=%v)", member, err)
		}
		ids := myInvitationIDs(t, f, f.token(invitee))
		if len(ids) != 1 {
			t.Fatalf("the invitee sees %d pending invitations, want 1", len(ids))
		}
		// Accepting is what joins.
		rr = f.do("POST", "/api/v1/me/invitations/"+ids[0]+"/accept", f.token(invitee), nil)
		f.must(rr, http.StatusOK, "accept")
		if member, _ := f.srv.store.IsWorkspaceMember(f.wsID, invitee.ID); !member {
			t.Fatalf("accept did not add the member")
		}
	})
}

func TestBUG2136_ReinviteReplacesThePendingInvitation(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		invitee := mkUser(t, f.srv, "again@example.com")
		invite := func(role string) inviteBody {
			t.Helper()
			rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
				map[string]any{"email": "again@example.com", "role": role})
			f.must(rr, http.StatusCreated, "invite")
			var b inviteBody
			parseJSON(t, rr, &b)
			return b
		}
		first := invite("viewer")
		second := invite("editor")
		pending, err := f.srv.store.ListWorkspaceInvitations(f.wsID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, inv := range pending {
			if inv.Email == "again@example.com" {
				n++
				if inv.Role != "editor" {
					t.Errorf("the surviving invitation has role %q, want the re-invite's editor", inv.Role)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%d pending invitations for one address after a re-invite, want 1", n)
		}
		if rr := f.do("POST", "/api/v1/invitations/"+first.Code+"/accept", f.token(invitee), nil); rr.Code != http.StatusNotFound {
			t.Errorf("the replaced invitation's code still works: %d %s", rr.Code, rr.Body.String())
		}
		rr := f.do("POST", "/api/v1/invitations/"+second.Code+"/accept", f.token(invitee), nil)
		f.must(rr, http.StatusOK, "accept the re-invite")
	})
}

func TestBUG2136_DeclineByID(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		invitee := mkUser(t, f.srv, "decliner@example.com")
		other := mkUser(t, f.srv, "someone-else@example.com")
		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "decliner@example.com"})
		f.must(rr, http.StatusCreated, "invite")
		ids := myInvitationIDs(t, f, f.token(invitee))
		if len(ids) != 1 {
			t.Fatalf("want 1 pending invitation, got %d", len(ids))
		}
		// Another account cannot decline it, and is told nothing exists.
		unknown := f.do("POST", "/api/v1/me/invitations/no-such-id/decline", f.token(other), nil)
		f.must(unknown, http.StatusNotFound, "decline unknown id")
		if rr := f.do("POST", "/api/v1/me/invitations/"+ids[0]+"/decline", f.token(other), nil); rr.Code != http.StatusNotFound || rr.Body.String() != unknown.Body.String() {
			t.Fatalf("another account's decline: %d %s, want the unknown-id 404", rr.Code, rr.Body.String())
		}

		rr = f.do("POST", "/api/v1/me/invitations/"+ids[0]+"/decline", f.token(invitee), nil)
		if rr.Code != http.StatusNoContent && rr.Code != http.StatusOK {
			t.Fatalf("decline: %d %s", rr.Code, rr.Body.String())
		}
		if left := myInvitationIDs(t, f, f.token(invitee)); len(left) != 0 {
			t.Errorf("a declined invitation is still listed: %v", left)
		}
		if pending, _ := f.srv.store.ListWorkspaceInvitations(f.wsID); len(pending) != 0 {
			t.Errorf("the owner still sees the declined invitation as pending: %d", len(pending))
		}
		if rr := f.do("POST", "/api/v1/me/invitations/"+ids[0]+"/accept", f.token(invitee), nil); rr.Code != http.StatusNotFound {
			t.Errorf("a declined invitation can still be accepted: %d", rr.Code)
		}
		if member, _ := f.srv.store.IsWorkspaceMember(f.wsID, invitee.ID); member {
			t.Errorf("declining made the invitee a member")
		}
		acts, err := f.srv.store.ListWorkspaceActivity(f.wsID, models.ActivityListParams{Limit: 50})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, a := range acts {
			found = found || a.Action == models.ActionMemberInviteDeclined
		}
		if !found {
			t.Errorf("no %s audit event", models.ActionMemberInviteDeclined)
		}
	})
}

func TestBUG2136_DeclineByCode(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		invitee := mkUser(t, f.srv, "code-decliner@example.com")
		other := mkUser(t, f.srv, "not-the-invitee@example.com")
		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "code-decliner@example.com"})
		f.must(rr, http.StatusCreated, "invite")
		var b inviteBody
		parseJSON(t, rr, &b)

		if rr := f.do("POST", "/api/v1/invitations/"+b.Code+"/decline", f.token(other), nil); rr.Code != http.StatusForbidden {
			t.Fatalf("a different account declined by code: %d %s", rr.Code, rr.Body.String())
		}
		rr = f.do("POST", "/api/v1/invitations/"+b.Code+"/decline", f.token(invitee), nil)
		if rr.Code != http.StatusNoContent && rr.Code != http.StatusOK {
			t.Fatalf("decline by code: %d %s", rr.Code, rr.Body.String())
		}
		if rr := f.do("POST", "/api/v1/invitations/"+b.Code+"/accept", f.token(invitee), nil); rr.Code != http.StatusNotFound {
			t.Errorf("a declined invitation's code still accepts: %d", rr.Code)
		}
	})
}

// Two sibling invitations for one address (a concurrent re-invite can leave
// both on Postgres without the lock): accepting both is one membership and no
// error, because a second accept by a member is idempotent (BUG-3281).
func TestBUG2136_SiblingInvitationsAcceptCleanly(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		invitee := mkUser(t, f.srv, "twin@example.com")
		a, err := f.srv.store.CreateInvitation(f.wsID, "twin@example.com", "editor", f.owner.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Insert the sibling directly, bypassing the replace, to model the race.
		b := insertRawInvitation(t, f, d, "twin@example.com", "viewer")
		for _, id := range []string{a.ID, b} {
			rr := f.do("POST", "/api/v1/me/invitations/"+id+"/accept", f.token(invitee), nil)
			if rr.Code != http.StatusOK && rr.Code != http.StatusNotFound {
				t.Fatalf("accepting a sibling invitation: %d %s", rr.Code, rr.Body.String())
			}
		}
		members, err := f.srv.store.ListWorkspaceMembers(f.wsID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, m := range members {
			if m.UserID == invitee.ID {
				n++
			}
		}
		if n != 1 {
			t.Fatalf("%d memberships after accepting two sibling invitations, want 1", n)
		}
	})
}

// Inviting an existing member is still refused.
func TestBUG2136_InvitingAMemberIsRefused(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		m := mkUser(t, f.srv, "already@example.com")
		if err := f.srv.store.AddWorkspaceMember(f.wsID, m.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "already@example.com"})
		f.must(rr, http.StatusConflict, "invite a member")
	})
}

var rawInvitationSeq atomic.Int64

// insertRawInvitation inserts a pending invitation row directly, bypassing
// CreateInvitation's replace, to model two invites that raced past it.
func insertRawInvitation(t *testing.T, f *accessFixture, d store.DriverType, email, role string) string {
	t.Helper()
	id := fmt.Sprintf("raw-%d-%s", rawInvitationSeq.Add(1), email)
	q := `INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, code_hash, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	if d == store.DriverPostgres {
		q = `INSERT INTO workspace_invitations (id, workspace_id, email, role, invited_by, code, code_hash, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := f.srv.store.DB().Exec(q, id, f.wsID, email, role, f.owner.ID, id, "hash-"+id, ts, exp); err != nil {
		t.Fatalf("insert raw invitation: %v", err)
	}
	return id
}
