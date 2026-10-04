package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3397 (U8b): provisioning, install codes and redeem.

const testProvisionAudience = "https://pad.test.example/api/app"

func newProvisionEnv(t *testing.T) *appsEnv {
	t.Helper()
	e := newAppsEnv(t)
	e.srv.store.SetAppAPIAudience(testProvisionAudience)
	return e
}

// stagePreview previews and returns the preview the owner reviews.
func (e *appsEnv) stagePreview(t *testing.T) appPreview {
	t.Helper()
	rr := e.preview(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	var p appPreview
	parseJSON(t, rr, &p)
	return p
}

func (e *appsEnv) confirm(t *testing.T, p appPreview, manifestSHA string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/install/pending/"+p.PendingID+"/confirm",
		map[string]any{"manifest_sha256": manifestSHA}, e.token)
}

// provisionCensus counts every table provisioning writes.
var provisionCensusTables = []string{
	"app_installs", "app_install_codes", "app_install_pending", "app_install_pending_blobs",
	"users", "workspace_members", "member_collection_access", "oauth_clients",
	"collections", "items", "item_versions", "event_outbox", "activities",
}

func provisionCensus(t *testing.T, e *appsEnv) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, tbl := range provisionCensusTables {
		var n int
		if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM ` + tbl).Scan(&n); err != nil {
			t.Fatalf("census %s: %v", tbl, err)
		}
		out[tbl] = n
	}
	return out
}

func assertCensusUnchanged(t *testing.T, before, after map[string]int) {
	t.Helper()
	for _, tbl := range provisionCensusTables {
		if before[tbl] != after[tbl] {
			t.Errorf("a refused provisioning wrote %s: %d -> %d rows", tbl, before[tbl], after[tbl])
		}
	}
}

func redeem(srv *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/app/v1/install/redeem", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func TestAppInstallConfirm_ProvisionsEverything(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	if out.InstallID == "" || !strings.HasPrefix(out.InstallCode, "padic_") || len(out.Items) != 1 || out.Items[0].Status != "draft" {
		t.Fatalf("confirm response: %+v", out)
	}
	db := e.srv.store.DB()

	var state, origin, manifestSHA, version, svc, digests string
	var epoch int
	var botID *string
	if err := db.QueryRow(`SELECT state, auth_epoch, origin, manifest_sha256, manifest_version, service_access, digests, bot_user_id FROM app_installs WHERE id = ?`, out.InstallID).
		Scan(&state, &epoch, &origin, &manifestSHA, &version, &svc, &digests, &botID); err != nil {
		t.Fatal(err)
	}
	if state != "active" || epoch != 1 || origin != e.origin() || manifestSHA != p.ManifestSHA256 || version != "1.0.0" || svc != "write" {
		t.Errorf("install row: %s %d %s %s %s %s", state, epoch, origin, manifestSHA, version, svc)
	}
	if !strings.Contains(digests, p.Artifacts[0].NormalizedSHA256) || !strings.Contains(digests, p.Artifacts[0].RawSHA256) {
		t.Errorf("digests %s lack the artifact's raw and normalized hashes", digests)
	}
	if botID == nil {
		t.Fatal("bot_user_id not set")
	}

	// The bot: kind app, editor (service access write), scoped to exactly the
	// companion collection.
	var kind, role, access string
	if err := db.QueryRow(`SELECT kind FROM users WHERE id = ?`, *botID).Scan(&kind); err != nil || kind != models.UserKindApp {
		t.Errorf("bot kind %q (%v)", kind, err)
	}
	if err := db.QueryRow(`SELECT role, collection_access FROM workspace_members WHERE workspace_id = ? AND user_id = ?`, e.wsID, *botID).Scan(&role, &access); err != nil {
		t.Fatal(err)
	}
	if role != "editor" || access != "specific" {
		t.Errorf("bot membership %s/%s, want editor/specific", role, access)
	}
	companion, err := e.srv.store.GetCollectionBySlug(e.wsID, "portal-tickets")
	if err != nil || companion == nil {
		t.Fatalf("companion collection: %v", err)
	}
	granted, err := e.srv.store.GetMemberCollectionAccess(e.wsID, *botID)
	if err != nil || len(granted) != 1 || granted[0] != companion.ID {
		t.Errorf("bot collection grants %v (%v), want exactly %s", granted, err, companion.ID)
	}
	var viaApp *string
	if err := db.QueryRow(`SELECT via_app FROM collections WHERE id = ?`, companion.ID).Scan(&viaApp); err != nil || viaApp == nil || *viaApp != out.InstallID {
		t.Errorf("companion via_app %v (%v)", viaApp, err)
	}

	// The client exists, bound to the install.
	if n := appRows(t, e, `SELECT COUNT(*) FROM oauth_clients WHERE app_install_id = ?`, out.InstallID); n != 1 {
		t.Errorf("%d install clients", n)
	}

	// The artifact: a draft created by the owner, stamped with its pack and
	// both digests.
	item, err := e.srv.store.ResolveItem(e.wsID, out.Items[0].Ref)
	if err != nil || item == nil {
		t.Fatalf("artifact item: %v", err)
	}
	var pack, rawSHA, instSHA string
	if err := db.QueryRow(`SELECT source_pack, source_artifact_sha256, installed_sha256 FROM items WHERE id = ?`, item.ID).Scan(&pack, &rawSHA, &instSHA); err != nil {
		t.Fatal(err)
	}
	if pack != e.origin()+"@1.0.0" || rawSHA != p.Artifacts[0].RawSHA256 || instSHA != p.Artifacts[0].NormalizedSHA256 {
		t.Errorf("artifact stamp %s %s %s", pack, rawSHA, instSHA)
	}
	if item.CreatedBy != "user" {
		t.Errorf("artifact created_by %q, want the owner's kind", item.CreatedBy)
	}

	// The pending record is consumed with its blobs.
	if n := e.pendingRows(t); n != 0 {
		t.Errorf("%d pending rows left", n)
	}
	if n := appRows(t, e, `SELECT COUNT(*) FROM app_install_pending_blobs`); n != 0 {
		t.Errorf("%d pending blobs left", n)
	}
}

func appRows(t *testing.T, e *appsEnv, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The confirm must name the manifest the owner reviewed.
func TestAppInstallConfirm_ManifestMismatchWritesNothing(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	before := provisionCensus(t, e)
	rr := e.confirm(t, p, strings.Repeat("0", 64))
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "install_review_mismatch") {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
	assertCensusUnchanged(t, before, provisionCensus(t, e))
}

// The workspace changed between preview and confirm: the re-normalization
// differs, the owner is told what changed, and nothing is written. Each
// sub-case moves a different state the preview depended on.
func TestAppInstallConfirm_ReviewStale(t *testing.T) {
	cases := map[string]struct {
		move     func(t *testing.T, e *appsEnv)
		wantText []string
	}{
		"invocation slug squatted": {
			move: func(t *testing.T, e *appsEnv) {
				squatPlaybook(t, e, "Squatter", "ship")
			},
			wantText: []string{"invocation_slug", `reviewed \"ship\", now \"ship-2\"`},
		},
		"companion slug taken": {
			move: func(t *testing.T, e *appsEnv) {
				if _, err := e.srv.store.CreateCollection(e.wsID, models.CollectionCreate{Name: "Mine", Slug: "portal-tickets"}); err != nil {
					t.Fatal(err)
				}
			},
			wantText: []string{"portal-tickets"},
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newProvisionEnv(t)
			p := e.stagePreview(t)
			c.move(t, e)
			before := provisionCensus(t, e)
			rr := e.confirm(t, p, p.ManifestSHA256)
			if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "install_review_stale") {
				t.Fatalf("got %d %s", rr.Code, rr.Body.String())
			}
			for _, w := range c.wantText {
				if !strings.Contains(rr.Body.String(), w) {
					t.Errorf("refusal does not name %s: %s", w, rr.Body.String())
				}
			}
			assertCensusUnchanged(t, before, provisionCensus(t, e))
			if e.pendingRows(t) != 1 {
				t.Error("the pending record was not kept for a retry or a discard")
			}
		})
	}
}

