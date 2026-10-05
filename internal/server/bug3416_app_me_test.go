package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
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

// A soft delete in flight when the redeem reads the workspace: the redeem
// waits for it and then refuses, rather than spending the code on a
// workspace that is gone by its commit (codex r2 on BUG-3416). The deleter
// then touches the install row, as account deletion does through the bot's
// foreign key, which must not deadlock: the redeem locks the workspace
// before the install (codex r3). Postgres only: SQLite serialises every
// writer.
func TestBug3416_RedeemWaitsForAnInFlightSoftDelete(t *testing.T) {
	e := newAppsEnvOn(t, accountDeleteServer(t, store.DriverPostgres)) // skips without PAD_TEST_POSTGRES_URL
	e.srv.store.SetAppAPIAudience(testProvisionAudience)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)

	db := e.srv.store.DB()
	del, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer del.Rollback()
	if _, err := del.Exec(`UPDATE workspaces SET deleted_at = $1 WHERE id = $2`, "2026-01-01T00:00:00Z", e.wsID); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- redeem(e.srv, `{"code":"`+out.InstallCode+`"}`) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case rr := <-done:
			t.Fatalf("the redeem did not wait for the in-flight soft delete: %d %s", rr.Code, rr.Body.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the redeem never waited on a lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Account deletion goes on to delete the workspace's bot, whose ON DELETE
	// SET NULL updates the install row (codex r3). With the redeem holding
	// the install while it waits for the workspace, this deadlocked.
	if _, err := del.Exec(`UPDATE app_installs SET bot_user_id = NULL WHERE id = $1`, out.InstallID); err != nil {
		t.Fatalf("the deleter's install update while the redeem waits: %v", err)
	}
	if err := del.Commit(); err != nil {
		t.Fatal(err)
	}
	if rr := <-done; rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_install_code") {
		t.Fatalf("redeem after the delete committed: %d %s", rr.Code, rr.Body.String())
	}
	var consumed int
	if err := db.QueryRow(`SELECT COUNT(*) FROM app_install_codes WHERE install_id = $1 AND consumed_at IS NOT NULL`, out.InstallID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 0 {
		t.Error("the refused redeem consumed the code")
	}
}
