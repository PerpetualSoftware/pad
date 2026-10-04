package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/appmanifest"
)

// TASK-3408 (U10a): the app's hook is created from the manifest, HELD until
// a redeem hands the app its secret, hidden from every owner webhook door,
// and follows upgrades and uninstall.

type appHookRow struct {
	id, url, events, secret string
	delivered               sql.NullString
}

func (u *upgradeEnv) hook(t *testing.T) *appHookRow {
	t.Helper()
	var h appHookRow
	err := u.srv.store.DB().QueryRow(`SELECT id, url, events, secret, secret_delivered_at FROM webhooks WHERE app_install_id = ?`, u.installID).
		Scan(&h.id, &h.url, &h.events, &h.secret, &h.delivered)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return &h
}

func (u *upgradeEnv) getInstall(t *testing.T) appInstallStateResponse {
	t.Helper()
	rr := doRequestWithCookie(u.srv, "GET", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID, nil, u.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("get install: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallStateResponse
	parseJSON(t, rr, &out)
	return out
}

// issueAndRedeem issues a fresh install code and redeems it.
func (u *upgradeEnv) issueAndRedeem(t *testing.T) map[string]string {
	t.Helper()
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/install-code", nil, u.token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue code: %d %s", rr.Code, rr.Body.String())
	}
	var issued map[string]any
	parseJSON(t, rr, &issued)
	rr = redeem(u.srv, `{"code":"`+issued["install_code"].(string)+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", rr.Code, rr.Body.String())
	}
	var out map[string]string
	parseJSON(t, rr, &out)
	return out
}

func TestAppWebhook_HeldUntilRedeem(t *testing.T) {
	u := newUpgradeEnv(t)
	h := u.hook(t)
	if h == nil {
		t.Fatal("provisioning created no hook for a manifest that declares events")
	}
	if h.url != u.origin()+"/hooks" || h.delivered.Valid {
		t.Fatalf("hook: url %q delivered %v", h.url, h.delivered)
	}
	companion, err := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if err != nil || companion == nil {
		t.Fatal(err)
	}
	var subs []struct {
		Name          string   `json:"name"`
		CollectionIDs []string `json:"collection_ids"`
	}
	if err := json.Unmarshal([]byte(h.events), &subs); err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 || subs[0].Name != "item.created" || len(subs[0].CollectionIDs) != 1 || subs[0].CollectionIDs[0] != companion.ID {
		t.Fatalf("subscriptions %s, want item.created on %s", h.events, companion.ID)
	}

	got := u.getInstall(t)
	if got.Webhook == nil || got.Webhook.Status != "awaiting_secret" || got.Webhook.Notice != appWebhookHeldNotice {
		t.Fatalf("held hook view: %+v", got.Webhook)
	}

	first := u.issueAndRedeem(t)
	if !strings.HasPrefix(first["webhook_secret"], "padwh_") {
		t.Fatalf("redeem handed out no webhook secret: %v", first)
	}
	h1 := u.hook(t)
	if !h1.delivered.Valid || h1.secret == h.secret {
		t.Fatalf("redeem did not release the hold or rotate the secret: %+v", h1)
	}
	got = u.getInstall(t)
	if got.Webhook == nil || got.Webhook.Status != "active" || got.Webhook.Notice != "" {
		t.Fatalf("released hook view: %+v", got.Webhook)
	}

	// A reissued code's redeem hands out a NEW secret.
	second := u.issueAndRedeem(t)
	if second["webhook_secret"] == "" || second["webhook_secret"] == first["webhook_secret"] || u.hook(t).secret == h1.secret {
		t.Fatal("a second redeem did not rotate the webhook secret")
	}
}

func TestAppWebhook_NoEventsNoHook(t *testing.T) {
	e := newProvisionEnv(t)
	m := e.manifest(t)
	delete(m, "events")
	delete(m, "webhook_url")
	e.publish(t, m)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	if n := appRows(t, e, `SELECT COUNT(*) FROM webhooks`); n != 0 {
		t.Fatalf("%d hooks for a manifest with no events", n)
	}
	rr = redeem(e.srv, `{"code":"`+out.InstallCode+`"}`)
	var red map[string]string
	parseJSON(t, rr, &red)
	if _, ok := red["webhook_secret"]; ok {
		t.Fatalf("redeem carried a webhook_secret with no hook: %v", red)
	}
}