func squatPlaybook(t *testing.T, e *appsEnv, title, slug string) {
	t.Helper()
	rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/collections/playbooks/items",
		map[string]any{"title": title, "fields": map[string]any{"invocation_slug": slug}}, e.token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("squat %q: %d %s", slug, rr.Code, rr.Body.String())
	}
}

// The single-transaction re-check is the guarantee: a write that lands AFTER
// the pre-transaction comparison passed is still refused inside the
// transaction, the refusal names artifact, field and the colliding item, and
// nothing is written (lead ruling, U8b).
func TestProvisionAppInstall_InTransactionRecheck(t *testing.T) {
	cases := map[string]struct {
		move      func(t *testing.T, e *appsEnv)
		artifact  string
		coll      string
		field     string
		wantInMsg string
	}{
		"invocation slug": {
			move:     func(t *testing.T, e *appsEnv) { squatPlaybook(t, e, "Late squatter", "ship") },
			artifact: "ship", field: "invocation_slug", wantInMsg: "PLAYB-",
		},
		"companion slug": {
			move: func(t *testing.T, e *appsEnv) {
				if _, err := e.srv.store.CreateCollection(e.wsID, models.CollectionCreate{Name: "Late", Slug: "portal-tickets"}); err != nil {
					t.Fatal(err)
				}
			},
			coll: "tickets", field: "slug", wantInMsg: "portal-tickets",
		},
		// Codex round 1: everything the re-check compares derives from the
		// destination schema, so a schema edit after the comparison refuses.
		"destination schema": {
			move: func(t *testing.T, e *appsEnv) {
				coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "playbooks")
				if err != nil || coll == nil {
					t.Fatalf("playbooks: %v", err)
				}
				var schema map[string]any
				if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
					t.Fatal(err)
				}
				schema["fields"] = append(schema["fields"].([]any), map[string]any{"key": "late", "label": "Late", "type": "text"})
				b, _ := json.Marshal(schema)
				sch := string(b)
				if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
					t.Fatal(err)
				}
			},
			artifact: "ship", wantInMsg: "schema changed",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newProvisionEnv(t)
			p := e.stagePreview(t)
			pending, err := e.srv.store.GetPendingInstall(p.PendingID, e.wsID, ownerIDOf(t, e))
			if err != nil {
				t.Fatal(err)
			}
			var reviewed appPreview
			if err := json.Unmarshal([]byte(pending.Preview), &reviewed); err != nil {
				t.Fatal(err)
			}
			req, err := e.srv.buildProvisionRequest(ownerRequest(t, e), e.wsID, pending.OwnerID, pending, &reviewed)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			c.move(t, e) // lands after the comparison, before the transaction
			before := provisionCensus(t, e)
			_, err = e.srv.store.ProvisionAppInstall(*req)
			var pc *store.ProvisionConflictError
			if !errors.As(err, &pc) {
				t.Fatalf("got %v, want a ProvisionConflictError", err)
			}
			if pc.Artifact != c.artifact || pc.Collection != c.coll || pc.Field != c.field || !strings.Contains(pc.Detail, c.wantInMsg) {
				t.Errorf("conflict %+v; want artifact %q collection %q field %q naming %q", pc, c.artifact, c.coll, c.field, c.wantInMsg)
			}
			assertCensusUnchanged(t, before, provisionCensus(t, e))
		})
	}
}

