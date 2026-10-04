package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/webhooks"
)

// TASK-3408 (U10b): app webhook delivery end to end, through the outbox
// drain to the test app's /hooks.

type deliveryEnv struct {
	*upgradeEnv
	companion *models.Collection
	secret    string
}

// newDeliveryEnv installs the test app, flags its pinned origin for webhooks
// as well as fetches, attaches a dispatcher, and drains what provisioning
// emitted. It does NOT redeem: the hook starts held.
func newDeliveryEnv(t *testing.T) *deliveryEnv {
	t.Helper()
	u := newUpgradeEnv(t)
	rr := doRequestWithCookie(u.srv, "PUT", "/api/v1/admin/apps", map[string]any{
		"enabled":         true,
		"private_origins": []map[string]any{{"origin": u.app.URL, "allowed": []string{"127.0.0.1"}, "fetch": true, "webhook": true}},
	}, u.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("admin apps: %d %s", rr.Code, rr.Body.String())
	}
	d := webhooks.NewDispatcher(u.srv.store)
	d.SkipSSRF = true
	d.SetRetryBackoff(0)
	u.srv.SetWebhookDispatcher(d)
	companion, err := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if err != nil || companion == nil {
		t.Fatal(err)
	}
	e := &deliveryEnv{upgradeEnv: u, companion: companion}
	e.tick(t)
	return e
}

func (e *deliveryEnv) redeemNow(t *testing.T) {
	t.Helper()
	e.secret = e.issueAndRedeem(t)["webhook_secret"]
	if e.secret == "" {
		t.Fatal("redeem handed out no webhook secret")
	}
}

func (e *deliveryEnv) tick(t *testing.T) {
	t.Helper()
	e.srv.runOutboxDrainTick()
}

func (e *deliveryEnv) item(t *testing.T, collectionID, title string) *models.Item {
	t.Helper()
	it, err := e.srv.store.CreateItem(e.wsID, collectionID, models.ItemCreate{Title: title, Content: "body of " + title, Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	return it
}

func (e *deliveryEnv) hooksAt(path string) []recordedHook {
	var out []recordedHook
	for _, h := range e.receivedHooks() {
		if h.Path == path {
			out = append(out, h)
		}
	}
	return out
}

func (e *deliveryEnv) pendingOutbox(t *testing.T) int {
	t.Helper()
	return e.count(t, `SELECT COUNT(*) FROM event_outbox WHERE dispatched_at IS NULL`)
}

func verifyAppSignature(t *testing.T, secret string, h recordedHook) {
	t.Helper()
	sig := h.Header.Get(webhooks.AppSignatureHeader)
	parts := strings.SplitN(sig, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		t.Fatalf("signature header %q", sig)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strings.TrimPrefix(parts[0], "t=") + "."))
	mac.Write(h.Body)
	if !hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(strings.TrimPrefix(parts[1], "v1="))) {
		t.Fatal("the delivery does not verify with the redeemed webhook secret")
	}
}

func TestAppDelivery_HeldUntilRedeemThenDelivered(t *testing.T) {
	e := newDeliveryEnv(t)
	e.item(t, e.companion.ID, "Before redeem")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("%d deliveries before the app was handed its secret", len(got))
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events left pending: a held hook's events are skipped, not queued", n)
	}

	e.redeemNow(t)
	it := e.item(t, e.companion.ID, "After redeem")
	e.tick(t)
	got := e.hooksAt("/hooks")
	if len(got) != 1 {
		t.Fatalf("%d deliveries after redeem, want 1", len(got))
	}
	h := got[0]
	verifyAppSignature(t, e.secret, h)
	if h.Header.Get(webhooks.AppInstallHeader) != e.installID || h.Header.Get("X-Pad-Signature") != "" {
		t.Fatalf("headers %v", h.Header)
	}
	var body map[string]any
	if err := json.Unmarshal(h.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["event"] != "item.created" || body["item_id"] != it.ID || body["collection_id"] != e.companion.ID ||
		body["title"] != "After redeem" || body["content"] != "body of After redeem" {
		t.Fatalf("body %s", h.Body)
	}
	if f, _ := body["fields"].(map[string]any); f["status"] != "open" {
		t.Fatalf("fields %v", body["fields"])
	}
	// Nothing of the stored snapshot beyond the table's keys.
	for _, k := range []string{"workspace_id", "slug", "app_projection", "data", "ref"} {
		if _, ok := body[k]; ok {
			t.Errorf("app body carries %q: %s", k, h.Body)
		}
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("%d events pending after a delivered event", n)
	}
}

func TestAppDelivery_OnlyTheAppsCollections(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	other, err := e.srv.store.CreateCollection(e.wsID, models.CollectionCreate{Name: "Internal", Slug: "internal"})
	if err != nil {
		t.Fatal(err)
	}
	e.item(t, other.ID, "Not the app's")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("delivered an event from a collection that is not the app's: %s", got[0].Body)
	}
}

