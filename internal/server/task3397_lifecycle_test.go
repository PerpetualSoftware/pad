package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3397 (U8c): two-phase disable and rotate, re-enable, uninstall.

// lifecycleInstall is a U5a test install whose bot is bound (bot_user_id) and
// a member, as provisioning leaves it.
func lifecycleInstall(t *testing.T, srv *Server, id string) testInstall {
	t.Helper()
	in := newTestInstall(t, srv, id)
	if _, err := srv.store.DB().Exec(`UPDATE app_installs SET bot_user_id = ?, service_access = 'write' WHERE id = ?`, in.bot.ID, id); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`INSERT INTO workspace_members (workspace_id, user_id, role, created_at) VALUES (?, ?, 'editor', ?)`,
		in.wsID, in.bot.ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	return in
}

func tokenLive(srv *Server, tok string) bool {
	_, err := srv.introspectAppToken(context.Background(), tok)
	return err == nil
}

func mintStatus(srv *Server, in testInstall, secret string) int {
	return postTokenBasic(srv, serviceTokenForm(testAppAPIAudience), in.clientID, secret).Code
}

func disable(t *testing.T, srv *Server, in testInstall) {
	t.Helper()
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownDisable); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.FinishDisable(in.wsID, in.id); err != nil {
		t.Fatal(err)
	}
}

