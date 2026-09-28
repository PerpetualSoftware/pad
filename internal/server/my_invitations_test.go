package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3277 (PLAN-3002 U4b): GET /me/invitations and
// POST /me/invitations/{id}/accept, on both dialects.

type myInvitationsBody struct {
	Invitations   []store.MyInvitation `json:"invitations"`
	EmailVerified bool                 `json:"email_verified"`
}

func (f *accessFixture) invite(email, role string) *models.WorkspaceInvitation {
	f.t.Helper()
	inv, err := f.srv.store.CreateInvitation(f.wsID, email, role, f.owner.ID)
	if err != nil {
		f.t.Fatalf("CreateInvitation: %v", err)
	}
	return inv
}

func (f *accessFixture) myInvitations(tok string) myInvitationsBody {
	f.t.Helper()
	rr := f.do("GET", "/api/v1/me/invitations", tok, nil)
	f.must(rr, http.StatusOK, "list my invitations")
	var body myInvitationsBody
	parseJSON(f.t, rr, &body)
	return body
}

func invitationIDs(invs []store.MyInvitation) []string {
	ids := []string{}
	for _, inv := range invs {
		ids = append(ids, inv.ID)
	}
	sort.Strings(ids)
	return ids
}

func sortedIDs(ids ...string) []string {
	sort.Strings(ids)
	return ids
}

func mkUnverifiedUser(t *testing.T, srv *Server, email string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: email, Password: "correct-horse-battery-staple", Role: "member", Unverified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	if u.IsEmailVerified() {
		t.Fatalf("precondition: %s was created verified", email)
	}
	return u
}

func expireInvitation(t *testing.T, srv *Server, id string) {
	t.Helper()
	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(
		`UPDATE workspace_invitations SET expires_at = ? WHERE id = ?`), past, id); err != nil {
		t.Fatalf("expire invitation: %v", err)
	}
}

func TestMyInvitations_ListFilters(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		me := mkUser(t, f.srv, "me@example.com")
		meTok := f.token(me)
		other := mkUser(t, f.srv, "other@example.com")

		// The fixture owner has no username; give it one so the owner join
		// is measured against a value it could get wrong.
		if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(
			`UPDATE users SET username = ? WHERE id = ?`), "ownerhandle", f.owner.ID); err != nil {
			t.Fatalf("set owner username: %v", err)
		}
		f.owner.Username = "ownerhandle"

		mine := f.invite("me@example.com", "editor")
		// Case differs only in how the inviter typed it; stored lowercased.
		mineUpper := f.invite("  ME@Example.com ", "viewer")
		f.invite("other@example.com", "editor")
		expired := f.invite("me@example.com", "editor")
		expireInvitation(t, f.srv, expired.ID)
		accepted := f.invite("me@example.com", "editor")
		if err := f.srv.store.AcceptInvitation(accepted.ID); err != nil {
			t.Fatalf("AcceptInvitation: %v", err)
		}

		got := f.myInvitations(meTok)
		if !got.EmailVerified {
			t.Fatalf("verified caller reported email_verified=false")
		}
		if want := sortedIDs(mine.ID, mineUpper.ID); !reflect.DeepEqual(invitationIDs(got.Invitations), want) {
			t.Fatalf("listed %v, want %v (not expired %s, not accepted %s, not other's)",
				invitationIDs(got.Invitations), want, expired.ID, accepted.ID)
		}
		for _, inv := range got.Invitations {
			if inv.WorkspaceSlug != f.wsSlug || inv.WorkspaceName != "Access WS" || inv.WorkspaceOwnerUsername != f.owner.Username {
				t.Fatalf("entry lacks the workspace it opens: %+v", inv)
			}
			if inv.InvitedByName != f.owner.Name {
				t.Fatalf("invited_by_name = %q, want %q", inv.InvitedByName, f.owner.Name)
			}
		}

		// The other user sees only their own.
		if ids := invitationIDs(f.myInvitations(f.token(other)).Invitations); len(ids) != 1 {
			t.Fatalf("other user listed %v, want exactly their one invitation", ids)
		}

		// No code, hashed or otherwise, is ever in the list.
		rr := f.do("GET", "/api/v1/me/invitations", meTok, nil)
		var raw map[string][]map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &raw); err == nil {
			for _, e := range raw["invitations"] {
				if _, ok := e["code"]; ok {
					t.Fatalf("list entry carries a code: %v", e)
				}
			}
		}
	})
}

