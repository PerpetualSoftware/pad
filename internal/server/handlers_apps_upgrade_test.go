package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3397 (U8b2): app upgrade, end to end over a provisioned install.

type upgradeEnv struct {
	*appsEnv
	installID string
	m         map[string]any
}

func newUpgradeEnv(t *testing.T) *upgradeEnv {
	t.Helper()
	e := newProvisionEnv(t)
	m := e.manifest(t)
	e.publish(t, m)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("install: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	return &upgradeEnv{appsEnv: e, installID: out.InstallID, m: m}
}

func (u *upgradeEnv) previewUpgrade(t *testing.T) (int, appPreview, string) {
	t.Helper()
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/upgrade/preview", nil, u.token)
	var p appPreview
	if rr.Code == http.StatusOK {
		parseJSON(t, rr, &p)
	}
	return rr.Code, p, rr.Body.String()
}

func (u *upgradeEnv) confirmUpgrade(t *testing.T, p appPreview) (int, string) {
	t.Helper()
	rr := doRequestWithCookie(u.srv, "POST", "/api/v1/workspaces/"+u.ws+"/apps/"+u.installID+"/upgrade/confirm",
		map[string]any{"pending_id": p.PendingID, "manifest_sha256": p.ManifestSHA256}, u.token)
	return rr.Code, rr.Body.String()
}

func (u *upgradeEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := u.srv.store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (u *upgradeEnv) botID(t *testing.T) string {
	t.Helper()
	var bot string
	if err := u.srv.store.DB().QueryRow(`SELECT bot_user_id FROM app_installs WHERE id = ?`, u.installID).Scan(&bot); err != nil {
		t.Fatal(err)
	}
	return bot
}

// Removals and narrowing only: no review needed, still applied by the confirm.
// The removed companion is RELEASED: via_app cleared, out of the bot's access,
// the collection and its items kept.
func TestAppUpgrade_RemovalsOnlyReleaseTheCompanion(t *testing.T) {
	u := newUpgradeEnv(t)
	companion, err := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if err != nil || companion == nil {
		t.Fatal(err)
	}
	if _, err := u.srv.store.CreateItem(u.wsID, companion.ID, models.ItemCreate{Title: "a ticket"}); err != nil {
		t.Fatal(err)
	}
	// A manifest needs one companion, so first add a second (a reviewed
	// upgrade), then release the original in a removals-only one.
	m := u.m
	faq := map[string]any{"key": "faq", "slug": "portal-faq", "name": "FAQ",
		"schema": map[string]any{"fields": []any{map[string]any{"key": "answer", "label": "Answer", "type": "text"}}}}
	m["version"] = "1.0.5"
	m["companion_pack"].(map[string]any)["collections"] = append(m["companion_pack"].(map[string]any)["collections"].([]any), faq)
	u.publish(t, m)
	if code, p, body := u.previewUpgrade(t); code != http.StatusOK {
		t.Fatalf("add faq preview: %d %s", code, body)
	} else if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("add faq confirm: %d %s", code, body)
	}

	m["version"] = "1.1.0"
	m["events"] = []any{}
	m["item_actions"] = []any{}
	m["scopes"] = map[string]any{"service": map[string]any{"access": "read"}, "delegated": map[string]any{"access": "read"}}
	m["companion_pack"].(map[string]any)["collections"] = []any{faq}
	u.publish(t, m)
	code, p, body := u.previewUpgrade(t)
	if code != http.StatusOK || p.Upgrade == nil {
		t.Fatalf("preview: %d %s", code, body)
	}
	if p.Upgrade.ReviewRequired {
		t.Errorf("removals and narrowing required review: %+v", p.Upgrade.Diff)
	}
	if !strings.Contains(body, "released to the workspace (data kept)") {
		t.Errorf("the release is not shown: %s", body)
	}
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, body)
	}
	if n := u.count(t, `SELECT COUNT(*) FROM collections WHERE id = ? AND via_app IS NULL AND deleted_at IS NULL`, companion.ID); n != 1 {
		t.Fatal("the released companion is gone or still the app's")
	}
	if n := u.count(t, `SELECT COUNT(*) FROM items WHERE collection_id = ? AND deleted_at IS NULL`, companion.ID); n != 1 {
		t.Fatalf("the released companion lost its items (%d)", n)
	}
	bot := u.botID(t)
	if n := u.count(t, `SELECT COUNT(*) FROM member_collection_access WHERE user_id = ? AND collection_id = ?`, bot, companion.ID); n != 0 {
		t.Fatal("the bot still has the released companion")
	}
	var role, version, access string
	if err := u.srv.store.DB().QueryRow(`SELECT role FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, u.wsID, bot).Scan(&role); err != nil || role != "viewer" {
		t.Fatalf("bot role %q (%v), want viewer after narrowing", role, err)
	}
	if err := u.srv.store.DB().QueryRow(`SELECT manifest_version, service_access FROM app_installs WHERE id = ?`, u.installID).Scan(&version, &access); err != nil || version != "1.1.0" || access != "read" {
		t.Fatalf("install row %s/%s (%v)", version, access, err)
	}
}

// A changed artifact lands as a NEW draft; the installed item is untouched.
// An unchanged artifact makes no draft.
func TestAppUpgrade_ChangedArtifactIsANewDraft(t *testing.T) {
	u := newUpgradeEnv(t)
	before := u.count(t, `SELECT COUNT(*) FROM items WHERE source_pack IS NOT NULL`)
	digestsOf := func() string {
		t.Helper()
		var d string
		if err := u.srv.store.DB().QueryRow(`SELECT digests FROM app_installs WHERE id = ?`, u.installID).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	installedDigests := digestsOf()

	// Every raw digest unchanged (a version bump only): no draft, and no
	// installed digest moves (lead ruling, day 86: changed = raw differs).
	u.m["version"] = "1.0.1"
	u.publish(t, u.m)
	code, p, body := u.previewUpgrade(t)
	if code != http.StatusOK {
		t.Fatalf("preview: %d %s", code, body)
	}
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, body)
	}
	if n := u.count(t, `SELECT COUNT(*) FROM items WHERE source_pack IS NOT NULL`); n != before {
		t.Fatalf("an unchanged artifact made a draft (%d -> %d)", before, n)
	}
	var was, now struct {
		Artifacts map[string]storedArtifactDigest `json:"artifacts"`
	}
	if err := json.Unmarshal([]byte(installedDigests), &was); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(digestsOf()), &now); err != nil {
		t.Fatal(err)
	}
	if now.Artifacts["ship"] != was.Artifacts["ship"] {
		t.Fatalf("an unchanged artifact's installed digests moved: %+v -> %+v", was.Artifacts["ship"], now.Artifacts["ship"])
	}

	// Changed raw bytes: a new draft, needing review.
	body2 := []byte(strings.Replace(string(u.files["/pack/ship.md"]), "title: Ship a change", "title: Ship a change v2", 1))
	u.files["/pack/ship.md"] = body2
	u.m["version"] = "1.1.0"
	u.m["companion_pack"].(map[string]any)["artifacts"] = []any{map[string]any{"key": "ship", "url": u.origin() + "/pack/ship.md", "sha256": appSHA(body2)}}
	u.publish(t, u.m)
	code, p, body = u.previewUpgrade(t)
	if code != http.StatusOK || !p.Upgrade.ReviewRequired {
		t.Fatalf("preview: %d %s", code, body)
	}
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, body)
	}
	if n := u.count(t, `SELECT COUNT(*) FROM items WHERE source_pack IS NOT NULL`); n != before+1 {
		t.Fatalf("a changed artifact made %d drafts", n-before)
	}
	if n := u.count(t, `SELECT COUNT(*) FROM items WHERE title = 'Ship a change' AND deleted_at IS NULL`); n != 1 {
		t.Fatal("the installed item was changed or removed")
	}
}

// Additive optional fields are appended to the current schema; a new
// companion is created and granted to the bot.
func TestAppUpgrade_AdditiveFieldAndNewCompanion(t *testing.T) {
	u := newUpgradeEnv(t)
	pack := u.m["companion_pack"].(map[string]any)
	coll := pack["collections"].([]any)[0].(map[string]any)
	fields := coll["schema"].(map[string]any)["fields"].([]any)
	coll["schema"].(map[string]any)["fields"] = append(fields, map[string]any{"key": "priority", "label": "Priority", "type": "select", "options": []any{"low", "high"}})
	pack["collections"] = append(pack["collections"].([]any), map[string]any{"key": "faq", "slug": "portal-faq", "name": "FAQ",
		"schema": map[string]any{"fields": []any{map[string]any{"key": "answer", "label": "Answer", "type": "text"}}}})
	u.m["version"] = "1.2.0"
	u.publish(t, u.m)
	code, p, body := u.previewUpgrade(t)
	if code != http.StatusOK || !p.Upgrade.ReviewRequired {
		t.Fatalf("preview: %d %s", code, body)
	}
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("confirm: %d %s", code, body)
	}
	tickets, _ := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if !strings.Contains(tickets.Schema, `"priority"`) {
		t.Fatalf("the additive field is missing: %s", tickets.Schema)
	}
	faq, err := u.srv.store.GetCollectionBySlug(u.wsID, "portal-faq")
	if err != nil || faq == nil {
		t.Fatalf("the new companion was not created: %v", err)
	}
	if n := u.count(t, `SELECT COUNT(*) FROM member_collection_access WHERE user_id = ? AND collection_id = ?`, u.botID(t), faq.ID); n != 1 {
		t.Fatal("the bot was not granted the new companion")
	}
}

// Refusals, at preview: a changed field, a removed field, and a non-optional
// addition. Nothing is staged.
func TestAppUpgrade_SchemaRefusals(t *testing.T) {
	for name, edit := range map[string]func(fields []any) []any{
		"changed field": func(f []any) []any { f[0].(map[string]any)["options"] = []any{"open"}; return f },
		"removed field": func(f []any) []any { return f[:0] },
		"required add": func(f []any) []any {
			return append(f, map[string]any{"key": "x", "label": "X", "type": "text", "required": true})
		},
	} {
		t.Run(name, func(t *testing.T) {
			u := newUpgradeEnv(t)
			coll := u.m["companion_pack"].(map[string]any)["collections"].([]any)[0].(map[string]any)
			sch := coll["schema"].(map[string]any)
			sch["fields"] = edit(sch["fields"].([]any))
			u.publish(t, u.m)
			pendingBefore := u.pendingRows(t)
			code, _, body := u.previewUpgrade(t)
			if code != http.StatusUnprocessableEntity || !strings.Contains(body, "upgrade_not_supported") {
				t.Fatalf("got %d %s; want 422 upgrade_not_supported", code, body)
			}
			if u.pendingRows(t) != pendingBefore {
				t.Fatal("a refused upgrade left a pending record")
			}
		})
	}
}

// The install moved between preview and confirm (another upgrade landed, or
// it was disabled): the confirm refuses and writes nothing.
func TestAppUpgrade_InstallMovedRefuses(t *testing.T) {
	u := newUpgradeEnv(t)
	u.m["version"] = "1.0.1"
	u.publish(t, u.m)
	_, first, _ := u.previewUpgrade(t)
	u.m["version"] = "1.0.2"
	u.publish(t, u.m)
	_, second, _ := u.previewUpgrade(t)
	if code, body := u.confirmUpgrade(t, second); code != http.StatusOK {
		t.Fatalf("second confirm: %d %s", code, body)
	}
	code, body := u.confirmUpgrade(t, first)
	if code != http.StatusConflict || !strings.Contains(body, "install_moved") {
		t.Fatalf("stale confirm: %d %s; want 409 install_moved", code, body)
	}
	var version string
	if err := u.srv.store.DB().QueryRow(`SELECT manifest_version FROM app_installs WHERE id = ?`, u.installID).Scan(&version); err != nil || version != "1.0.2" {
		t.Fatalf("version %q (%v)", version, err)
	}
}

// An upgrade's pending record never provisions as a fresh install, and an
// install's never confirms as an upgrade.
func TestAppUpgrade_RecordsDoNotCross(t *testing.T) {
	u := newUpgradeEnv(t)
	u.m["version"] = "1.0.1"
	u.publish(t, u.m)
	_, up, _ := u.previewUpgrade(t)
	rr := u.confirm(t, up, up.ManifestSHA256)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("an upgrade's pending record on the install route: %d %s, want 404", rr.Code, rr.Body.String())
	}
	if _, err := u.srv.store.DB().Exec(`UPDATE app_installs SET state = 'uninstalled' WHERE id = ?`, u.installID); err != nil {
		t.Fatal(err)
	}
	fresh := u.stagePreview(t) // an install preview (adopts the tombstone's companion)
	code, body := u.confirmUpgrade(t, fresh)
	if code != http.StatusNotFound {
		t.Fatalf("an install's pending record confirmed as an upgrade: %d %s", code, body)
	}
}

func (u *upgradeEnv) epoch(t *testing.T) int64 {
	t.Helper()
	var e int64
	if err := u.srv.store.DB().QueryRow(`SELECT auth_epoch FROM app_installs WHERE id = ?`, u.installID).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

func (u *upgradeEnv) addTicketField(f map[string]any) {
	coll := u.m["companion_pack"].(map[string]any)["collections"].([]any)[0].(map[string]any)
	sch := coll["schema"].(map[string]any)
	sch["fields"] = append(sch["fields"].([]any), f)
}

// codex r1 #1: an owner renamed a companion; the upgrade cannot map it by
// slug, so it refuses rather than leaving it the app's.
func TestAppUpgrade_RenamedCompanionRefuses(t *testing.T) {
	u := newUpgradeEnv(t)
	c, _ := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if _, err := u.srv.store.DB().Exec(`UPDATE collections SET slug = 'renamed-tickets' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	u.m["version"] = "1.0.1"
	u.publish(t, u.m)
	code, _, body := u.previewUpgrade(t)
	if code != http.StatusConflict || !strings.Contains(body, "upgrade_conflict") || !strings.Contains(body, "no longer resolves") {
		t.Fatalf("got %d %s; want 409 upgrade_conflict", code, body)
	}
}

