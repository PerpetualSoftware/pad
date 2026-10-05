package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
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
	// The install's hook (U10a), so the webhook step deletes a real row.
	htx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := htx.Exec(f.s.q(`INSERT INTO webhooks (id, workspace_id, url, secret, events, active, created_at, updated_at, failure_count, app_install_id)
		VALUES (?, ?, 'https://portal.example/h', 's', '[]', ?, ?, ?, 0, ?)`), newID(), f.ws.ID, f.s.dialect.BoolToInt(true), now(), now(), f.installID); err != nil {
		t.Fatal(err)
	}
	if _, err := htx.Exec(f.s.q(`INSERT INTO app_item_actions (id, install_id, action_key, label, path, collection_ids, revision, active, created_at, updated_at)
		VALUES (?, ?, 'open', 'Open', '/t', '[]', 1, ?, ?, ?)`), newID(), f.installID, f.s.dialect.BoolToInt(true), now(), now()); err != nil {
		t.Fatal(err)
	}
	if err := htx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	tables := []string{"app_installs", "oauth_clients", "oauth_access_tokens", "app_token_bindings", "oauth_connections",
		"workspace_members", "member_collection_access", "users", "sessions", "api_tokens", "webhooks", "app_item_actions"}
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
	for _, at := range []string{"client", "webhook", "actions", "membership", "bot", "state"} {
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

func task3397LifecycleFix(t *testing.T, installID string) task3394Fix {
	t.Helper()
	f := task3394Fixture(t, installID)
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
	return f
}

func task3397Epoch(t *testing.T, s *Store, id string) int64 {
	t.Helper()
	e, err := s.InstallEpoch(id)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// codex r1 #1: rotate's phase 2 bumps the epoch again, so a grant that read
// the phase-1 epoch while the install was disabling (and authenticated with
// the old secret) is refused once the install is active again.
func TestTask3397_RotateEndsGrantsFromBetweenThePhases(t *testing.T) {
	f := task3397LifecycleFix(t, "inst-rot-window")
	before := task3397Epoch(t, f.s, f.installID)
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownRotate); err != nil {
		t.Fatal(err)
	}
	between := task3397Epoch(t, f.s, f.installID)
	if _, _, err := f.s.FinishRotate(f.ws.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	if got := task3397Epoch(t, f.s, f.installID); got != before+2 || between != before+1 {
		t.Fatalf("epochs %d -> %d -> %d, want +1 then +2", before, between, got)
	}
	req := task3394Req(f.clientID, f.bot.ID, "req-between")
	req.SessionData = fmt.Sprintf(`{"extra":{"%s":%d}}`, InstallEpochSessionKey, between)
	if err := f.s.CreateAccessToken(req); err == nil {
		t.Fatal("a grant carrying the phase-1 epoch was issued after rotate")
	}
}

// codex r1 #2: phase 1 kills every unconsumed install code, so an earlier
// code cannot be redeemed for a fresh secret after a rotate or after a
// disable and re-enable. The rotate's own code works.
func TestTask3397_TeardownInvalidatesEarlierInstallCodes(t *testing.T) {
	f := task3397LifecycleFix(t, "inst-codes")
	old, _, err := f.s.IssueInstallCode(f.ws.ID, f.installID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownRotate); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := f.s.FinishRotate(f.ws.ID, f.installID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RedeemInstallCode(old); !errors.Is(err, ErrInstallCodeInvalid) {
		t.Fatalf("a pre-rotate code redeemed after rotate: %v", err)
	}
	if _, err := f.s.RedeemInstallCode(fresh); err != nil {
		t.Fatalf("the rotate's code: %v", err)
	}

	again, _, err := f.s.IssueInstallCode(f.ws.ID, f.installID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable); err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishDisable(f.ws.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReenableInstall(f.ws.ID, f.installID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RedeemInstallCode(again); !errors.Is(err, ErrInstallCodeInvalid) {
		t.Fatalf("a pre-disable code redeemed after re-enable: %v", err)
	}
}

// codex r1 #3: uninstall racing account deletion's bot purge, which deletes
// the bot's membership, then the bot (whose FK action updates the install's
// bot_user_id), must not deadlock. The writer here replays that order.
func TestTask3397_UninstallDoesNotDeadlockWithTheBotPurge(t *testing.T) {
	f := task3397LifecycleFix(t, "inst-purge-race")
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	w, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Exec(f.s.q(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`), f.ws.ID, f.bot.ID); err != nil {
		_ = w.Rollback()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.s.UninstallAppTx(f.ws.ID, f.installID) }()
	time.Sleep(700 * time.Millisecond)
	if _, err := w.Exec(f.s.q(`UPDATE app_installs SET bot_user_id = NULL WHERE bot_user_id = ?`), f.bot.ID); err != nil {
		_ = w.Rollback()
		<-done
		t.Fatalf("the purge could not reach the install row (a deadlock victim?): %v", err)
	}
	if err := w.Commit(); err != nil {
		t.Fatalf("purge commit: %v", err)
	}
	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "40P01") {
			t.Fatalf("uninstall was a deadlock victim: %v", err)
		}
		if err != nil {
			t.Fatalf("uninstall: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("uninstall never finished")
	}
	if st, _ := f.s.InstallState(f.ws.ID, f.installID); st != InstallUninstalled {
		t.Fatalf("state %s, want uninstalled", st)
	}
}

// Phase 1 refuses an app write admitted at the old epoch: BeginFenced re-reads
// the epoch and state under the same install row (DOC-3371 §2, R3-4). Lives in
// internal/store because only appstore may open a fence outside this package
// (TestFencedTx_OnlyAppstoreOpensOne).
func TestTask3397_PhaseOneFencesAnAdmittedWrite(t *testing.T) {
	f := task3397LifecycleFix(t, "inst-fence")
	epoch := task3397Epoch(t, f.s, f.installID)
	spec := FenceSpec{InstallID: f.installID, WorkspaceID: f.ws.ID, Epoch: epoch}
	ftx, err := f.s.BeginFenced(context.Background(), spec)
	if err != nil {
		t.Fatalf("control: a fence at the current epoch: %v", err)
	}
	_ = ftx.Rollback()
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.BeginFenced(context.Background(), spec); err == nil {
		t.Fatal("a write admitted before phase 1 opened its fence after it")
	}
}

// The lead's check on #1775: UninstallAppTx takes the bot's users row BEFORE
// the install row, and the issuance barrier takes the install row FOR UPDATE.
// No cycle, for two reasons: by the time UninstallAppTx runs the install is
// 'uninstalling', so the barrier refuses on its state check
// (install_clients.go, `state != "active"`) before it reads the user; and its
// user read is a plain SELECT, which never waits on a row lock. This races a
// real issuance against UninstallAppTx held just after its bot-row locks, so
// a barrier reordered to lock-read the user first goes red (40P01).
func TestTask3397_IssuanceDoesNotDeadlockWithUninstall(t *testing.T) {
	f := task3397LifecycleFix(t, "inst-issue-race")
	epoch := task3397Epoch(t, f.s, f.installID)
	if err := f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	held, release := make(chan struct{}), make(chan struct{})
	uninstallHookAfterStep = func(step string) error {
		if step == "bot-locks" {
			close(held)
			<-release
		}
		return nil
	}
	t.Cleanup(func() { uninstallHookAfterStep = nil })
	done := make(chan error, 1)
	go func() { done <- f.s.UninstallAppTx(f.ws.ID, f.installID) }()
	<-held
	issued := make(chan error, 1)
	go func() {
		req := task3394Req(f.clientID, f.bot.ID, "req-issue-race")
		req.SessionData = fmt.Sprintf(`{"extra":{"%s":%d}}`, InstallEpochSessionKey, epoch)
		issued <- f.s.CreateAccessToken(req)
	}()
	time.Sleep(700 * time.Millisecond) // the issuance reaches the install row
	close(release)
	for name, ch := range map[string]chan error{"uninstall": done, "issuance": issued} {
		select {
		case err := <-ch:
			if err != nil && strings.Contains(err.Error(), "40P01") {
				t.Fatalf("%s was a deadlock victim: %v", name, err)
			}
			if name == "uninstall" && err != nil {
				t.Fatalf("uninstall: %v", err)
			}
			if name == "issuance" && err == nil {
				t.Fatal("a token was issued for an install being uninstalled")
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s never finished", name)
		}
	}
}