func TestMyInvitations_UnverifiedListsNothing(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		// Someone registered with an address they have not proven.
		squatter := mkUnverifiedUser(t, f.srv, "victim@example.com")
		inv := f.invite("victim@example.com", "editor")

		got := f.myInvitations(f.token(squatter))
		if got.EmailVerified || len(got.Invitations) != 0 {
			t.Fatalf("unverified caller got %+v, want an empty list with email_verified=false", got)
		}
		// Control: the same invitation IS listed once the address is verified,
		// so the empty list above is the verification filter, not a miss.
		if err := f.srv.store.SetUserEmailVerified(squatter.ID); err != nil {
			t.Fatalf("SetUserEmailVerified: %v", err)
		}
		if ids := invitationIDs(f.myInvitations(f.token(squatter)).Invitations); !reflect.DeepEqual(ids, []string{inv.ID}) {
			t.Fatalf("after verification listed %v, want [%s]", ids, inv.ID)
		}
	})
}

func TestMyInvitations_ExcludesMembersAndDeletedWorkspaces(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		mem := f.member("mem@example.com", "viewer")
		f.invite("mem@example.com", "editor")
		if ids := invitationIDs(f.myInvitations(f.token(mem)).Invitations); len(ids) != 0 {
			t.Fatalf("a member was listed an invitation to their own workspace: %v", ids)
		}

		me := mkUser(t, f.srv, "me@example.com")
		f.invite("me@example.com", "editor")
		if n := len(f.myInvitations(f.token(me)).Invitations); n != 1 {
			t.Fatalf("precondition: listed %d, want 1", n)
		}
		if err := f.srv.store.DeleteWorkspace(f.wsSlug); err != nil {
			t.Fatalf("DeleteWorkspace: %v", err)
		}
		if ids := invitationIDs(f.myInvitations(f.token(me)).Invitations); len(ids) != 0 {
			t.Fatalf("invitation to a soft-deleted workspace listed: %v", ids)
		}
	})
}

func TestMyInvitations_AcceptByID(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		me := mkUser(t, f.srv, "me@example.com")
		meTok := f.token(me)
		inv := f.invite("me@example.com", "editor")

		since := f.mark()
		rr := f.do("POST", "/api/v1/me/invitations/"+inv.ID+"/accept", meTok, nil)
		f.must(rr, http.StatusOK, "accept by id")
		var body map[string]any
		parseJSON(t, rr, &body)
		if body["workspace_slug"] != f.wsSlug || body["role"] != "editor" {
			t.Fatalf("accept body = %v", body)
		}
		if m, err := f.srv.store.GetWorkspaceMember(f.wsID, me.ID); err != nil || m == nil || m.Role != "editor" {
			t.Fatalf("membership after accept = %+v, %v", m, err)
		}
		f.expect(since, f.line("gained", "me@example.com"))
		if n := len(f.myInvitations(meTok).Invitations); n != 0 {
			t.Fatalf("accepted invitation still listed (%d)", n)
		}
		// Accepting again is the same 404 as any id that is not pending.
		f.must(f.do("POST", "/api/v1/me/invitations/"+inv.ID+"/accept", meTok, nil), http.StatusNotFound, "re-accept")
	})
}

func TestAcceptInvitation_ExistingMemberIsIdempotent(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		member := f.member("member@example.com", "viewer")
		tok := f.token(member)
		inv := f.invite(member.Email, "editor")
		since := f.mark()

		rr := f.do("POST", "/api/v1/invitations/"+inv.Code+"/accept", tok, nil)
		f.must(rr, http.StatusOK, "accept as existing member")
		var body map[string]any
		parseJSON(t, rr, &body)
		if body["workspace_slug"] != f.wsSlug {
			t.Fatalf("workspace_slug = %v, want %q", body["workspace_slug"], f.wsSlug)
		}
		got, err := f.srv.store.GetWorkspaceMember(f.wsID, member.ID)
		if err != nil || got == nil || got.Role != "viewer" {
			t.Fatalf("membership = %+v, %v; existing viewer role must remain unchanged", got, err)
		}
		if pending, err := f.srv.store.GetInvitationByCode(inv.Code); err != nil || pending != nil {
			t.Fatalf("invitation remains pending: %+v, %v", pending, err)
		}
		f.expect(since)
	})
}

