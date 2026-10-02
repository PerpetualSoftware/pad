package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3348: an account whose email is UNVERIFIED has proven nothing about
// that address. Anyone may register victim@x.com on a self-serve instance,
// so every door that grants access BY EMAIL must treat such an account as
// "no account for this address", never as its owner. Each leg below drives
// the door with a squatter and, as its control, with the same address once
// verified — so a refusal is the verification check, not a broken door.

func TestBUG3348_InviteDoesNotDirectAddUnverifiedAccount(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		squatter := mkUnverifiedUser(t, f.srv, "victim@example.com")

		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "victim@example.com", "role": "editor"})
		f.must(rr, http.StatusCreated, "invite unverified")
		var body struct {
			Added   bool   `json:"added"`
			Invited bool   `json:"invited"`
			Code    string `json:"code"`
		}
		parseJSON(t, rr, &body)
		if body.Added || !body.Invited || body.Code == "" {
			t.Fatalf("invite of an unverified account: got %s, want an emailed invitation, not a direct add", rr.Body.String())
		}
		if member, err := f.srv.store.IsWorkspaceMember(f.wsID, squatter.ID); err != nil || member {
			t.Fatalf("unverified squatter became a member (member=%v err=%v)", member, err)
		}
		// The squatter cannot read the workspace.
		if rr := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/collections", f.token(squatter), nil); rr.Code == http.StatusOK {
			t.Fatalf("unverified squatter read the workspace: %d %s", rr.Code, rr.Body.String())
		}

		// Control: a verified account is still added directly.
		mkUser(t, f.srv, "real@example.com")
		rr = f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/members/invite", f.ownerTok,
			map[string]any{"email": "real@example.com", "role": "editor"})
		f.must(rr, http.StatusCreated, "invite verified")
		parseJSON(t, rr, &body)
		if !body.Added {
			t.Fatalf("verified account was not added directly: %s", rr.Body.String())
		}
	})
}

func TestBUG3348_GrantByEmailRefusesUnverifiedAccount(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		squatter := mkUnverifiedUser(t, f.srv, "victim@example.com")
		it := f.seedItem()
		collPath := "/api/v1/workspaces/" + f.wsSlug + "/collections/tasks/grants"
		itemPath := "/api/v1/workspaces/" + f.wsSlug + "/items/" + it.Slug + "/grants"

		// The refusal is byte-identical to an address with no account.
		unknown := f.do("POST", collPath, f.ownerTok, map[string]any{"email": "nobody@example.com"})
		f.must(unknown, http.StatusNotFound, "collection grant to unknown email")
		for _, path := range []string{collPath, itemPath} {
			rr := f.do("POST", path, f.ownerTok, map[string]any{"email": "victim@example.com"})
			if rr.Code != http.StatusNotFound || rr.Body.String() != unknown.Body.String() {
				t.Fatalf("grant by email to unverified account at %s: got %d %s, want the unknown-email 404 %s",
					path, rr.Code, rr.Body.String(), unknown.Body.String())
			}
		}
		if rr := f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug, f.token(squatter), nil); rr.Code == http.StatusOK {
			t.Fatalf("unverified squatter read the item: %d", rr.Code)
		}

		// Control: once verified, both grants land.
		if err := f.srv.store.SetUserEmailVerified(squatter.ID); err != nil {
			t.Fatalf("SetUserEmailVerified: %v", err)
		}
		f.must(f.do("POST", collPath, f.ownerTok, map[string]any{"email": "victim@example.com"}), http.StatusCreated, "collection grant verified")
		f.must(f.do("POST", itemPath, f.ownerTok, map[string]any{"email": "victim@example.com"}), http.StatusCreated, "item grant verified")
	})
}

func TestBUG3348_EmailRestrictedShareLinkRefusesUnverifiedAccount(t *testing.T) {
	forEachDialect(t, func(t *testing.T, d store.DriverType) {
		f := newAccessFixture(t, d)
		squatter := mkUnverifiedUser(t, f.srv, "victim@example.com")
		it := f.seedItem()
		rr := f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/items/"+it.Slug+"/share-links", f.ownerTok,
			map[string]any{"restrict_to_email": "victim@example.com"})
		f.must(rr, http.StatusCreated, "create restricted share link")
		var link struct {
			Token string `json:"token"`
		}
		parseJSON(t, rr, &link)
		if link.Token == "" {
			t.Fatalf("share link has no token: %s", rr.Body.String())
		}

		rr = f.do("GET", "/api/v1/s/"+link.Token, f.token(squatter), nil)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("unverified squatter resolved an email-restricted link: %d %s", rr.Code, rr.Body.String())
		}

		// Control: the same account, verified, resolves it.
		if err := f.srv.store.SetUserEmailVerified(squatter.ID); err != nil {
			t.Fatalf("SetUserEmailVerified: %v", err)
		}
		f.must(f.do("GET", "/api/v1/s/"+link.Token, f.token(squatter), nil), http.StatusOK, "verified resolve")
	})
}