func TestAppDelivery_CommentEvent(t *testing.T) {
	e := newDeliveryEnv(t)
	// Subscribe to comment.created as well.
	e.m["version"] = "1.0.1"
	e.m["events"] = []any{
		map[string]any{"name": "item.created", "collections": []any{"tickets"}},
		map[string]any{"name": "comment.created", "collections": []any{"tickets"}},
	}
	e.publish(t, e.m)
	_, p, _ := e.previewUpgrade(t)
	if code, body := e.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("upgrade: %d %s", code, body)
	}
	e.redeemNow(t)
	it := e.item(t, e.companion.ID, "Ticket")
	c, err := e.srv.store.CreateComment(e.wsID, it.ID, "", models.CommentCreate{Body: "a reply", Author: "Owner"})
	if err != nil {
		t.Fatal(err)
	}
	e.tick(t)
	var sawComment bool
	for _, h := range e.hooksAt("/hooks") {
		var body map[string]any
		_ = json.Unmarshal(h.Body, &body)
		if body["event"] != "comment.created" {
			continue
		}
		sawComment = true
		if body["comment_id"] != c.ID || body["item_id"] != it.ID || body["body"] != "a reply" || body["collection_id"] != e.companion.ID {
			t.Fatalf("comment body %s", h.Body)
		}
	}
	if !sawComment {
		t.Fatal("no comment.created delivery")
	}
}

func TestAppDelivery_DisabledAndRotatedDeliverNothing(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	base := "/api/v1/workspaces/" + e.ws + "/apps/" + e.installID
	if rr := doRequestWithCookie(e.srv, "POST", base+"/disable", nil, e.token); rr.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", rr.Code, rr.Body.String())
	}
	e.item(t, e.companion.ID, "While disabled")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("%d deliveries to a disabled install", len(got))
	}
	if rr := doRequestWithCookie(e.srv, "POST", base+"/enable", nil, e.token); rr.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", rr.Code, rr.Body.String())
	}
	// Disabled events are not queued for after the re-enable.
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("an event from while disabled was delivered after the re-enable")
	}

	if rr := doRequestWithCookie(e.srv, "POST", base+"/rotate", nil, e.token); rr.Code != http.StatusOK {
		t.Fatalf("rotate: %d %s", rr.Code, rr.Body.String())
	}
	e.item(t, e.companion.ID, "After rotate")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("%d deliveries after a rotate, before the app redeemed its new code", len(got))
	}
}

func TestAppDelivery_RedirectIsNotFollowed(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.status["/hooks"] = http.StatusFound
	e.item(t, e.companion.ID, "Redirected")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 1 {
		t.Fatalf("%d attempts at a 3xx endpoint, want 1 (permanent, no retry)", len(got))
	}
	if got := e.hooksAt("/elsewhere"); len(got) != 0 {
		t.Fatal("the redirect was followed")
	}
	if n := e.pendingOutbox(t); n != 0 {
		t.Fatalf("a permanent failure left %d events owed", n)
	}
}

func TestAppDelivery_TransientStaysOwed(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.status["/hooks"] = http.StatusServiceUnavailable
	e.item(t, e.companion.ID, "Flaky")
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 3 {
		t.Fatalf("%d attempts at a 503 endpoint, want 3", len(got))
	}
	if n := e.pendingOutbox(t); n == 0 {
		t.Fatal("a transient failure acked the event")
	}
}

// An event without an app-projection block is skipped, never rebuilt from
// live state.
func TestAppDelivery_NoProjectionIsSkipped(t *testing.T) {
	e := newDeliveryEnv(t)
	e.redeemNow(t)
	e.item(t, e.companion.ID, "Stripped")
	db := e.srv.store.DB()
	rows, err := db.Query(`SELECT id, payload FROM event_outbox WHERE dispatched_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ id, payload string }
	var rs []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.payload); err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	rows.Close()
	if len(rs) == 0 {
		t.Fatal("precondition: no pending event")
	}
	for _, r := range rs {
		var m map[string]any
		if err := json.Unmarshal([]byte(r.payload), &m); err != nil {
			t.Fatal(err)
		}
		if _, ok := m["app_projection"]; !ok {
			t.Fatal("precondition: the stored payload has no block to strip")
		}
		delete(m, "app_projection")
		b, _ := json.Marshal(m)
		if _, err := db.Exec(`UPDATE event_outbox SET payload = ? WHERE id = ?`, string(b), r.id); err != nil {
			t.Fatal(err)
		}
	}
	e.tick(t)
	if got := e.hooksAt("/hooks"); len(got) != 0 {
		t.Fatalf("delivered an event that had no app projection: %s", got[0].Body)
	}
}
