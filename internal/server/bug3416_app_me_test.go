package server

import (
	"encoding/json"
	"net/http"
	"strings"
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

// A code issued before its workspace was soft-deleted is refused, not
// spent: the workspace is resolved live inside the redeem's transaction,
// before the code is consumed (codex r1 on BUG-3416).
func TestBug3416_RedeemRefusesASoftDeletedWorkspace(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	if _, err := e.srv.store.DB().Exec(`UPDATE workspaces SET deleted_at = '2026-01-01T00:00:00Z' WHERE id = ?`, e.wsID); err != nil {
		t.Fatal(err)
	}
	if rr := redeem(e.srv, `{"code":"`+out.InstallCode+`"}`); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_install_code") {
		t.Fatalf("redeem into a deleted workspace: %d %s", rr.Code, rr.Body.String())
	}
	var consumed int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM app_install_codes WHERE install_id = ? AND consumed_at IS NOT NULL`, out.InstallID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 0 {
		t.Error("the refused redeem consumed the code")
	}
}

// A database failure while reading the install is a 500 the app may retry,
// not invalid_install_code, which would tell it the code is bad (BUG-3416).
func TestBug3416_RedeemReportsADatabaseFailureAsAFault(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	db := e.srv.store.DB()
	// The install table goes missing under the redeem: the code row still
	// resolves, the install read fails.
	if _, err := db.Exec(`ALTER TABLE app_installs RENAME TO app_installs_gone`); err != nil {
		t.Fatal(err)
	}
	rr = redeem(e.srv, `{"code":"`+out.InstallCode+`"}`)
	if _, err := db.Exec(`ALTER TABLE app_installs_gone RENAME TO app_installs`); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusInternalServerError || strings.Contains(rr.Body.String(), "invalid_install_code") {
		t.Fatalf("a database failure answered %d %s, want 500", rr.Code, rr.Body.String())
	}
	// The code was not spent: it redeems once the database is back.
	if rr := redeem(e.srv, `{"code":"`+out.InstallCode+`"}`); rr.Code != http.StatusOK {
		t.Fatalf("redeem after the fault: %d %s", rr.Code, rr.Body.String())
	}
}