func installEpoch(t *testing.T, srv *Server, id string) int64 {
	t.Helper()
	var e int64
	if err := srv.store.DB().QueryRow(`SELECT auth_epoch FROM app_installs WHERE id = ?`, id).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// Disable kills every token, refuses new ones, and re-enable never revives a
// pre-disable token (the epoch never moves back), while fresh ones work.
func TestTask3397_DisableAndReenable(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := lifecycleInstall(t, srv, "inst-disable")
	tok := mintServiceToken(t, srv, in)
	if !tokenLive(srv, tok) {
		t.Fatal("fixture: the token is not live")
	}
	before := installEpoch(t, srv, in.id)
	disable(t, srv, in)
	if st, _ := srv.store.InstallState(in.wsID, in.id); st != store.InstallInactive {
		t.Fatalf("state %s, want inactive", st)
	}
	if installEpoch(t, srv, in.id) != before+1 {
		t.Fatal("phase 1 did not bump the epoch")
	}
	if tokenLive(srv, tok) {
		t.Fatal("a pre-disable token is live after disable")
	}
	// Phase 1 also REVOKES the grants, not only outdates them: the epoch
	// alone would already fail introspection, so the rows are checked.
	var grants int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM oauth_access_tokens WHERE client_id = ?`, in.clientID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if grants != 0 {
		t.Fatalf("%d access tokens left after phase 1", grants)
	}
	if c := mintStatus(srv, in, in.secret); c == http.StatusOK {
		t.Fatal("a disabled install minted a token")
	}
	if err := srv.store.ReenableInstall(in.wsID, in.id); err != nil {
		t.Fatal(err)
	}
	if installEpoch(t, srv, in.id) != before+1 {
		t.Fatal("re-enable moved the epoch")
	}
	if tokenLive(srv, tok) {
		t.Fatal("a pre-disable token was revived by re-enable")
	}
	fresh := mintServiceToken(t, srv, in)
	if !tokenLive(srv, fresh) {
		t.Fatal("a fresh token after re-enable is not live")
	}
}

// Rotate: old tokens die, the old secret stops authenticating, and the new
// install code redeems for the secret the app uses from then on.
func TestTask3397_Rotate(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := lifecycleInstall(t, srv, "inst-rotate")
	tok := mintServiceToken(t, srv, in)
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownRotate); err != nil {
		t.Fatal(err)
	}
	code, _, err := srv.store.FinishRotate(in.wsID, in.id)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := srv.store.InstallState(in.wsID, in.id); st != store.InstallActive {
		t.Fatalf("state %s, want active", st)
	}
	if tokenLive(srv, tok) {
		t.Fatal("a pre-rotate token is live")
	}
	if c := mintStatus(srv, in, in.secret); c == http.StatusOK {
		t.Fatal("the pre-rotate secret still authenticates")
	}
	red, err := srv.store.RedeemInstallCode(code)
	if err != nil {
		t.Fatalf("redeem the rotate code: %v", err)
	}
	in.secret = red.ClientSecret
	if !tokenLive(srv, mintServiceToken(t, srv, in)) {
		t.Fatal("the redeemed secret does not mint a live token")
	}
}

// Uninstall keeps the bot's users row, disabled, and the install row as the
// tombstone; it removes the client, the bot's membership and every credential
// (lead ruling, day 86: the spec, not the account-deletion purge). A disabled
// bot cannot mint a credential through any door even with the kind gate
// taken away, which pins "disabled" as a second, independent lock.
func TestTask3397_UninstallKeepsADisabledBotAndTheTombstone(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := lifecycleInstall(t, srv, "inst-uninstall")
	tok := mintServiceToken(t, srv, in)
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UninstallAppTx(in.wsID, in.id); err != nil {
		t.Fatal(err)
	}
	db := srv.store.DB()
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if st, _ := srv.store.InstallState(in.wsID, in.id); st != store.InstallUninstalled {
		t.Fatalf("state %s, want uninstalled", st)
	}
	if count(`SELECT COUNT(*) FROM app_installs WHERE id = ?`, in.id) != 1 {
		t.Fatal("the install row was deleted; it must stay as the tombstone")
	}
	if count(`SELECT COUNT(*) FROM users WHERE id = ? AND disabled_at IS NOT NULL`, in.bot.ID) != 1 {
		t.Fatal("the bot's users row is gone or not disabled")
	}
	for name, q := range map[string]string{
		"client":      `SELECT COUNT(*) FROM oauth_clients WHERE app_install_id = ?`,
		"bindings":    `SELECT COUNT(*) FROM app_token_bindings WHERE install_id = ?`,
		"membership":  `SELECT COUNT(*) FROM workspace_members wm JOIN app_installs i ON i.bot_user_id = wm.user_id WHERE i.id = ?`,
		"sessions":    `SELECT COUNT(*) FROM sessions s JOIN app_installs i ON i.bot_user_id = s.user_id WHERE i.id = ?`,
		"api tokens":  `SELECT COUNT(*) FROM api_tokens a JOIN app_installs i ON i.bot_user_id = a.user_id WHERE i.id = ?`,
		"connections": `SELECT COUNT(*) FROM oauth_connections c JOIN app_installs i ON i.bot_user_id = c.user_id WHERE i.id = ?`,
	} {
		if n := count(q, in.id); n != 0 {
			t.Errorf("%s: %d rows left", name, n)
		}
	}
	if tokenLive(srv, tok) {
		t.Fatal("a pre-uninstall token is live")
	}
	if c := mintStatus(srv, in, in.secret); c == http.StatusOK {
		t.Fatal("an uninstalled install minted a token")
	}
	// Take the kind gate away: "disabled" alone must still refuse every mint.
	if _, err := db.Exec(`UPDATE users SET kind = ? WHERE id = ?`, models.UserKindHuman, in.bot.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.CreateSession(in.bot.ID, "test", "192.0.2.1", "ua", time.Hour); err == nil {
		t.Error("a session was minted for the uninstalled (disabled) bot")
	}
	if _, err := srv.store.CreateAPIToken(in.bot.ID, models.APITokenCreate{Name: "x"}, 30, 365); err == nil {
		t.Error("an API token was minted for the uninstalled (disabled) bot")
	}
}

// Attribution survives: what the bot wrote keeps its via_app and its user id.
func TestTask3397_UninstallKeepsAttribution(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := lifecycleInstall(t, srv, "inst-attrib")
	coll, err := srv.store.CreateCollection(in.wsID, models.CollectionCreate{Name: "Tickets", Slug: "tickets"})
	if err != nil {
		t.Fatal(err)
	}
	item, err := srv.store.CreateItem(in.wsID, coll.ID, models.ItemCreate{Title: "By the app"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE items SET via_app = ?, created_by_user_id = ? WHERE id = ?`, in.id, in.bot.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownUninstall); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.UninstallAppTx(in.wsID, in.id); err != nil {
		t.Fatal(err)
	}
	var via, by string
	if err := srv.store.DB().QueryRow(`SELECT via_app, created_by_user_id FROM items WHERE id = ?`, item.ID).Scan(&via, &by); err != nil {
		t.Fatal(err)
	}
	if via != in.id || by != in.bot.ID {
		t.Fatalf("attribution changed: via_app %q, created_by_user_id %q", via, by)
	}
}

