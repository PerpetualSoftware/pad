package server

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/oauth"
)

// TASK-3397 (U8a): app install staging and preview.

type appsEnv struct {
	srv    *Server
	token  string
	ws     string
	wsID   string
	app    *httptest.Server
	files  map[string][]byte // path -> body served by the app
	status map[string]int    // path -> status override

	// hookMu guards hooks: every POST the test app received (TASK-3408).
	hookMu sync.Mutex
	hooks  []recordedHook
}

type recordedHook struct {
	Path   string
	Header http.Header
	Body   []byte
}

func (e *appsEnv) receivedHooks() []recordedHook {
	e.hookMu.Lock()
	defer e.hookMu.Unlock()
	return append([]recordedHook(nil), e.hooks...)
}

func appSHA(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func newAppsEnv(t *testing.T) *appsEnv {
	t.Helper()
	srv := testServer(t)
	token := bootstrapFirstUser(t, srv, "owner@test.com", "Owner")
	rr := doRequestWithCookie(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Apps"}, token)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	srv.SetMCPConfig(config.MCPEndpoints{Origin: "https://pad.test.example", OriginVar: "PAD_URL",
		ResourceURL: "https://pad.test.example/mcp", AuthServerURL: "https://pad.test.example"}, nil)
	o, err := oauth.NewServer(oauth.Config{Store: srv.store, HMACSecret: bytes32ForTest(), AllowedAudience: "https://pad.test.example/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetOAuthServer(o)

	e := &appsEnv{srv: srv, token: token, ws: ws.Slug, wsID: ws.ID, files: map[string][]byte{}, status: map[string]int{}}
	e.app = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			e.hookMu.Lock()
			e.hooks = append(e.hooks, recordedHook{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
			e.hookMu.Unlock()
		}
		if code, ok := e.status[r.URL.Path]; ok {
			if code >= 300 && code < 400 {
				http.Redirect(w, r, "/elsewhere", code)
				return
			}
			w.WriteHeader(code)
			return
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, ok := e.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(e.app.Close)
	pool := x509.NewCertPool()
	pool.AddCert(e.app.Certificate())
	srv.SetAppFetchTLS(&tls.Config{RootCAs: pool})

	// Admin: enable apps and pin the test app's loopback origin.
	rr = doRequestWithCookie(srv, "PUT", "/api/v1/admin/apps", map[string]any{
		"enabled":         true,
		"private_origins": []map[string]any{{"origin": e.app.URL, "allowed": []string{"127.0.0.1"}, "fetch": true}},
	}, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("enable apps: %d %s", rr.Code, rr.Body.String())
	}
	e.publish(t, e.manifest(t))
	return e
}

func (e *appsEnv) origin() string { return e.app.URL }

// manifest returns a valid manifest for the test app with one companion
// collection and the golden playbook as its artifact.
func (e *appsEnv) manifest(t *testing.T) map[string]any {
	t.Helper()
	pb, err := os.ReadFile(filepath.Join("..", "artifact", "testdata", "playbook.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	e.files["/pack/ship.md"] = pb
	return map[string]any{
		"id": "acme/portal", "version": "1.0.0", "min_contract": map[string]any{"apps": 1, "events": 1},
		"title": "Portal", "publisher": "Acme", "base_url": e.origin(),
		"redirect_uris": []any{e.origin() + "/oauth/callback"},
		"scopes":        map[string]any{"service": map[string]any{"access": "write"}, "delegated": map[string]any{"access": "read"}},
		"events":        []any{map[string]any{"name": "item.created", "collections": []any{"tickets"}}},
		"webhook_url":   e.origin() + "/hooks",
		"companion_pack": map[string]any{
			"collections": []any{map[string]any{"key": "tickets", "slug": "portal-tickets", "name": "Portal tickets",
				"schema": map[string]any{"fields": []any{map[string]any{"key": "status", "label": "Status", "type": "select", "options": []any{"open", "solved"}}}}}},
			"artifacts": []any{map[string]any{"key": "ship", "url": e.origin() + "/pack/ship.md", "sha256": appSHA(pb)}},
		},
		"item_actions": []any{map[string]any{"key": "open", "label": "Open", "collections": []any{"tickets"}, "path": "/t"}},
	}
}

func (e *appsEnv) publish(t *testing.T, m map[string]any) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	e.files["/.well-known/pad-app.json"] = b
}

func (e *appsEnv) preview(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/install/preview", map[string]any{"base_url": e.origin()}, e.token)
}

func (e *appsEnv) pendingRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM app_install_pending`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAppInstallPreview_StagesAndPreviews(t *testing.T) {
	e := newAppsEnv(t)
	rr := e.preview(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	var p appPreview
	parseJSON(t, rr, &p)
	if p.ReviewedByPad || !strings.Contains(p.Notice, "Not reviewed by Pad") {
		t.Errorf("not-reviewed notice missing: %+v", p.Notice)
	}
	if p.Origin != e.origin() || p.ServiceAccess != "write" || p.DelegatedAccess != "read" || p.DeferredNotice == "" {
		t.Errorf("preview header: %+v", p)
	}
	if len(p.Collections) != 1 || p.Collections[0].Adopt {
		t.Errorf("collections: %+v", p.Collections)
	}
	if len(p.Artifacts) != 1 {
		t.Fatalf("artifacts: %+v", p.Artifacts)
	}
	a := p.Artifacts[0]
	if a.Raw == "" || a.RawSHA256 != appSHA(e.files["/pack/ship.md"]) || a.Normalized.Fields["status"] != "draft" {
		t.Errorf("artifact preview: raw=%d sha=%s status=%v", len(a.Raw), a.RawSHA256, a.Normalized.Fields["status"])
	}
	// Every field the importer changed is shown: the status reset, and the
	// select values this workspace does not offer.
	joined := strings.Join(a.Changes, "\n")
	for _, want := range []string{`reset to "draft"`, `"trigger" value "on-intent"`} {
		if !strings.Contains(joined, want) {
			t.Errorf("change %q not shown: %v", want, a.Changes)
		}
	}
	if want, _ := a.Normalized.digest(); a.NormalizedSHA256 != want {
		t.Errorf("normalized digest %s, want %s", a.NormalizedSHA256, want)
	}

	// The staged record serves the same preview, raw bytes included.
	rr = doRequestWithCookie(e.srv, "GET", "/api/v1/workspaces/"+e.ws+"/apps/install/pending/"+p.PendingID, nil, e.token)
	if rr.Code != http.StatusOK {
		t.Fatalf("get pending: %d %s", rr.Code, rr.Body.String())
	}
	var again appPreview
	parseJSON(t, rr, &again)
	if again.Artifacts[0].Raw != a.Raw || again.Artifacts[0].NormalizedSHA256 != a.NormalizedSHA256 {
		t.Error("the stored preview differs from the one returned")
	}
	var staged int64
	if err := e.srv.store.DB().QueryRow(`SELECT reserved_bytes FROM app_install_pending WHERE id = ?`, p.PendingID).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if want := int64(len(e.files["/.well-known/pad-app.json"]) + len(e.files["/pack/ship.md"])); staged != want {
		t.Errorf("charge after staging %d, want the %d staged bytes", staged, want)
	}

	rr = doRequestWithCookie(e.srv, "DELETE", "/api/v1/workspaces/"+e.ws+"/apps/install/pending/"+p.PendingID, nil, e.token)
	if rr.Code != http.StatusNoContent || e.pendingRows(t) != 0 {
		t.Fatalf("discard: %d, %d rows left", rr.Code, e.pendingRows(t))
	}
}

// Every refusal releases its reservation and stages nothing.
func TestAppInstallPreview_Refusals(t *testing.T) {
	for name, c := range map[string]struct {
		mutate func(t *testing.T, e *appsEnv)
		status int
		code   string
	}{
		"artifact hash mismatch": {func(t *testing.T, e *appsEnv) { e.files["/pack/ship.md"] = []byte("tampered") }, 422, "artifact_hash_mismatch"},
		"unsubscribable event": {func(t *testing.T, e *appsEnv) {
			m := e.manifest(t)
			m["events"] = []any{map[string]any{"name": "item.bulk_updated", "collections": []any{"tickets"}}}
			e.publish(t, m)
		}, 422, "invalid_manifest"},
		"manifest names another origin": {func(t *testing.T, e *appsEnv) {
			m := e.manifest(t)
			m["base_url"] = "https://other.example"
			m["redirect_uris"] = []any{"https://other.example/cb"}
			m["webhook_url"] = "https://other.example/h"
			pack := m["companion_pack"].(map[string]any)
			pack["artifacts"] = []any{}
			e.publish(t, m)
		}, 422, "invalid_manifest"},
		// TASK-3539: a companion schema is a collection write, held to the
		// same rule as the collection handlers.
		"companion schema repeats an option": {func(t *testing.T, e *appsEnv) {
			m := e.manifest(t)
			pack := m["companion_pack"].(map[string]any)
			pack["collections"] = []any{map[string]any{"key": "tickets", "slug": "portal-tickets", "name": "Portal tickets",
				"schema": map[string]any{"fields": []any{map[string]any{"key": "status", "label": "Status", "type": "select", "options": []any{"open", "open"}}}}}}
			e.publish(t, m)
		}, 422, "invalid_manifest"},
		"companion schema repeats a field key": {func(t *testing.T, e *appsEnv) {
			m := e.manifest(t)
			pack := m["companion_pack"].(map[string]any)
			pack["collections"] = []any{map[string]any{"key": "tickets", "slug": "portal-tickets", "name": "Portal tickets",
				"schema": map[string]any{"fields": []any{
					map[string]any{"key": "status", "label": "Status", "type": "text"},
					map[string]any{"key": "status", "label": "Again", "type": "text"}}}}}
			e.publish(t, m)
		}, 422, "invalid_manifest"},
		"manifest redirects": {func(t *testing.T, e *appsEnv) { e.status["/.well-known/pad-app.json"] = http.StatusFound }, 502, "manifest_fetch_failed"},
		"artifact missing":   {func(t *testing.T, e *appsEnv) { delete(e.files, "/pack/ship.md") }, 502, "artifact_fetch_failed"},
		"slug taken by a human collection": {func(t *testing.T, e *appsEnv) {
			if _, err := e.srv.store.CreateCollection(e.wsID, models.CollectionCreate{Name: "Portal tickets", Slug: "portal-tickets", Schema: `{"fields":[]}`}); err != nil {
				t.Fatal(err)
			}
		}, 409, "collection_conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newAppsEnv(t)
			c.mutate(t, e)
			rr := e.preview(t)
			if rr.Code != c.status || !strings.Contains(rr.Body.String(), c.code) {
				t.Fatalf("got %d %s, want %d %s", rr.Code, rr.Body.String(), c.status, c.code)
			}
			if n := e.pendingRows(t); n != 0 {
				t.Fatalf("a refused preview left %d pending record(s)", n)
			}
		})
	}
}

func TestAppInstallPreview_Gates(t *testing.T) {
	e := newAppsEnv(t)
	// Apps off: the routes answer 404.
	if rr := doRequestWithCookie(e.srv, "PUT", "/api/v1/admin/apps", map[string]any{"enabled": false}, e.token); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	if rr := e.preview(t); rr.Code != http.StatusNotFound {
		t.Fatalf("apps off: %d", rr.Code)
	}
	if rr := doRequestWithCookie(e.srv, "PUT", "/api/v1/admin/apps", map[string]any{"enabled": true}, e.token); rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	// A bad base URL is refused before any reservation.
	rr := doRequestWithCookie(e.srv, "POST", "/api/v1/workspaces/"+e.ws+"/apps/install/preview", map[string]any{"base_url": "http://x.example"}, e.token)
	if rr.Code != http.StatusBadRequest || e.pendingRows(t) != 0 {
		t.Fatalf("http base url: %d", rr.Code)
	}
	// The per-owner cap.
	for i := 0; i < 3; i++ {
		if rr := e.preview(t); rr.Code != http.StatusOK {
			t.Fatalf("preview %d: %d %s", i, rr.Code, rr.Body.String())
		}
	}
	if rr := e.preview(t); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth pending install: %d %s", rr.Code, rr.Body.String())
	}
}

func TestAppsAdminSettings(t *testing.T) {
	e := newAppsEnv(t)
	rr := doRequestWithCookie(e.srv, "PUT", "/api/v1/admin/apps", map[string]any{
		"private_origins": []map[string]any{{"origin": "https://x.example", "fetch": true}},
	}, e.token)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("an entry without pinned addresses was accepted: %d", rr.Code)
	}
	rr = doRequestWithCookie(e.srv, "GET", "/api/v1/admin/apps", nil, e.token)
	var s appsSettingsResponse
	parseJSON(t, rr, &s)
	if !s.Enabled || !s.Available || len(s.PrivateOrigins) != 1 {
		t.Fatalf("settings: %+v", s)
	}
	// Without an https issuer apps are unavailable even when enabled.
	e.srv.SetMCPConfig(config.MCPEndpoints{Origin: "http://pad.test.example", OriginVar: "PAD_URL",
		ResourceURL: "http://pad.test.example/mcp", AuthServerURL: "http://pad.test.example"}, nil)
	if e.srv.appsAvailable() {
		t.Fatal("apps available over an http issuer")
	}
}

// The preview's normalized item is what the import would STORE: the create's
// own defaults and validation run on it (codex round 1). A destination field
// the artifact cannot satisfy refuses the preview; a schema default appears
// in the previewed fields and so in the digest.
func TestAppInstallPreview_RunsTheCreateFieldPipeline(t *testing.T) {
	addField := func(t *testing.T, e *appsEnv, field map[string]any) {
		t.Helper()
		coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "playbooks")
		if err != nil || coll == nil {
			t.Fatalf("playbooks collection: %v", err)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
			t.Fatal(err)
		}
		schema["fields"] = append(schema["fields"].([]any), field)
		b, _ := json.Marshal(schema)
		sch := string(b)
		if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("required field refuses", func(t *testing.T) {
		e := newAppsEnv(t)
		addField(t, e, map[string]any{"key": "owner_team", "label": "Owner team", "type": "text", "required": true})
		rr := e.preview(t)
		if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "invalid_artifact") {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
		if n := e.pendingRows(t); n != 0 {
			t.Fatalf("%d pending left", n)
		}
	})
	t.Run("default is previewed", func(t *testing.T) {
		e := newAppsEnv(t)
		addField(t, e, map[string]any{"key": "team", "label": "Team", "type": "text", "default": "core"})
		rr := e.preview(t)
		if rr.Code != http.StatusOK {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
		var p appPreview
		parseJSON(t, rr, &p)
		if got := p.Artifacts[0].Normalized.Fields["team"]; got != "core" {
			t.Fatalf("the schema default is not in the previewed item: %v", got)
		}
	})
}

// An abandoned preview's staged bytes are deleted by the background sweep,
// not only when a later install happens to sweep (codex round 1).
func TestAppPendingSweep_DeletesExpiredStagedBytes(t *testing.T) {
	e := newAppsEnv(t)
	rr := e.preview(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := e.srv.store.DB().Exec(`UPDATE app_install_pending SET expires_at = '2000-01-01T00:00:00Z'`); err != nil {
		t.Fatal(err)
	}
	e.srv.runAppPendingSweep()
	var records, blobs int
	_ = e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM app_install_pending`).Scan(&records)
	_ = e.srv.store.DB().QueryRow(`SELECT COUNT(*) FROM app_install_pending_blobs`).Scan(&blobs)
	if records != 0 || blobs != 0 {
		t.Fatalf("after the sweep: %d records, %d blobs", records, blobs)
	}
	// The loop starts and stops cleanly.
	e.srv.StartAppPendingSweep()
	e.srv.StartAppPendingSweep()
	e.srv.stopAppPendingSweep()
	e.srv.stopAppPendingSweep()
}

// Two artifacts of one pack asking for the same invocation_slug are
// allocated distinct slugs in the preview, as their sequential import would
// be (codex round 2).
func TestAppInstallPreview_PackArtifactsDeCollideWithEachOther(t *testing.T) {
	e := newAppsEnv(t)
	m := e.manifest(t)
	second := []byte(strings.Replace(string(e.files["/pack/ship.md"]), "title: Ship a change", "title: Ship again", 1))
	e.files["/pack/ship2.md"] = second
	pack := m["companion_pack"].(map[string]any)
	pack["artifacts"] = append(pack["artifacts"].([]any), map[string]any{"key": "ship2", "url": e.origin() + "/pack/ship2.md", "sha256": appSHA(second)})
	e.publish(t, m)
	rr := e.preview(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	var p appPreview
	parseJSON(t, rr, &p)
	a, b := p.Artifacts[0].Normalized.Fields["invocation_slug"], p.Artifacts[1].Normalized.Fields["invocation_slug"]
	if a == b {
		t.Fatalf("both artifacts previewed invocation_slug %v", a)
	}
	if a != "ship" || b != "ship-2" {
		t.Fatalf("slugs %v, %v; want ship, ship-2 (the sequential import's allocation)", a, b)
	}
}

// A schema DEFAULT counts as a claim: an artifact without a slug, stored with
// the default, makes a later artifact asking for that slug de-collide, as
// the sequential import does (codex round 3).
func TestAppInstallPreview_DefaultSlugIsClaimed(t *testing.T) {
	e := newAppsEnv(t)
	coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "playbooks")
	if err != nil || coll == nil {
		t.Fatalf("playbooks collection: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatal(err)
	}
	for _, f := range schema["fields"].([]any) {
		if fm := f.(map[string]any); fm["key"] == "invocation_slug" {
			fm["default"] = "ship"
		}
	}
	b, _ := json.Marshal(schema)
	sch := string(b)
	if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
		t.Fatal(err)
	}
	m := e.manifest(t)
	orig := string(e.files["/pack/ship.md"])
	noSlug := []byte(strings.Replace(strings.Replace(orig, "invocation_slug: ship\n", "", 1), "title: Ship a change", "title: Defaulted", 1))
	e.files["/pack/a.md"] = noSlug
	e.files["/pack/b.md"] = []byte(strings.Replace(orig, "title: Ship a change", "title: Explicit", 1))
	pack := m["companion_pack"].(map[string]any)
	pack["artifacts"] = []any{
		map[string]any{"key": "a", "url": e.origin() + "/pack/a.md", "sha256": appSHA(e.files["/pack/a.md"])},
		map[string]any{"key": "b", "url": e.origin() + "/pack/b.md", "sha256": appSHA(e.files["/pack/b.md"])},
	}
	e.publish(t, m)
	rr := e.preview(t)
	if rr.Code != http.StatusOK {
		t.Fatalf("preview: %d %s", rr.Code, rr.Body.String())
	}
	var p appPreview
	parseJSON(t, rr, &p)
	if a, b := p.Artifacts[0].Normalized.Fields["invocation_slug"], p.Artifacts[1].Normalized.Fields["invocation_slug"]; a != "ship" || b != "ship-2" {
		t.Fatalf("slugs %v, %v; want ship (the default), ship-2", a, b)
	}
}

// Any unique-scoped destination field, not only invocation_slug: two pack
// artifacts storing the same unique value (here through a default) refuse
// the preview, as the second sequential import would 409 (codex round 4).
func TestAppInstallPreview_PackUniqueValuesConflict(t *testing.T) {
	e := newAppsEnv(t)
	coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "playbooks")
	if err != nil || coll == nil {
		t.Fatalf("playbooks collection: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatal(err)
	}
	codeField := map[string]any{"key": "code", "label": "Code", "type": "text", "unique_scope": "workspace_collection", "default": "shared"}
	// Declared twice: a single artifact must still not conflict with itself
	// (codex round 5).
	schema["fields"] = append(schema["fields"].([]any), codeField, codeField)
	b, _ := json.Marshal(schema)
	sch := string(b)
	if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
		t.Fatal(err)
	}
	if rr := e.preview(t); rr.Code != http.StatusOK {
		t.Fatalf("a single artifact conflicted with itself: %d %s", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(e.srv, "DELETE", "/api/v1/workspaces/"+e.ws+"/apps/install/pending/"+pendingIDOf(t, e), nil, e.token); rr.Code != http.StatusNoContent {
		t.Fatalf("discard: %d", rr.Code)
	}
	m := e.manifest(t)
	second := []byte(strings.Replace(strings.Replace(string(e.files["/pack/ship.md"]), "title: Ship a change", "title: Other", 1), "invocation_slug: ship", "invocation_slug: other", 1))
	e.files["/pack/other.md"] = second
	pack := m["companion_pack"].(map[string]any)
	pack["artifacts"] = append(pack["artifacts"].([]any), map[string]any{"key": "other", "url": e.origin() + "/pack/other.md", "sha256": appSHA(second)})
	e.publish(t, m)
	rr := e.preview(t)
	if rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "code") {
		t.Fatalf("got %d %s; want 422 naming the unique field", rr.Code, rr.Body.String())
	}
	if n := e.pendingRows(t); n != 0 {
		t.Fatalf("%d pending left", n)
	}
}

func pendingIDOf(t *testing.T, e *appsEnv) string {
	t.Helper()
	var id string
	if err := e.srv.store.DB().QueryRow(`SELECT id FROM app_install_pending ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// A unique_scope the import does NOT enforce is not a pack conflict either
// (codex round 6): two artifacts sharing that default both import, so both
// preview.
func TestAppInstallPreview_UnenforcedUniqueScopeIsNotAConflict(t *testing.T) {
	e := newAppsEnv(t)
	coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "playbooks")
	if err != nil || coll == nil {
		t.Fatalf("playbooks collection: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatal(err)
	}
	schema["fields"] = append(schema["fields"].([]any), map[string]any{"key": "code", "label": "Code", "type": "text", "unique_scope": "collection", "default": "shared"})
	b, _ := json.Marshal(schema)
	sch := string(b)
	if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
		t.Fatal(err)
	}
	m := e.manifest(t)
	second := []byte(strings.Replace(strings.Replace(string(e.files["/pack/ship.md"]), "title: Ship a change", "title: Other", 1), "invocation_slug: ship", "invocation_slug: other", 1))
	e.files["/pack/other.md"] = second
	pack := m["companion_pack"].(map[string]any)
	pack["artifacts"] = append(pack["artifacts"].([]any), map[string]any{"key": "other", "url": e.origin() + "/pack/other.md", "sha256": appSHA(second)})
	e.publish(t, m)
	if rr := e.preview(t); rr.Code != http.StatusOK {
		t.Fatalf("an unenforced scope refused the preview: %d %s", rr.Code, rr.Body.String())
	}
}

// invocation_slug is unique per collection at the DATABASE, whatever the
// schema declares (codex round 7): a defaulted slug that a stored item
// already holds, and a non-text slug, both refuse the preview.
func TestAppInstallPreview_InvocationSlugIndexIsMirrored(t *testing.T) {
	setDefault := func(t *testing.T, e *appsEnv, def any) string {
		t.Helper()
		coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "conventions")
		if err != nil || coll == nil {
			t.Fatalf("conventions collection: %v", err)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
			t.Fatal(err)
		}
		typ := "text"
		if _, isNum := def.(int); isNum {
			typ = "number"
		}
		schema["fields"] = append(schema["fields"].([]any), map[string]any{"key": "invocation_slug", "label": "Slug", "type": typ, "default": def})
		b, _ := json.Marshal(schema)
		sch := string(b)
		if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
			t.Fatal(err)
		}
		return coll.ID
	}
	useConvention := func(t *testing.T, e *appsEnv) {
		t.Helper()
		cv, err := os.ReadFile(filepath.Join("..", "artifact", "testdata", "convention.golden.md"))
		if err != nil {
			t.Fatal(err)
		}
		e.files["/pack/conv.md"] = cv
		m := e.manifest(t)
		m["companion_pack"].(map[string]any)["artifacts"] = []any{map[string]any{"key": "conv", "url": e.origin() + "/pack/conv.md", "sha256": appSHA(cv)}}
		e.publish(t, m)
	}

	t.Run("a stored item holds the defaulted slug", func(t *testing.T) {
		e := newAppsEnv(t)
		collID := setDefault(t, e, "shared")
		if _, err := e.srv.store.CreateItem(e.wsID, collID, models.ItemCreate{Title: "Holder", Fields: `{"invocation_slug":"shared","status":"draft"}`}); err != nil {
			t.Fatal(err)
		}
		useConvention(t, e)
		if rr := e.preview(t); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "shared") {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("a non-text slug", func(t *testing.T) {
		e := newAppsEnv(t)
		setDefault(t, e, 42)
		useConvention(t, e)
		if rr := e.preview(t); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "non-text") {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
	})
}

// The stored-slug check uses the index's own expression: a stored ARRAY
// "[]" collides with a defaulted text "[]" there, so the preview refuses
// (codex round 8).
func TestAppInstallPreview_InvocationSlugIndexExpression(t *testing.T) {
	e := newAppsEnv(t)
	coll, err := e.srv.store.GetCollectionBySlug(e.wsID, "conventions")
	if err != nil || coll == nil {
		t.Fatalf("conventions collection: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		t.Fatal(err)
	}
	schema["fields"] = append(schema["fields"].([]any), map[string]any{"key": "invocation_slug", "label": "Slug", "type": "text", "default": "[]"})
	b, _ := json.Marshal(schema)
	sch := string(b)
	if _, err := e.srv.store.UpdateCollection(coll.ID, models.CollectionUpdate{Schema: &sch}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.srv.store.DB().Exec(e.srv.store.D().Rebind(`INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, created_at, updated_at, item_number)
		VALUES (?, ?, ?, 'Array holder', 'array-holder', '', '{"invocation_slug":[],"status":"draft"}', '[]', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 99999)`),
		"arr-"+e.wsID[:8], e.wsID, coll.ID); err != nil {
		t.Fatal(err)
	}
	cv, err := os.ReadFile(filepath.Join("..", "artifact", "testdata", "convention.golden.md"))
	if err != nil {
		t.Fatal(err)
	}
	e.files["/pack/conv.md"] = cv
	m := e.manifest(t)
	m["companion_pack"].(map[string]any)["artifacts"] = []any{map[string]any{"key": "conv", "url": e.origin() + "/pack/conv.md", "sha256": appSHA(cv)}}
	e.publish(t, m)
	if rr := e.preview(t); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d %s", rr.Code, rr.Body.String())
	}
}

// In-pack claims collide wherever the import's lookup (JSONFieldEquals)
// would match: boolean true vs "true", number 1000 vs "1e3" (codex rounds
// 9-10).
func TestClaimForms(t *testing.T) {
	collide := func(a, b any) bool {
		seen := map[string]bool{}
		for _, f := range claimForms(a) {
			seen[f] = true
		}
		for _, f := range claimForms(b) {
			if seen[f] {
				return true
			}
		}
		return false
	}
	for _, c := range []struct {
		a, b any
		want bool
	}{
		{true, "true", true},
		{float64(1000), "1e3", true},
		{float64(1000), "1000", true},
		{float64(9210000000000000000), "9210000000000000000", true},
		{float64(1000000000000000100), "1000000000000000100", true},
		// codex round 12: exponent spelling past numericArg's integer bound,
		// its negative, and a decimal spelling that SQLite rounds to the same
		// double.
		{float64(9210000000000000000), "9.21e18", true},
		{float64(-9210000000000000000), "-9.21e18", true},
		{float64(1000000000000000100), "1000000000000000100.0", true},
		{float64(0.5), "0.50", true},
		{float64(2), "3", false},
		{"ship", "ship", true},
		{"ship", "other", false},
		{float64(1), true, false},
		{nil, "", false},
		{"", "", false},
	} {
		if got := collide(c.a, c.b); got != c.want {
			t.Errorf("collide(%#v, %#v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// App-authored artifact content may not carry pad-attachment: tokens or
// [[workspace::REF]] links; ordinary [[Title]] links are fine (lead ruling,
// U8b Q3).
func TestAppInstallPreview_AppContentReferenceRules(t *testing.T) {
	for name, c := range map[string]struct {
		edit string
		ok   bool
	}{
		"attachment token in the body": {"![x](PAD-Attachment:abc)", false},
		"workspace-qualified link":     {"see [[other-ws::TASK-1]]", false},
		"ordinary title link":          {"see [[Some Title]]", true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newAppsEnv(t)
			m := e.manifest(t)
			body := []byte(string(e.files["/pack/ship.md"]) + "\n" + c.edit + "\n")
			e.files["/pack/ship.md"] = body
			m["companion_pack"].(map[string]any)["artifacts"] = []any{map[string]any{"key": "ship", "url": e.origin() + "/pack/ship.md", "sha256": appSHA(body)}}
			e.publish(t, m)
			rr := e.preview(t)
			if c.ok && rr.Code != http.StatusOK {
				t.Fatalf("got %d %s", rr.Code, rr.Body.String())
			}
			if !c.ok && (rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "not accepted in app content")) {
				t.Fatalf("got %d %s", rr.Code, rr.Body.String())
			}
		})
	}
	t.Run("attachment token in a field", func(t *testing.T) {
		e := newAppsEnv(t)
		m := e.manifest(t)
		orig := string(e.files["/pack/ship.md"])
		if !strings.Contains(orig, "description: the item to ship") {
			t.Fatal("fixture changed: no arguments description to carry the token")
		}
		body := []byte(strings.Replace(orig, "description: the item to ship", "description: \"see pad-attachment:abc\"", 1))
		e.files["/pack/ship.md"] = body
		m["companion_pack"].(map[string]any)["artifacts"] = []any{map[string]any{"key": "ship", "url": e.origin() + "/pack/ship.md", "sha256": appSHA(body)}}
		e.publish(t, m)
		if rr := e.preview(t); rr.Code != http.StatusUnprocessableEntity || !strings.Contains(rr.Body.String(), "not accepted in app content") {
			t.Fatalf("got %d %s", rr.Code, rr.Body.String())
		}
	})
}

// Lead review of U8a: the fields check fails closed. A field set that cannot
// be serialized is refused, never passed unchecked.
func TestRefuseAppAuthoredReferences_FailsClosedOnUnserializableFields(t *testing.T) {
	err := refuseAppAuthoredReferences("plain content", map[string]any{"bad": make(chan int)})
	if err == nil {
		t.Fatal("an unserializable field set passed the reference check")
	}
	if err := refuseAppAuthoredReferences("plain content", map[string]any{"ok": "value"}); err != nil {
		t.Fatalf("control: a clean field set was refused: %v", err)
	}
}