func ownerIDOf(t *testing.T, e *appsEnv) string {
	t.Helper()
	var id string
	if err := e.srv.store.DB().QueryRow(`SELECT owner_id FROM app_install_pending LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// Redeem consumes the code once and returns the secret once; a reissued code
// rotates the secret again, and the earlier secret stops matching.
func TestAppInstallRedeem(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	if rr.Code != http.StatusCreated {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	hashOf := func() string {
		var h string
		if err := e.srv.store.DB().QueryRow(`SELECT client_secret_hash FROM oauth_clients WHERE app_install_id = ?`, out.InstallID).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	provisioned := hashOf()

	rr = redeem(e.srv, `{"code":"`+out.InstallCode+`"}`)
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("redeem: %d %s", rr.Code, rr.Body.String())
	}
	var first map[string]string
	parseJSON(t, rr, &first)
	if first["install_id"] != out.InstallID || first["client_id"] == "" || !strings.HasPrefix(first["client_secret"], "padapp_") {
		t.Fatalf("redeem body: %v", first)
	}
	afterFirst := hashOf()
	if afterFirst == provisioned {
		t.Error("redeem did not rotate the provisioning secret")
	}

	// Single use.
	if rr := redeem(e.srv, `{"code":"`+out.InstallCode+`"}`); rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_install_code") {
		t.Fatalf("second redeem: %d %s", rr.Code, rr.Body.String())
	}

	// Recovery: the owner issues a new code; redeeming it rotates again.
	rr = doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/"+out.InstallID+"/install-code", nil, e.token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("issue code: %d %s", rr.Code, rr.Body.String())
	}
	var issued map[string]any
	parseJSON(t, rr, &issued)
	rr = redeem(e.srv, `{"code":"`+issued["install_code"].(string)+`"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("redeem reissued: %d %s", rr.Code, rr.Body.String())
	}
	var second map[string]string
	parseJSON(t, rr, &second)
	if second["client_secret"] == first["client_secret"] || hashOf() == afterFirst {
		t.Error("the reissued code's redeem did not rotate the secret")
	}
}

// Every refusal is the same 400, and the body cap holds.
func TestAppInstallRedeem_Refusals(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)

	if _, err := e.srv.store.DB().Exec(`UPDATE app_install_codes SET expires_at = '2000-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	// A VALID code padded past the cap: only the cap refuses it (a padded
	// unknown code would be refused without one).
	fresh, _, err := e.srv.store.IssueInstallCode(e.wsID, out.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	big := `{"code":"` + fresh + `"}` + strings.Repeat(" ", appRedeemMaxBody)
	for name, body := range map[string]string{
		"unknown":  `{"code":"padic_nope"}`,
		"expired":  `{"code":"` + out.InstallCode + `"}`,
		"empty":    `{}`,
		"too big":  big,
		"not json": `code=1`,
	} {
		rr := redeem(e.srv, body)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid_install_code") {
			t.Errorf("%s: %d %s", name, rr.Code, rr.Body.String())
		}
	}
	// An inactive install's code is the same refusal.
	code, _, err := e.srv.store.IssueInstallCode(e.wsID, out.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.store.DB().Exec(`UPDATE app_installs SET state = 'inactive'`); err != nil {
		t.Fatal(err)
	}
	if rr := redeem(e.srv, `{"code":"`+code+`"}`); rr.Code != http.StatusBadRequest {
		t.Errorf("inactive install: %d %s", rr.Code, rr.Body.String())
	}
}

// Redeem is unauthenticated and not under /api/v1: a cookie or bearer is
// neither needed nor consulted, and apps being off answers 404.
func TestAppInstallRedeem_GateAndMount(t *testing.T) {
	e := newProvisionEnv(t)
	rr := doRequestWithCookie(e.srv, "PUT", "/api/v1/admin/apps", map[string]any{"enabled": false}, e.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("disable: %d", rr.Code)
	}
	req := httptest.NewRequest("POST", "/api/app/v1/install/redeem", bytes.NewReader([]byte(`{"code":"padic_x"}`)))
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("apps off: %d %s", rec.Code, rec.Body.String())
	}
}

// The per-address limiter refuses a stream of probes.
func TestAppInstallRedeem_RateLimited(t *testing.T) {
	e := newProvisionEnv(t)
	limited := false
	for i := 0; i < 30; i++ {
		if rr := redeem(e.srv, `{"code":"padic_probe"}`); rr.Code == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if e.srv.rateLimiters != nil && !limited {
		t.Fatal("30 probes from one address were never limited")
	}
}

// The store re-checks the reviewed manifest hash itself, so a caller that
// skipped the handler's comparison still cannot provision another record.
func TestProvisionAppInstall_RefusesAnUnreviewedManifest(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	pending, err := e.srv.store.GetPendingInstall(p.PendingID, e.wsID, ownerIDOf(t, e))
	if err != nil {
		t.Fatal(err)
	}
	var reviewed appPreview
	if err := json.Unmarshal([]byte(pending.Preview), &reviewed); err != nil {
		t.Fatal(err)
	}
	req, err := e.srv.buildProvisionRequest(ownerRequest(t, e), e.wsID, pending.OwnerID, pending, &reviewed)
	if err != nil {
		t.Fatal(err)
	}
	req.ManifestSHA256 = strings.Repeat("0", 64)
	before := provisionCensus(t, e)
	if _, err := e.srv.store.ProvisionAppInstall(*req); !errors.Is(err, store.ErrPendingManifestChanged) {
		t.Fatalf("got %v, want ErrPendingManifestChanged", err)
	}
	assertCensusUnchanged(t, before, provisionCensus(t, e))
}

// A relation value reaches an artifact only through a schema default (an
// artifact carries a fixed key set). The preview resolves it; if its target
// is deleted after the comparison, the transaction refuses, naming the field.
func TestProvisionAppInstall_RelationRecheck(t *testing.T) {
	for _, deleteTarget := range []bool{false, true} {
		name := "target kept"
		if deleteTarget {
			name = "target deleted"
		}
		t.Run(name, func(t *testing.T) {
			e := newProvisionEnv(t)
			people, err := e.srv.store.CreateCollection(e.wsID, models.CollectionCreate{Name: "People", Slug: "people"})
			if err != nil {
				t.Fatal(err)
			}
			target, err := e.srv.store.CreateItem(e.wsID, people.ID, models.ItemCreate{Title: "Ada"})
			if err != nil {
				t.Fatal(err)
			}
			coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "playbooks")
			if err != nil || coll == nil {
				t.Fatalf("playbooks: %v", err)
			}
			var schema map[string]any
			if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
				t.Fatal(err)
			}
			schema["fields"] = append(schema["fields"].([]any), map[string]any{
				"key": "owner", "label": "Owner", "type": "relation", "collection": "people", "default": target.ID,
			})
			b, _ := json.Marshal(schema)
			sch := string(b)
			if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
				t.Fatal(err)
			}
			p := e.stagePreview(t)
			if p.Artifacts[0].Normalized.Fields["owner"] != target.ID {
				t.Fatalf("preview owner %v, want the resolved default %s", p.Artifacts[0].Normalized.Fields["owner"], target.ID)
			}
			pending, err := e.srv.store.GetPendingInstall(p.PendingID, e.wsID, ownerIDOf(t, e))
			if err != nil {
				t.Fatal(err)
			}
			var reviewed appPreview
			if err := json.Unmarshal([]byte(pending.Preview), &reviewed); err != nil {
				t.Fatal(err)
			}
			req, err := e.srv.buildProvisionRequest(ownerRequest(t, e), e.wsID, pending.OwnerID, pending, &reviewed)
			if err != nil {
				t.Fatal(err)
			}
			if req.Artifacts[0].RelationTargets["owner"] != "people" {
				t.Fatalf("relation targets %v: the handler did not pass the resolved relation to the re-check", req.Artifacts[0].RelationTargets)
			}
			if deleteTarget {
				if err := e.srv.store.DeleteItem(target.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := provisionCensus(t, e)
			_, err = e.srv.store.ProvisionAppInstall(*req)
			if !deleteTarget {
				if err != nil {
					t.Fatalf("control: %v", err)
				}
				return
			}
			var pc *store.ProvisionConflictError
			if !errors.As(err, &pc) || pc.Artifact != "ship" || pc.Field != "owner" || !strings.Contains(pc.Detail, target.ID) {
				t.Fatalf("got %v (%+v), want a conflict on ship/owner naming %s", err, pc, target.ID)
			}
			assertCensusUnchanged(t, before, provisionCensus(t, e))
		})
	}
}

// The store re-checks the pending record itself: not staged, expired, or
// another owner's or workspace's record is refused with nothing written.
func TestProvisionAppInstall_PendingRecordGuards(t *testing.T) {
	for name, mutate := range map[string]func(t *testing.T, e *appsEnv, req *store.ProvisionRequest){
		"not staged": func(t *testing.T, e *appsEnv, req *store.ProvisionRequest) {
			if _, err := e.srv.store.DB().Exec(`UPDATE app_install_pending SET state = 'fetching'`); err != nil {
				t.Fatal(err)
			}
		},
		"expired": func(t *testing.T, e *appsEnv, req *store.ProvisionRequest) {
			if _, err := e.srv.store.DB().Exec(`UPDATE app_install_pending SET expires_at = '2000-01-01T00:00:00Z'`); err != nil {
				t.Fatal(err)
			}
		},
		"another owner": func(t *testing.T, e *appsEnv, req *store.ProvisionRequest) { req.OwnerID = "someone-else" },
		"another workspace": func(t *testing.T, e *appsEnv, req *store.ProvisionRequest) {
			req.WorkspaceID = "other-ws"
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newProvisionEnv(t)
			p := e.stagePreview(t)
			pending, err := e.srv.store.GetPendingInstall(p.PendingID, e.wsID, ownerIDOf(t, e))
			if err != nil {
				t.Fatal(err)
			}
			var reviewed appPreview
			if err := json.Unmarshal([]byte(pending.Preview), &reviewed); err != nil {
				t.Fatal(err)
			}
			req, err := e.srv.buildProvisionRequest(ownerRequest(t, e), e.wsID, pending.OwnerID, pending, &reviewed)
			if err != nil {
				t.Fatal(err)
			}
			mutate(t, e, req)
			before := provisionCensus(t, e)
			if _, err := e.srv.store.ProvisionAppInstall(*req); !errors.Is(err, store.ErrPendingNotStaged) {
				t.Fatalf("got %v, want ErrPendingNotStaged", err)
			}
			assertCensusUnchanged(t, before, provisionCensus(t, e))
		})
	}
}

// ownerRequest is a request carrying the owner, as the confirm handler's
// does after the auth middleware: relation defaults resolve against what the
// caller can see, so a bare request would resolve differently.
func ownerRequest(t *testing.T, e *appsEnv) *http.Request {
	t.Helper()
	u, err := e.srv.store.GetUser(ownerIDOf(t, e))
	if err != nil || u == nil {
		t.Fatalf("owner: %v", err)
	}
	r := httptest.NewRequest("POST", "/", nil)
	ctx := WithCurrentUser(r.Context(), u)
	ctx = context.WithValue(ctx, ctxWorkspaceRole, "owner")
	return r.WithContext(ctx)
}

// Codex round 1: a reserved companion slug would be rewritten to
// "<slug>-collection", which the app cannot address. The preview refuses it,
// and the store refuses a de-collided OR rewritten slug as a backstop.
func TestAppInstall_ReservedCompanionSlug(t *testing.T) {
	e := newProvisionEnv(t)
	m := e.manifest(t)
	m["companion_pack"].(map[string]any)["collections"].([]any)[0].(map[string]any)["slug"] = "settings"
	e.publish(t, m)
	rr := e.preview(t)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "reserved") {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}

	// The store backstop, reached directly.
	e2 := newProvisionEnv(t)
	p := e2.stagePreview(t)
	pending, err := e2.srv.store.GetPendingInstall(p.PendingID, e2.wsID, ownerIDOf(t, e2))
	if err != nil {
		t.Fatal(err)
	}
	var reviewed appPreview
	if err := json.Unmarshal([]byte(pending.Preview), &reviewed); err != nil {
		t.Fatal(err)
	}
	req, err := e2.srv.buildProvisionRequest(ownerRequest(t, e2), e2.wsID, pending.OwnerID, pending, &reviewed)
	if err != nil {
		t.Fatal(err)
	}
	req.Collections[0].Slug = "settings"
	before := provisionCensus(t, e2)
	_, err = e2.srv.store.ProvisionAppInstall(*req)
	var pc *store.ProvisionConflictError
	if !errors.As(err, &pc) || pc.Collection != "tickets" || pc.Field != "slug" {
		t.Fatalf("got %v, want a slug conflict on tickets", err)
	}
	assertCensusUnchanged(t, before, provisionCensus(t, e2))
}

// Codex round 1: a dead code (expired or consumed) must answer exactly like an
// unknown one. It never charges the install bucket, so it can never draw a
// 429 the unknown code would not, and it cannot throttle a fresh code.
func TestAppInstallRedeem_DeadCodeNeverChargesTheInstallBucket(t *testing.T) {
	e := newProvisionEnv(t)
	p := e.stagePreview(t)
	rr := e.confirm(t, p, p.ManifestSHA256)
	var out appInstallConfirmResponse
	parseJSON(t, rr, &out)
	if _, err := e.srv.store.DB().Exec(`UPDATE app_install_codes SET expires_at = '2000-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	// Fewer than the per-address burst (10), more than the per-install one (5).
	for i := 0; i < 8; i++ {
		if rr := redeem(e.srv, `{"code":"`+out.InstallCode+`"}`); rr.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d with a dead code: %d %s", i+1, rr.Code, rr.Body.String())
		}
	}
	// A fresh code for the same install still redeems.
	code, _, err := e.srv.store.IssueInstallCode(e.wsID, out.InstallID)
	if err != nil {
		t.Fatal(err)
	}
	if rr := redeem(e.srv, `{"code":"`+code+`"}`); rr.Code != http.StatusOK {
		t.Fatalf("fresh code after dead-code attempts: %d %s", rr.Code, rr.Body.String())
	}
}