// Phase 1 refuses an app write admitted at the old epoch: the FencedTx
// re-reads the epoch under the same install row (DOC-3371 §2, R3-4).
func TestTask3397_PhaseOneFencesAnAdmittedWrite(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := lifecycleInstall(t, srv, "inst-fence")
	epoch := installEpoch(t, srv, in.id)
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownDisable); err != nil {
		t.Fatal(err)
	}
	_, err := srv.store.BeginFenced(context.Background(), store.FenceSpec{InstallID: in.id, WorkspaceID: in.wsID, Epoch: epoch})
	if err == nil {
		t.Fatal("a write admitted before phase 1 opened its fence after it")
	}
}

// Wrong states answer a typed refusal, and the doors resume a call left
// between the phases.
func TestTask3397_LifecycleStatesAndResume(t *testing.T) {
	srv := appOAuthServer(t, true)
	in := lifecycleInstall(t, srv, "inst-states")
	var se *store.InstallStateError
	if err := srv.store.ReenableInstall(in.wsID, in.id); err != nil {
		t.Fatalf("re-enable of an active install is a no-op, got %v", err)
	}
	if err := srv.store.FinishDisable(in.wsID, in.id); !errors.As(err, &se) || se.State != store.InstallActive {
		t.Fatalf("phase 2 without phase 1: %v", err)
	}
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownDisable); err != nil {
		t.Fatal(err)
	}
	epoch := installEpoch(t, srv, in.id)
	// A crash here; the owner repeats the call. Phase 1 is a no-op (no second
	// bump), phase 2 finishes.
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownDisable); err != nil {
		t.Fatal(err)
	}
	if installEpoch(t, srv, in.id) != epoch {
		t.Fatal("a resumed phase 1 bumped the epoch again")
	}
	if err := srv.store.FinishDisable(in.wsID, in.id); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.BeginInstallTeardown(in.wsID, in.id, store.TeardownRotate); !errors.As(err, &se) || se.State != store.InstallInactive {
		t.Fatalf("rotate of an inactive install: %v", err)
	}
	if err := srv.store.BeginInstallTeardown("other-ws", in.id, store.TeardownDisable); !errors.Is(err, store.ErrInstallNotFound) {
		t.Fatalf("another workspace's install: %v", err)
	}
}

// The owner doors, end to end over a provisioned install: disable, a
// wrong-state refusal, enable, rotate (a code that redeems), uninstall
// (repeatable), and a reinstall that adopts the tombstone's companion.
func TestTask3397_LifecycleDoors(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install: %d %s", rr.Code, rr.Body.String())
	}
	var inst appInstallConfirmResponse
	parseJSON(t, rr, &inst)
	door := func(action string) (int, appInstallStateResponse, string) {
		rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/"+inst.InstallID+"/"+action, nil, e.token)
		var out appInstallStateResponse
		if rr.Code == http.StatusOK {
			parseJSON(t, rr, &out)
		}
		return rr.Code, out, rr.Body.String()
	}
	if c, out, body := door("disable"); c != http.StatusOK || out.State != store.InstallInactive {
		t.Fatalf("disable: %d %s", c, body)
	}
	if c, _, body := door("rotate"); c != http.StatusConflict || !strings.Contains(body, `"state":"inactive"`) {
		t.Fatalf("rotate an inactive install: %d %s, want 409 naming the state", c, body)
	}
	if c, out, body := door("enable"); c != http.StatusOK || out.State != store.InstallActive {
		t.Fatalf("enable: %d %s", c, body)
	}
	c, out, body := door("rotate")
	if c != http.StatusOK || out.State != store.InstallActive || !strings.HasPrefix(out.InstallCode, "padic_") {
		t.Fatalf("rotate: %d %s", c, body)
	}
	if rr := redeem(e.srv, `{"code":"`+out.InstallCode+`"}`); rr.Code != http.StatusOK {
		t.Fatalf("redeem the rotate code: %d %s", rr.Code, rr.Body.String())
	}
	for i := 0; i < 2; i++ { // repeatable
		if c, out, body := door("uninstall"); c != http.StatusOK || out.State != store.InstallUninstalled {
			t.Fatalf("uninstall #%d: %d %s", i+1, c, body)
		}
	}
	if c, _, body := door("enable"); c != http.StatusConflict {
		t.Fatalf("enable a tombstone: %d %s, want 409", c, body)
	}
	if rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/no-such-install/disable", nil, e.token); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown install: %d", rr.Code)
	}
	// The tombstone's companion is adopted by a reinstall (U8b's rule).
	p2 := e.stagePreview(t)
	if !p2.Collections[0].Adopt {
		t.Fatalf("reinstall did not adopt: %+v", p2.Collections)
	}
}