// codex r1 #2: a restrictive upgrade bumps the epoch (admitted writes and
// earlier tokens die); a neutral one does not.
func TestAppUpgrade_RestrictiveUpgradeBumpsTheEpoch(t *testing.T) {
	u := newUpgradeEnv(t)
	e0 := u.epoch(t)
	u.m["version"] = "1.0.1"
	u.publish(t, u.m)
	_, p, _ := u.previewUpgrade(t)
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("neutral: %d %s", code, body)
	}
	if u.epoch(t) != e0 {
		t.Fatal("a neutral upgrade bumped the epoch")
	}
	u.m["version"] = "1.0.2"
	u.m["scopes"] = map[string]any{"service": map[string]any{"access": "read"}, "delegated": map[string]any{"access": "read"}}
	u.publish(t, u.m)
	_, p, _ = u.previewUpgrade(t)
	if code, body := u.confirmUpgrade(t, p); code != http.StatusOK {
		t.Fatalf("narrowing: %d %s", code, body)
	}
	if u.epoch(t) != e0+1 {
		t.Fatalf("a narrowing upgrade did not bump the epoch (%d -> %d)", e0, u.epoch(t))
	}
}

// codex r1 #3: items already hold values under the key a field would declare.
func TestAppUpgrade_AdditiveFieldOverExistingDataRefuses(t *testing.T) {
	u := newUpgradeEnv(t)
	c, _ := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if _, err := u.srv.store.CreateItem(u.wsID, c.ID, models.ItemCreate{Title: "held", Fields: `{"code":"same"}`}); err != nil {
		t.Fatal(err)
	}
	u.addTicketField(map[string]any{"key": "code", "label": "Code", "type": "text", "unique_scope": "workspace_collection"})
	u.publish(t, u.m)
	code, _, body := u.previewUpgrade(t)
	if code != http.StatusConflict || !strings.Contains(body, "already hold values") {
		t.Fatalf("got %d %s; want 409 naming the existing values", code, body)
	}
}

// codex r1 #4: board_group_by names the new select, so adding it would move
// the done field and reclassify every existing item.
func TestAppUpgrade_AdditiveFieldThatMovesTheDoneFieldRefuses(t *testing.T) {
	u := newUpgradeEnv(t)
	c, _ := u.srv.store.GetCollectionBySlug(u.wsID, "portal-tickets")
	if _, err := u.srv.store.DB().Exec(`UPDATE collections SET settings = '{"board_group_by":"resolution"}' WHERE id = ?`, c.ID); err != nil {
		t.Fatal(err)
	}
	u.addTicketField(map[string]any{"key": "resolution", "label": "Resolution", "type": "select", "options": []any{"fixed", "wontfix"}})
	u.publish(t, u.m)
	code, _, body := u.previewUpgrade(t)
	if code != http.StatusConflict || !strings.Contains(body, "done field") {
		t.Fatalf("got %d %s; want 409 naming the done field", code, body)
	}
}
