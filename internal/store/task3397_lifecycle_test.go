package store

import (
	"errors"
	"fmt"
	"testing"
)

// TASK-3397 (U8c): UninstallAppTx is one transaction (DOC-3371 §8 step 3). A
// failure injected after EACH step leaves every table exactly as it was.
func TestTask3397_UninstallAppTxIsAllOrNothing(t *testing.T) {
	f := task3394Fixture(t, "inst-atomic")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET bot_user_id = ? WHERE id = ?`), f.bot.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.addAppPrincipalMemberTx(tx, f.ws.ID, f.bot.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateAccessToken(task3394Req(f.clientID, f.bot.ID, "req-atomic")); err != nil {
		t.Fatalf("fixture token: %v", err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	tables := []string{"app_installs", "oauth_clients", "oauth_access_tokens", "app_token_bindings", "oauth_connections",
		"workspace_members", "member_collection_access", "users", "sessions", "api_tokens"}
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, tbl := range tables {
			var n int
			if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
				t.Fatal(err)
			}
			out[tbl] = fmt.Sprint(n)
		}
		var state, disabled string
		if err := f.s.db.QueryRow(f.s.q(`SELECT state FROM app_installs WHERE id = ?`), f.installID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if err := f.s.db.QueryRow(f.s.q(`SELECT COALESCE(disabled_at, '') FROM users WHERE id = ?`), f.bot.ID).Scan(&disabled); err != nil {
			t.Fatal(err)
		}
		out["state"], out["bot_disabled"] = state, disabled
		return out
	}
	before := snapshot()
	boom := errors.New("injected")
	for _, at := range []string{"client", "membership", "bot", "state"} {
		uninstallHookAfterStep = func(step string) error {
			if step == at {
				return boom
			}
			return nil
		}
		if err := f.s.UninstallAppTx(f.ws.ID, f.installID); !errors.Is(err, boom) {
			uninstallHookAfterStep = nil
			t.Fatalf("failure after %q: got %v", at, err)
		}
		uninstallHookAfterStep = nil
		after := snapshot()
		for k, v := range before {
			if after[k] != v {
				t.Errorf("failure after %q changed %s: %q -> %q", at, k, v, after[k])
			}
		}
	}
	if err := f.s.UninstallAppTx(f.ws.ID, f.installID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got := snapshot()["state"]; got != InstallUninstalled {
		t.Fatalf("state %s, want uninstalled", got)
	}
}