func TestBUG3348_OAuthLinkRefusesUnverifiedAccount(t *testing.T) {
	srv := testServer(t)
	srv.SetCloudMode(oauthProviderTestSecret)
	squatter := mkUnverifiedUser(t, srv, "victim@example.com")

	link := func() *httptest.ResponseRecorder {
		req := cloudAdminReq(t, "POST", "/api/v1/auth/oauth-link", map[string]interface{}{
			"provider":       "google",
			"email":          "victim@example.com",
			"email_verified": true,
			"cloud_secret":   oauthProviderTestSecret,
		}, map[string]string{"X-Cloud-Secret": oauthProviderTestSecret})
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		return rr
	}

	// The real owner of the address must not end up signing in, through
	// their provider, to an account somebody else registered and holds the
	// password of.
	if rr := link(); rr.Code == http.StatusOK {
		t.Fatalf("oauth-link attached a provider to an unverified account: %s", rr.Body.String())
	}
	u, err := srv.store.GetUserByEmail("victim@example.com")
	if err != nil || u == nil {
		t.Fatalf("lookup: %v", err)
	}
	if u.HasOAuthProvider("google") {
		t.Fatalf("google was linked to the unverified account")
	}

	// Control: once verified, the link lands.
	if err := srv.store.SetUserEmailVerified(squatter.ID); err != nil {
		t.Fatalf("SetUserEmailVerified: %v", err)
	}
	if rr := link(); rr.Code != http.StatusOK {
		t.Fatalf("verified oauth-link: got %d %s", rr.Code, rr.Body.String())
	}
}

// The inviter is handed the invitation code, so registering with it proves
// nothing about the address either. On cloud with a deliverable verification
// email the invited signup starts unverified and is sent the link; the
// membership still lands. Self-hosted (control) is unchanged: verified.
func TestBUG3348_RegisterWithInvitationCodeDoesNotVerifyOnCloud(t *testing.T) {
	register := func(srv *Server) {
		t.Helper()
		admin, err := srv.store.GetUserByEmail("admin@pad.test")
		if err != nil || admin == nil {
			t.Fatalf("admin lookup: %v", err)
		}
		ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Invite WS", OwnerID: admin.ID})
		if err != nil {
			t.Fatalf("CreateWorkspace: %v", err)
		}
		inv, err := srv.store.CreateInvitation(ws.ID, "victim@example.com", "editor", admin.ID)
		if err != nil {
			t.Fatalf("CreateInvitation: %v", err)
		}
		rr := doRequest(srv, "POST", "/api/v1/auth/register", map[string]string{
			"email":           "victim@example.com",
			"name":            "Victim",
			"password":        "correct-horse-battery-staple",
			"invitation_code": inv.Code,
		})
		if rr.Code != http.StatusCreated {
			t.Fatalf("register with invitation: %d %s", rr.Code, rr.Body.String())
		}
		u, err := srv.store.GetUserByEmail("victim@example.com")
		if err != nil || u == nil {
			t.Fatalf("lookup: %v", err)
		}
		if member, err := srv.store.IsWorkspaceMember(ws.ID, u.ID); err != nil || !member {
			t.Fatalf("invited signup did not join (member=%v err=%v)", member, err)
		}
	}

	t.Run("cloud", func(t *testing.T) {
		srv, mails := newCloudEmailServer(t)
		register(srv)
		u, _ := srv.store.GetUserByEmail("victim@example.com")
		if u.IsEmailVerified() {
			t.Fatal("registering with the inviter-visible code verified the account on cloud")
		}
		// The address is proven by the emailed link instead.
		select {
		case m := <-mails:
			if m.to != "victim@example.com" {
				t.Fatalf("verification mail went to %q", m.to)
			}
			extractVerifyToken(t, m)
		case <-time.After(5 * time.Second):
			t.Fatal("no verification email sent to the invited signup")
		}
	})

	t.Run("self-hosted control", func(t *testing.T) {
		srv := testServer(t)
		bootstrapFirstUser(t, srv, "admin@pad.test", "Admin")
		register(srv)
		u, _ := srv.store.GetUserByEmail("victim@example.com")
		if !u.IsEmailVerified() {
			t.Fatal("self-hosted invited signup should stay verified")
		}
	})
}