func TestAcceptInvitation_ConcurrentCoreAcceptsAreIdempotent(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		member := mkUser(t, f.srv, "race@example.com")
		inv := f.invite(member.Email, "editor")
		// Both requests have already read the pending invitation, as happens
		// when the code and by-id doors race. Calling the shared core directly
		// makes that read timing deterministic for both dialects.
		start := make(chan struct{})
		results := make(chan *httptest.ResponseRecorder, 2)
		for i := 0; i < 2; i++ {
			go func() {
				<-start
				rec := httptest.NewRecorder()
				ok := f.srv.acceptInvitationCore(rec, httptest.NewRequest("POST", "/", nil), inv, member)
				if !ok {
					rec.WriteHeader(http.StatusInternalServerError)
				}
				results <- rec
			}()
		}
		close(start)
		for i := 0; i < 2; i++ {
			if rr := <-results; rr.Code != http.StatusOK {
				t.Fatalf("concurrent accept = %d %s, want 200", rr.Code, rr.Body.String())
			}
		}
		var count int
		if err := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(`SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), f.wsID, member.ID).Scan(&count); err != nil {
			t.Fatalf("count membership: %v", err)
		}
		if count != 1 {
			t.Fatalf("membership rows = %d, want one", count)
		}
		if pending, err := f.srv.store.GetInvitationByCode(inv.Code); err != nil || pending != nil {
			t.Fatalf("invitation remains pending: %+v, %v", pending, err)
		}
	})
}

func TestMyInvitations_AcceptByIDRefusals(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		me := mkUser(t, f.srv, "me@example.com")
		meTok := f.token(me)
		mkUser(t, f.srv, "other@example.com")
		others := f.invite("other@example.com", "editor")

		// Someone else's invitation and an id that does not exist answer
		// byte-identically, so an id cannot be probed.
		theirs := f.do("POST", "/api/v1/me/invitations/"+others.ID+"/accept", meTok, nil)
		unknown := f.do("POST", "/api/v1/me/invitations/does-not-exist/accept", meTok, nil)
		f.must(theirs, http.StatusNotFound, "someone else's invitation")
		f.must(unknown, http.StatusNotFound, "unknown id")
		if theirs.Body.String() != unknown.Body.String() {
			t.Fatalf("someone else's id and an unknown id answer differently:\n%s\n%s", theirs.Body, unknown.Body)
		}
		if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, me.ID); m != nil {
			t.Fatalf("refused accept left a membership: %+v", m)
		}

		expired := f.invite("me@example.com", "editor")
		expireInvitation(t, f.srv, expired.ID)
		f.must(f.do("POST", "/api/v1/me/invitations/"+expired.ID+"/accept", meTok, nil), http.StatusGone, "expired")

		// An unverified caller is refused, and the door does NOT verify them,
		// unlike the code door: an id from the list proves nothing.
		unv := mkUnverifiedUser(t, f.srv, "unv@example.com")
		unvInv := f.invite("unv@example.com", "editor")
		f.must(f.do("POST", "/api/v1/me/invitations/"+unvInv.ID+"/accept", f.token(unv), nil), http.StatusForbidden, "unverified")
		after, err := f.srv.store.GetUser(unv.ID)
		if err != nil {
			t.Fatalf("GetUser: %v", err)
		}
		if after.IsEmailVerified() {
			t.Fatalf("the by-id door verified an unverified account")
		}
		if m, _ := f.srv.store.GetWorkspaceMember(f.wsID, unv.ID); m != nil {
			t.Fatalf("unverified accept left a membership: %+v", m)
		}
	})
}

// The by-id and by-code doors share acceptInvitationCore; this pins that what
// they produce is the same: the response, the membership row, the
// invitation's state, and the access notification.
func TestMyInvitations_AcceptPathsMatch(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		type outcome struct {
			Body       map[string]any
			MemberRole string
			Pending    int
			Notified   []string
		}
		run := func(byID bool) outcome {
			f := newAccessFixture(t, d)
			u := mkUser(t, f.srv, "invitee@example.com")
			tok := f.token(u)
			inv := f.invite("invitee@example.com", "viewer")
			since := f.mark()
			path := "/api/v1/invitations/" + inv.Code + "/accept"
			if byID {
				path = "/api/v1/me/invitations/" + inv.ID + "/accept"
			}
			rr := f.do("POST", path, tok, nil)
			f.must(rr, http.StatusOK, path)
			var o outcome
			parseJSON(t, rr, &o.Body)
			// Per-fixture identifiers differ by construction.
			if o.Body["workspace_id"] != f.wsID {
				t.Fatalf("%s: workspace_id = %v, want %s", path, o.Body["workspace_id"], f.wsID)
			}
			delete(o.Body, "workspace_id")
			if m, err := f.srv.store.GetWorkspaceMember(f.wsID, u.ID); err == nil && m != nil {
				o.MemberRole = m.Role
			}
			pending, err := f.srv.store.ListWorkspaceInvitations(f.wsID)
			if err != nil {
				t.Fatalf("ListWorkspaceInvitations: %v", err)
			}
			o.Pending = len(pending)
			for _, l := range f.accessSince(since) {
				o.Notified = append(o.Notified, l[:len(l)-len(f.wsID)])
			}
			return o
		}
		byCode, byID := run(false), run(true)
		if !reflect.DeepEqual(byCode, byID) {
			t.Fatalf("accept paths differ:\n by code: %+v\n by id:   %+v", byCode, byID)
		}
		if byID.MemberRole != "viewer" || byID.Pending != 0 || len(byID.Notified) != 1 {
			t.Fatalf("both paths agree on a wrong outcome: %+v", byID)
		}
	})
}
