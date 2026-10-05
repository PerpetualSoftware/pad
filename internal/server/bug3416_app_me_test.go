package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// BUG-3416: an app learns its install and workspace from the unscoped
// GET /api/app/v1/me, answered from the token's binding. A delegated token
// never sees the install-code redeem, so this is its only way in.
func TestBug3416_UnscopedMeNamesTheBinding(t *testing.T) {
	check := func(t *testing.T, f appAPIFix, wantKind, wantUser string, wantApp bool) {
		t.Helper()
		rr := appGet(f.srv, appAPIPrefix+"/me", f.token)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /me: %d %s", rr.Code, rr.Body.String())
		}
		var raw map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &raw)
		want := []string{"install_id", "workspace_id", "workspace_slug", "auth_kind", "access", "user_id", "display_name", "is_app"}
		if len(raw) != len(want) {
			t.Errorf("/me keys %v, want exactly %v", appKeysOf(raw), want)
		}
		var me AppInstallMe
		_ = json.Unmarshal(rr.Body.Bytes(), &me)
		if me.InstallID != f.in.id || me.WorkspaceID != f.ws.ID || me.WorkspaceSlug != f.ws.Slug ||
			me.AuthKind != wantKind || me.UserID != wantUser || me.IsApp != wantApp || me.Access == "" {
			t.Errorf("/me = %+v", me)
		}
		// The workspace it names is the {ws} the scoped routes take.
		if rr := appGet(f.srv, appAPIPrefix+"/workspaces/"+me.WorkspaceID+"/me", f.token); rr.Code != http.StatusOK {
			t.Errorf("scoped /me with the named workspace: %d %s", rr.Code, rr.Body.String())
		}
	}
	t.Run("service token", func(t *testing.T) {
		f := appAPIFixture(t, "read")
		check(t, f, "service", f.in.bot.ID, true)
	})
	t.Run("delegated token", func(t *testing.T) {
		f := delegatedAPIFixture(t, "write", "write", "editor")
		check(t, f.appAPIFix, "delegated", f.person.ID, false)
	})
	t.Run("no token", func(t *testing.T) {
		f := appAPIFixture(t, "read")
		if rr := appGet(f.srv, appAPIPrefix+"/me", ""); rr.Code != http.StatusUnauthorized {
			t.Errorf("GET /me without a token: %d, want 401", rr.Code)
		}
	})
}
