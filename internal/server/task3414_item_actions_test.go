package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3414 (SPEC-6 U11): item actions and context codes, end to end:
// list and mint as the viewer, redeem as the app.

type u11Fix struct {
	appAPIFix
	viewer    *models.User
	viewerTok string
	actionID  string
}

func u11Fixture(t *testing.T) u11Fix {
	t.Helper()
	return u11Prepare(t, appAPIFixture(t, "read"))
}

// u11Prepare adds to an app API fixture: the install's manifest (title and
// base_url), one item action on the companion collection, and a viewer who
// is a member with full access and a web session.
func u11Prepare(t *testing.T, base appAPIFix) u11Fix {
	t.Helper()
	f := u11Fix{appAPIFix: base}
	db := f.srv.store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE app_installs SET manifest = ? WHERE id = ?`, `{"title":"Portal","base_url":"https://portal.example"}`, f.in.id)
	f.actionID = "act-open"
	exec(`INSERT INTO app_item_actions (id, install_id, action_key, label, path, collection_ids, revision, active, created_at, updated_at)
		VALUES (?, ?, 'open', 'Open in Portal', '/t?view=1', ?, 1, 1, ?, ?)`,
		f.actionID, f.in.id, `["`+f.companion.ID+`"]`, time.Now().UTC().Format(time.RFC3339), time.Now().UTC().Format(time.RFC3339))
	f.viewer, f.viewerTok = loginTestUserAs(t, f.srv, "viewer-3414@example.com", "Vera Viewer", "pw-3414-viewer")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'viewer', 'all', ?)`,
		f.ws.ID, f.viewer.ID, time.Now().UTC().Format(time.RFC3339))
	return f
}

func (f u11Fix) itemPath(itemID, rest string) string {
	return "/api/v1/workspaces/" + f.ws.Slug + "/items/" + itemID + rest
}

// mint mints as the viewer; returns the status, the URL and its code.
func (f u11Fix) mint(t *testing.T, itemID, installID, key string) (int, string, string, string) {
	t.Helper()
	rr := doRequestWithCookie(f.srv, "POST", f.itemPath(itemID, "/app-actions/"+installID+"/"+key), nil, f.viewerTok)
	if rr.Code != http.StatusOK {
		return rr.Code, "", "", rr.Body.String()
	}
	var out map[string]string
	parseJSON(t, rr, &out)
	u, err := url.Parse(out["url"])
	if err != nil {
		t.Fatal(err)
	}
	return rr.Code, out["url"], u.Query().Get("pad_context"), rr.Body.String()
}

// redeem is §6's POST /api/app/v1/context/redeem: no workspace in the path,
// the token's own (codex r1 on U11).
func (f u11Fix) redeem(code string) *httptest.ResponseRecorder {
	return appDo(f.srv, "POST", appAPIPrefix+"/context/redeem", f.token, map[string]string{"code": code})
}