// The owner's webhook doors never see the app's hook: list, delete, test.
func TestAppWebhook_HiddenFromOwnerDoors(t *testing.T) {
	u := newUpgradeEnv(t)
	h := u.hook(t)
	base := "/api/v1/workspaces/" + u.ws + "/webhooks"
	rr := doRequestWithCookie(u.srv, "GET", base, nil, u.token)
	if rr.Code != http.StatusOK || strings.Contains(rr.Body.String(), h.id) {
		t.Fatalf("owner list showed the app hook: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(u.srv, "POST", base+"/"+h.id+"/test", nil, u.token); rr.Code != http.StatusNotFound {
		t.Fatalf("owner test ping on the app hook: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(u.srv, "DELETE", base+"/"+h.id, nil, u.token); rr.Code != http.StatusNotFound {
		t.Fatalf("owner delete of the app hook: %d %s", rr.Code, rr.Body.String())
	}
	if u.hook(t) == nil {
		t.Fatal("the owner delete removed the app hook")
	}
	// The owner dispatcher reads ListWebhooks, and the plan counts owner hooks.
	if hooks, err := u.srv.store.ListWebhooks(u.wsID); err != nil || len(hooks) != 0 {
		t.Fatalf("ListWebhooks (the owner dispatcher's read) returned %d hooks (%v)", len(hooks), err)
	}
	if lim, err := u.srv.store.CheckLimit(u.wsID, "webhooks"); err != nil || lim.Current != 0 {
		t.Fatalf("the app hook counted against the owner's plan: %+v (%v)", lim, err)
	}
}

func TestAppWebhook_FollowsUpgrade(t *testing.T) {
	u := newUpgradeEnv(t)
	u.issueAndRedeem(t)
	before := u.hook(t)

	// A new URL keeps the secret and the released hold.
	u.m["version"] = "1.0.1"
	u.m["webhook_url"] = u.origin() + "/hooks/v2"
	u.publish(t, u.m)
	_, p, _ := u.previewUpgrade(t)
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", code, body)
	}
	after := u.hook(t)
	if after == nil || after.id != before.id || after.url != u.origin()+"/hooks/v2" || after.secret != before.secret || !after.delivered.Valid {
		t.Fatalf("upgrade hook: %+v (was %+v)", after, before)
	}

	// Dropping every event removes the hook.
	u.m["version"] = "1.0.2"
	delete(u.m, "events")
	delete(u.m, "webhook_url")
	u.publish(t, u.m)
	_, p, _ = u.previewUpgrade(t)
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", code, body)
	}
	if u.hook(t) != nil {
		t.Fatal("an upgrade that drops every event kept the hook")
	}

	// Declaring events again creates a NEW hook, held.
	u.m["version"] = "1.0.3"
	u.m["events"] = []any{map[string]any{"name": "item.created", "collections": []any{"tickets"}}}
	u.m["webhook_url"] = u.origin() + "/hooks"
	u.publish(t, u.m)
	_, p, _ = u.previewUpgrade(t)
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", code, body)
	}
	if h := u.hook(t); h == nil || h.delivered.Valid {
		t.Fatalf("re-declared hook: %+v, want held", h)
	}
}

func TestAppWebhook_UninstallRemovesIt(t *testing.T) {
	u := newUpgradeEnv(t)
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/uninstall", nil, u.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	if u.hook(t) != nil {
		t.Fatal("uninstall left the app hook")
	}
}

// Installs provisioned before U10 get their hook at start, held, once.
func TestAppWebhook_Backfill(t *testing.T) {
	u := newUpgradeEnv(t)
	u.issueAndRedeem(t)
	db := u.srv.store.DB()
	if _, err := db.Exec(`DELETE FROM webhooks WHERE app_install_id = ?`, u.installID); err != nil {
		t.Fatal(err)
	}
	u.srv.EnsureAppWebhooks(context.Background())
	h := u.hook(t)
	if h == nil || h.delivered.Valid {
		t.Fatalf("backfilled hook: %+v, want created held", h)
	}
	u.srv.EnsureAppWebhooks(context.Background())
	if n := u.count(t, `SELECT COUNT(*) FROM webhooks WHERE app_install_id = ?`, u.installID); n != 1 {
		t.Fatalf("a second backfill left %d hooks", n)
	}
	if got := u.getInstall(t); got.Webhook == nil || got.Webhook.Status != "awaiting_secret" {
		t.Fatalf("backfilled view: %+v", got.Webhook)
	}

	// An uninstalled install is not backfilled.
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/uninstall", nil, u.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("uninstall: %d %s", rr.Code, rr.Body.String())
	}
	u.srv.EnsureAppWebhooks(context.Background())
	if u.hook(t) != nil {
		t.Fatal("the backfill created a hook for an uninstalled install")
	}
	// The same under the row lock, for an install that left the list's
	// states after it was read.
	if err := u.srv.store.EnsureAppWebhook(u.wsID, u.installID, appWebhookSpec(manifestOf(t, u.m))); err != nil {
		t.Fatal(err)
	}
	if u.hook(t) != nil {
		t.Fatal("EnsureAppWebhook created a hook for an uninstalled install")
	}
}

func manifestOf(t *testing.T, m map[string]any) *appmanifest.Manifest {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out appmanifest.Manifest
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// An event resolves only to THIS install's companions: a collection that now
// holds a companion's slug but is not the app's is never subscribed (the app
// would receive events about data it was never granted).
func TestAppWebhook_ResolvesOnlyOwnCompanions(t *testing.T) {
	u := newUpgradeEnv(t)
	db := u.srv.store.DB()
	if _, err := db.Exec(`DELETE FROM webhooks WHERE app_install_id = ?`, u.installID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE collections SET slug = 'portal-tickets-old' WHERE workspace_id = ? AND slug = 'portal-tickets'`, u.wsID); err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/collections", map[string]any{"name": "Portal tickets", "slug": "portal-tickets"}, u.token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	err := u.srv.store.EnsureAppWebhook(u.wsID, u.installID, appWebhookSpec(manifestOf(t, u.m)))
	if err == nil || u.hook(t) != nil {
		t.Fatalf("subscribed to a collection that is not the app's: err %v, hook %+v", err, u.hook(t))
	}
}