func TestTask3414_MintRedeemAndReplay(t *testing.T) {
	f := u11Fixture(t)
	// The list for the item pane.
	rr := doRequestWithCookie(f.srv, "GET", f.itemPath(f.item.ID, "/app-actions"), nil, f.viewerTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var actions []store.ItemAppAction
	parseJSON(t, rr, &actions)
	if len(actions) != 1 || actions[0].InstallID != f.in.id || actions[0].ActionKey != "open" || actions[0].Label != "Open in Portal" || actions[0].AppTitle != "Portal" {
		t.Fatalf("list: %+v", actions)
	}
	// Not offered on a collection the action does not name.
	rr = doRequestWithCookie(f.srv, "GET", f.itemPath(f.privateItem.ID, "/app-actions"), nil, f.viewerTok)
	if !strings.Contains(rr.Body.String(), "[]") {
		t.Fatalf("list on another collection: %d %s", rr.Code, rr.Body.String())
	}

	code, target, ctx, body := f.mint(t, f.item.ID, f.in.id, "open")
	if code != http.StatusOK || !strings.HasPrefix(target, "https://portal.example/t?") || !strings.HasPrefix(ctx, "padctx_") || len(ctx) != len("padctx_")+32 {
		t.Fatalf("mint: %d %s %s", code, target, body)
	}
	if u, _ := url.Parse(target); u.Query().Get("view") != "1" {
		t.Fatalf("the action's own query was lost: %s", target)
	}

	rr = f.redeem(ctx)
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", rr.Code, rr.Body.String())
	}
	var red map[string]any
	parseJSON(t, rr, &red)
	item, _ := red["item"].(map[string]any)
	if red["action_key"] != "open" || item["id"] != f.item.ID || item["collection"] != f.companion.Slug {
		t.Fatalf("redeem body: %s", rr.Body.String())
	}
	for _, k := range []string{"viewer", "viewer_user_id", "user_id", "email"} {
		if _, ok := red[k]; ok {
			t.Errorf("the redeem names the viewer (%q): %s", k, rr.Body.String())
		}
	}
	// Single use: a replay is the same refusal as an unknown code.
	replay := f.redeem(ctx)
	unknown := f.redeem("padctx_00000000000000000000000000000000")
	if replay.Code != http.StatusNotFound || replay.Body.String() != unknown.Body.String() {
		t.Fatalf("replay %d %q, unknown %d %q", replay.Code, replay.Body.String(), unknown.Code, unknown.Body.String())
	}
}

// Every refusal of a redeem is one body, whatever the reason; and a
// mismatch burns the code (lead ruling, day 88: consume first).
func TestTask3414_RedeemRefusalsAreIndistinguishable(t *testing.T) {
	f := u11Fixture(t)
	db := f.srv.store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	want := f.redeem("padctx_00000000000000000000000000000000")
	if want.Code != http.StatusNotFound {
		t.Fatalf("unknown code: %d %s", want.Code, want.Body.String())
	}
	for _, tc := range []struct {
		name    string
		breakIt func(code string)
		undo    func()
	}{
		{"expired", func(code string) {
			exec(`UPDATE app_context_codes SET expires_at = '2000-01-01T00:00:00.000Z'`)
		}, func() {}},
		{"viewer removed from the workspace", func(string) {
			exec(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.viewer.ID)
		}, func() {
			exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'viewer', 'all', ?)`,
				f.ws.ID, f.viewer.ID, time.Now().UTC().Format(time.RFC3339))
		}},
		{"viewer's collection access narrowed", func(string) {
			exec(`UPDATE workspace_members SET collection_access = 'specific' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.viewer.ID)
		}, func() {
			exec(`UPDATE workspace_members SET collection_access = 'all' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.viewer.ID)
		}},
		{"viewer disabled", func(string) {
			exec(`UPDATE users SET disabled_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), f.viewer.ID)
		}, func() {
			exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, f.viewer.ID)
		}},
		{"item moved out of the companions", func(string) {
			exec(`UPDATE items SET collection_id = ? WHERE id = ?`, f.private.ID, f.item.ID)
		}, func() {
			exec(`UPDATE items SET collection_id = ? WHERE id = ?`, f.companion.ID, f.item.ID)
		}},
		{"item deleted", func(string) {
			exec(`UPDATE items SET deleted_at = ? WHERE id = ?`, time.Now().UTC().Format(time.RFC3339), f.item.ID)
		}, func() {
			exec(`UPDATE items SET deleted_at = NULL WHERE id = ?`, f.item.ID)
		}},
		{"action changed by an upgrade", func(string) {
			exec(`UPDATE app_item_actions SET revision = revision + 1 WHERE id = ?`, f.actionID)
		}, func() {}},
		{"action removed by an upgrade", func(string) {
			exec(`UPDATE app_item_actions SET active = 0, revision = revision + 1 WHERE id = ?`, f.actionID)
		}, func() {
			exec(`UPDATE app_item_actions SET active = 1 WHERE id = ?`, f.actionID)
		}},
		{"action deactivated at the same revision", func(string) {
			exec(`UPDATE app_item_actions SET active = 0 WHERE id = ?`, f.actionID)
		}, func() {
			exec(`UPDATE app_item_actions SET active = 1 WHERE id = ?`, f.actionID)
		}},
		{"item moved to a companion the action is not offered on", func(string) {
			exec(`UPDATE items SET collection_id = ? WHERE id = ?`, f.otherCompanion(t), f.item.ID)
		}, func() {
			exec(`UPDATE items SET collection_id = ? WHERE id = ?`, f.companion.ID, f.item.ID)
		}},
		{"companion released", func(string) {
			exec(`UPDATE collections SET via_app = NULL WHERE id = ?`, f.companion.ID)
		}, func() {
			exec(`UPDATE collections SET via_app = ? WHERE id = ?`, f.in.id, f.companion.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, code, body := f.mint(t, f.item.ID, f.in.id, "open")
			if status != http.StatusOK {
				t.Fatalf("mint: %d %s", status, body)
			}
			tc.breakIt(code)
			rr := f.redeem(code)
			tc.undo()
			if rr.Code != want.Code || rr.Body.String() != want.Body.String() {
				t.Fatalf("refusal %d %q differs from the unknown-code refusal %d %q", rr.Code, rr.Body.String(), want.Code, want.Body.String())
			}
			// Burned: with everything restored, the same code still fails.
			if again := f.redeem(code); again.Code != http.StatusNotFound {
				t.Fatalf("a refused code was redeemable afterwards: %d %s", again.Code, again.Body.String())
			}
		})
	}
}

// The mint re-checks what the redeem will (lead ruling, day 88), with one
// refusal body.
func TestTask3414_MintRefusalsAreIndistinguishable(t *testing.T) {
	f := u11Fixture(t)
	status, _, _, want := f.mint(t, f.item.ID, "no-such-install", "open")
	if status != http.StatusNotFound {
		t.Fatalf("unknown install: %d %s", status, want)
	}
	other, err := f.srv.store.CreateItem(f.ws.ID, f.private.ID, models.ItemCreate{Title: "Elsewhere"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		item, install, key string
		prep, undo         func()
	}{
		{"unknown action", f.item.ID, f.in.id, "nope", func() {}, func() {}},
		{"item outside the action's collections", other.ID, f.in.id, "open", func() {}, func() {}},
		{"unknown item", "no-such-item", f.in.id, "open", func() {}, func() {}},
		{"viewer cannot see the item", f.item.ID, f.in.id, "open", func() {
			_, _ = f.srv.store.DB().Exec(`UPDATE workspace_members SET collection_access = 'specific' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.viewer.ID)
		}, func() {
			_, _ = f.srv.store.DB().Exec(`UPDATE workspace_members SET collection_access = 'all' WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.viewer.ID)
		}},
		{"companion released", f.item.ID, f.in.id, "open", func() {
			_, _ = f.srv.store.DB().Exec(`UPDATE collections SET via_app = NULL WHERE id = ?`, f.companion.ID)
		}, func() {
			_, _ = f.srv.store.DB().Exec(`UPDATE collections SET via_app = ? WHERE id = ?`, f.in.id, f.companion.ID)
		}},
		{"install disabled", f.item.ID, f.in.id, "open", func() {
			_, _ = f.srv.store.DB().Exec(`UPDATE app_installs SET state = 'inactive' WHERE id = ?`, f.in.id)
		}, func() {
			_, _ = f.srv.store.DB().Exec(`UPDATE app_installs SET state = 'active' WHERE id = ?`, f.in.id)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.prep()
			status, _, _, body := f.mint(t, tc.item, tc.install, tc.key)
			tc.undo()
			if status != http.StatusNotFound || body != want {
				t.Fatalf("refusal %d %q differs from %q", status, body, want)
			}
		})
	}
	if n := countRows(t, f.srv, `SELECT COUNT(*) FROM app_context_codes`); n != 0 {
		t.Fatalf("%d codes minted by refused mints", n)
	}
}

// The lead's pin (day 88): access revoked between mint and redeem refuses
// the redeem, read from the viewer's CURRENT memberships, not the mint's.
func TestTask3414_RevokedBetweenMintAndRedeem(t *testing.T) {
	f := u11Fixture(t)
	status, _, code, body := f.mint(t, f.item.ID, f.in.id, "open")
	if status != http.StatusOK {
		t.Fatalf("mint: %d %s", status, body)
	}
	// The viewer is removed from the workspace after the click.
	if _, err := f.srv.store.DB().Exec(`DELETE FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, f.ws.ID, f.viewer.ID); err != nil {
		t.Fatal(err)
	}
	if rr := f.redeem(code); rr.Code != http.StatusNotFound {
		t.Fatalf("redeem after the viewer lost access: %d %s", rr.Code, rr.Body.String())
	}
}

// Provisioning writes the manifest's actions; a reviewed upgrade that
// changes one gives it a new revision, one that removes it retires it;
// uninstall deletes them; installs from before U11 are backfilled.
func TestTask3414_ActionRowsFollowTheManifest(t *testing.T) {
	u := newUpgradeEnv(t)
	db := u.srv.store.DB()
	row := func() (label, path, colls string, rev int, active bool) {
		t.Helper()
		if err := db.QueryRow(`SELECT label, path, collection_ids, revision, active FROM app_item_actions WHERE install_id = ? AND action_key = 'open'`, u.installID).
			Scan(&label, &path, &colls, &rev, &active); err != nil {
			t.Fatal(err)
		}
		return
	}
	companion, err := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if err != nil || companion == nil {
		t.Fatal(err)
	}
	if label, path, colls, rev, active := row(); label != "Open" || path != "/t" || colls != `["`+companion.ID+`"]` || rev != 1 || !active {
		t.Fatalf("provisioned action: %s %s %s %d %v", label, path, colls, rev, active)
	}
	upgrade := func(version string, actions []any) {
		t.Helper()
		u.m["version"] = version
		u.m["item_actions"] = actions
		u.publish(t, u.m)
		_, p, _ := u.previewUpgrade(t)
		if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
			t.Fatalf("upgrade %s: %d %s", version, code, body)
		}
	}
	upgrade("1.0.1", []any{map[string]any{"key": "open", "label": "Open ticket", "collections": []any{"tickets"}, "path": "/t"}})
	if label, _, _, rev, active := row(); label != "Open ticket" || rev != 2 || !active {
		t.Fatalf("changed action: %s rev %d active %v", label, rev, active)
	}
	upgrade("1.0.2", []any{map[string]any{"key": "open", "label": "Open ticket", "collections": []any{"tickets"}, "path": "/t"}})
	if _, _, _, rev, _ := row(); rev != 2 {
		t.Fatalf("an unchanged action got revision %d", rev)
	}
	upgrade("1.0.3", []any{})
	if _, _, _, rev, active := row(); active || rev != 3 {
		t.Fatalf("removed action: rev %d active %v, want retired", rev, active)
	}

	// Backfill: an install from before U11 gets its rows at start.
	if _, err := db.Exec(`DELETE FROM app_item_actions WHERE install_id = ?`, u.installID); err != nil {
		t.Fatal(err)
	}
	upgrade("1.0.4", []any{map[string]any{"key": "open", "label": "Open", "collections": []any{"tickets"}, "path": "/t"}})
	if _, err := db.Exec(`DELETE FROM app_item_actions WHERE install_id = ?`, u.installID); err != nil {
		t.Fatal(err)
	}
	u.srv.EnsureAppItemActions(context.Background())
	if label, _, _, rev, active := row(); label != "Open" || rev != 1 || !active {
		t.Fatalf("backfilled action: %s rev %d active %v", label, rev, active)
	}

	// Uninstall deletes them.
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/uninstall", nil, u.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if n := countRows(t, u.srv, `SELECT COUNT(*) FROM app_item_actions WHERE install_id = ?`, u.installID); n != 0 {
		t.Fatalf("%d action rows after uninstall", n)
	}
}

// otherCompanion is a second companion collection of the install, which the
// action is not offered on (created once).
func (f u11Fix) otherCompanion(t *testing.T) string {
	t.Helper()
	if c, _ := f.srv.store.GetCollectionBySlug(f.ws.ID, "requests-two"); c != nil {
		return c.ID
	}
	c, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Requests two", Slug: "requests-two", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE collections SET via_app = ? WHERE id = ?`, f.in.id, c.ID); err != nil {
		t.Fatal(err)
	}
	return c.ID
}

// A code minted before the install's epoch moved (a rotate) is refused even
// with a token from the new epoch: the code is bound to the epoch it was
// minted under (§6).
func TestTask3414_CodeFromAnEarlierEpochIsRefused(t *testing.T) {
	f := u11Fixture(t)
	status, _, code, body := f.mint(t, f.item.ID, f.in.id, "open")
	if status != http.StatusOK {
		t.Fatalf("mint: %d %s", status, body)
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`, f.in.id); err != nil {
		t.Fatal(err)
	}
	f.token = mintServiceToken(t, f.srv, f.in)
	if rr := f.redeem(code); rr.Code != http.StatusNotFound {
		t.Fatalf("redeem of a code from an earlier epoch: %d %s", rr.Code, rr.Body.String())
	}
	// A code minted now, at the new epoch, redeems.
	status, _, code, body = f.mint(t, f.item.ID, f.in.id, "open")
	if status != http.StatusOK {
		t.Fatalf("mint at the new epoch: %d %s", status, body)
	}
	if rr := f.redeem(code); rr.Code != http.StatusOK {
		t.Fatalf("redeem at the new epoch: %d %s", rr.Code, rr.Body.String())
	}
}

// A platform admin who is not a member of the workspace is not a viewer an
// app may be handed an item for: the membership-only stance, not the admin
// cookie bypass.
func TestTask3414_NonMemberAdminCannotLaunch(t *testing.T) {
	f := u11Fixture(t)
	admin, tok := loginTestUserAs(t, f.srv, "admin-3414@example.com", "Ada Admin", "pw-3414-admin")
	if _, err := f.srv.store.DB().Exec(`UPDATE users SET role = 'admin' WHERE id = ?`, admin.ID); err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(f.srv, "POST", f.itemPath(f.item.ID, "/app-actions/"+f.in.id+"/open"), nil, tok)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("a non-member admin minted a code: %d %s", rr.Code, rr.Body.String())
	}
}

// The unscoped route census: redeem's answer is {action_key, item} with the
// app item DTO's keys, and it is the one root-mounted route.
func TestTask3414_RedeemRouteCensus(t *testing.T) {
	if len(appUnscopedRoutes) != 1 || appUnscopedRoutes[0].Name != "appRedeemContext" {
		t.Fatalf("unscoped routes: %+v", appUnscopedRoutes)
	}
	f := u11Fixture(t)
	_, _, code, body := f.mint(t, f.item.ID, f.in.id, "open")
	if code == "" {
		t.Fatalf("mint: %s", body)
	}
	rr := f.redeem(code)
	if rr.Code != http.StatusOK || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("redeem: %d %s", rr.Code, rr.Body.String())
	}
	var m map[string]any
	parseJSON(t, rr, &m)
	if strings.Join(appKeysOf(m), ",") != "action_key,item" {
		t.Fatalf("top-level keys %v", appKeysOf(m))
	}
	checkItem(t, "appRedeemContext.item", m["item"].(map[string]any))
	// The workspace route of the same name does not exist: nothing can be
	// refused at a workspace check before the code is consumed.
	if rr := appDo(f.srv, "POST", f.path("/context/redeem"), f.token, map[string]string{"code": "x"}); rr.Code != http.StatusNotFound && rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("a workspace-scoped redeem path answered %d", rr.Code)
	}
}

// §6: a service token only; a person's delegated token is refused with the
// same body.
func TestTask3414_DelegatedTokenCannotRedeem(t *testing.T) {
	d := delegatedAPIFixture(t, "read", "read", "editor")
	f := u11Prepare(t, d.appAPIFix)
	_, _, code, body := f.mint(t, f.item.ID, f.in.id, "open")
	if code == "" {
		t.Fatalf("mint: %s", body)
	}
	want := f.redeem("padctx_00000000000000000000000000000000")
	got := f.redeem(code)
	if got.Code != http.StatusNotFound || got.Body.String() != want.Body.String() {
		t.Fatalf("a delegated redeem: %d %s", got.Code, got.Body.String())
	}
}

// codex r1 on U11: the URL is built from the validated origin, not the raw
// stored base_url.
func TestTask3414_URLUsesTheNormalizedOrigin(t *testing.T) {
	f := u11Fixture(t)
	if _, err := f.srv.store.DB().Exec(`UPDATE app_installs SET manifest = ? WHERE id = ?`, `{"title":"Portal","base_url":"  https://Portal.Example:443  "}`, f.in.id); err != nil {
		t.Fatal(err)
	}
	status, target, _, body := f.mint(t, f.item.ID, f.in.id, "open")
	if status != http.StatusOK || !strings.HasPrefix(target, "https://portal.example/t?") {
		t.Fatalf("mint: %d %s %s", status, target, body)
	}
}

// codex r1 on U11: the mint and redeem transactions read nothing through the
// pool. With ONE connection, a pool read inside them deadlocks
// deterministically. Every viewer branch is armed: a full member, a
// restricted member (collection access list), a non-member guest (an item
// grant).
func TestTask3414_NoPoolReadsInsideTheTransactions(t *testing.T) {
	f := u11Fixture(t)
	db := f.srv.store.DB()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	restricted, restrictedTok := loginTestUserAs(t, f.srv, "restricted-3414@example.com", "Rae", "pw-3414-r")
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'viewer', 'specific', ?)`, f.ws.ID, restricted.ID, ts)
	exec(`INSERT INTO member_collection_access (workspace_id, user_id, collection_id, created_at) VALUES (?, ?, ?, ?)`, f.ws.ID, restricted.ID, f.companion.ID, ts)
	guest, guestTok := loginTestUserAs(t, f.srv, "guest-3414@example.com", "Gus", "pw-3414-g")
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.item.ID, guest.ID, "view", f.viewer.ID); err != nil {
		t.Fatal(err)
	}
	was := db.Stats().MaxOpenConnections
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.SetMaxOpenConns(was) })

	for _, v := range []struct {
		name, tok string
	}{{"full member", f.viewerTok}, {"restricted member", restrictedTok}, {"guest", guestTok}} {
		t.Run(v.name, func(t *testing.T) {
			done := make(chan string, 1)
			go func() {
				rr := doRequestWithCookie(f.srv, "POST", f.itemPath(f.item.ID, "/app-actions/"+f.in.id+"/open"), nil, v.tok)
				if rr.Code != http.StatusOK {
					done <- "mint " + rr.Body.String()
					return
				}
				var out map[string]string
				_ = json.Unmarshal(rr.Body.Bytes(), &out)
				u, _ := url.Parse(out["url"])
				red := f.redeem(u.Query().Get("pad_context"))
				if red.Code != http.StatusOK {
					done <- "redeem " + red.Body.String()
					return
				}
				done <- ""
			}()
			select {
			case msg := <-done:
				if msg != "" {
					t.Fatal(msg)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("mint or redeem hung on a one-connection pool: a pool read inside the transaction")
			}
		})
	}
}
