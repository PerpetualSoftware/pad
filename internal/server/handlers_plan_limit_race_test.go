package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/artifact"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2808: a plan limit is a count taken before the write. When a competing
// row lands between the count and the handler's own insert, the cap is
// exceeded. planLimitAdmittedHook runs right after a check admits a request,
// and the "competing" legs insert a second row there, as a concurrent request
// would. Each such leg is paired with a control that inserts nothing
// (CONVE-12): without it, a fix that refused everything would pass.
//
// These drive the real router in cloud mode (limits are no-ops otherwise).
// They run on SQLite because testServer is SQLite-backed.

// planLimitEnv is a cloud-mode server with one PAT-authenticated user on the
// free plan, who owns one workspace seeded with the startup collections.
type planLimitEnv struct {
	*consentEnv
	home *models.Workspace
}

func newPlanLimitEnv(t *testing.T) *planLimitEnv {
	t.Helper()
	e := newConsentEnv(t, true)
	e.srv.cloudMode = true
	if err := e.srv.store.SetUserPlan(e.user.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	all, err := e.srv.store.ListWorkspaces()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListWorkspaces = %d, %v; want exactly the fixture's home", len(all), err)
	}
	home := all[0]
	if err := e.srv.store.SeedCollectionsFromTemplate(home.ID, "startup"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	return &planLimitEnv{consentEnv: e, home: &home}
}

// capAtOneMore sets the user's override for feature to current+1, so exactly
// one more row is admitted.
func (e *planLimitEnv) capAtOneMore(t *testing.T, feature string, current int) int {
	t.Helper()
	limit := current + 1
	if err := e.srv.store.SetUserPlanOverrides(e.user.ID, fmt.Sprintf(`{%q:%d}`, feature, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	return limit
}

func (e *planLimitEnv) ownedWorkspaces(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(e.srv.store.D().Rebind(
		`SELECT COUNT(*) FROM workspaces WHERE owner_id = ?`), e.user.ID).Scan(&n); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	return n
}

// --- U1: workspaces (create door) ---

func TestPlanLimitRace_Workspaces_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	limit := e.capAtOneMore(t, "workspaces", e.ownedWorkspaces(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "workspaces" || scope != e.user.ID {
			return
		}
		ran = true
		if _, err := e.srv.store.CreateWorkspace(models.WorkspaceCreate{
			Name: "Competitor", OwnerID: e.user.ID,
		}); err != nil {
			t.Errorf("competing CreateWorkspace: %v", err)
		}
	}

	rr := e.createWorkspace("Racer", "")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for workspaces; the leg measured nothing")
	}
	e.mustBeRefusedAtCap(t, rr, limit, e.ownedWorkspaces(t), "workspaces")
	e.mustNotExist(t, "Racer")
}

// The JSON import door reaches the same pre-check (beginWorkspaceMint) and
// then ImportWorkspace, whose check runs last inside the import transaction.
func TestPlanLimitRace_WorkspacesImport_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	body := e.exportBody(t) // mints a workspace, so the cap is taken after it
	limit := e.capAtOneMore(t, "workspaces", e.ownedWorkspaces(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "workspaces" || scope != e.user.ID {
			return
		}
		ran = true
		if _, err := e.srv.store.CreateWorkspace(models.WorkspaceCreate{
			Name: "Competitor", OwnerID: e.user.ID,
		}); err != nil {
			t.Errorf("competing CreateWorkspace: %v", err)
		}
	}

	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-Racer", "application/json", body, "")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for the import; the leg measured nothing")
	}
	e.mustBeRefusedAtCap(t, rr, limit, e.ownedWorkspaces(t), "workspaces")
	e.mustNotExist(t, "Imported-Racer")
}

func TestPlanLimitRace_WorkspacesImport_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	body := e.exportBody(t)
	limit := e.capAtOneMore(t, "workspaces", e.ownedWorkspaces(t))

	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-Alone", "application/json", body, "")
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.ownedWorkspaces(t); got != limit {
		t.Errorf("user owns %d workspaces, want exactly the cap %d", got, limit)
	}
}

// mustBeRefusedAtCap asserts the refusal a limited mint owes when its own
// check sees the cap reached: the 403 plan_limit_exceeded envelope, and a
// count that did not pass the cap.
func (e *planLimitEnv) mustBeRefusedAtCap(t *testing.T, rr *httptest.ResponseRecorder, limit, got int, feature string) {
	t.Helper()
	if got > limit {
		t.Errorf("%s count is %d under a cap of %d (answered %d) — the competing insert "+
			"landed in the window and both were admitted", feature, got, limit, rr.Code)
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "plan_limit_exceeded" {
		t.Errorf("error code = %q, want plan_limit_exceeded (body=%s)", code, rr.Body.String())
	}
}

func TestPlanLimitRace_Workspaces_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	limit := e.capAtOneMore(t, "workspaces", e.ownedWorkspaces(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, _ string) {
		if feature == "workspaces" {
			ran = true
		}
	}

	rr := e.createWorkspace("Alone", "")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for workspaces; the leg measured nothing")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.ownedWorkspaces(t); got != limit {
		t.Errorf("user owns %d workspaces, want exactly the cap %d", got, limit)
	}
}

// --- U2: api_tokens (both token doors) ---

// newTokenLimitEnv is setupTokenAuthEnv (a session-authenticated owner holding
// two PATs) in cloud mode on the free plan, with the api_tokens cap at one
// more than the tokens already owned.
func newTokenLimitEnv(t *testing.T) (*tokenAuthEnv, int) {
	t.Helper()
	env := setupTokenAuthEnv(t)
	env.srv.cloudMode = true
	if err := env.srv.store.SetUserPlan(env.user.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	limit := ownedTokens(t, env) + 1
	if err := env.srv.store.SetUserPlanOverrides(env.user.ID, fmt.Sprintf(`{"api_tokens":%d}`, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	return env, limit
}

func ownedTokens(t *testing.T, env *tokenAuthEnv) int {
	t.Helper()
	var n int
	if err := env.srv.store.DB().QueryRow(env.srv.store.D().Rebind(
		`SELECT COUNT(*) FROM api_tokens WHERE user_id = ?`), env.user.ID).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

func TestPlanLimitRace_Tokens_CompetingCreateInWindow_Refused(t *testing.T) {
	env, limit := newTokenLimitEnv(t)

	ran := false
	env.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "api_tokens" || scope != env.user.ID {
			return
		}
		ran = true
		if _, err := env.srv.store.CreateAPIToken(env.user.ID, models.APITokenCreate{Name: "competitor"}, 0, 0); err != nil {
			t.Errorf("competing CreateAPIToken: %v", err)
		}
	}

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/auth/tokens", env.sessTok, map[string]any{"name": "racer"})
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for api_tokens; the leg measured nothing")
	}
	if got := ownedTokens(t, env); got > limit {
		t.Errorf("user owns %d tokens under a cap of %d (answered %d)", got, limit, rr.Code)
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "plan_limit_exceeded" {
		t.Errorf("error code = %q, want plan_limit_exceeded", code)
	}
}

func TestPlanLimitRace_Tokens_NoCompetitor_Admitted(t *testing.T) {
	env, limit := newTokenLimitEnv(t)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/auth/tokens", env.sessTok, map[string]any{"name": "alone"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := ownedTokens(t, env); got != limit {
		t.Errorf("user owns %d tokens, want exactly the cap %d", got, limit)
	}
}

// The workspace token door had NO plan-limit check before BUG-2808, although
// the token it mints is user-owned and counts against the same cap. At the cap
// it is now refused with the same envelope, and the message says the cap is
// per user, because the request names a workspace.
func TestPlanLimit_WorkspaceTokenDoor_RefusedAtUserCap(t *testing.T) {
	env, _ := newTokenLimitEnv(t)
	// Fill the one remaining slot through the user door first.
	if rr := doRequestWithBearer(env.srv, "POST", "/api/v1/auth/tokens", env.sessTok,
		map[string]any{"name": "fills-the-cap"}); rr.Code != http.StatusCreated {
		t.Fatalf("filling the cap: status = %d (body=%s)", rr.Code, rr.Body.String())
	}
	before := ownedTokens(t, env)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/workspaces/"+env.ws.Slug+"/tokens", env.sessTok,
		map[string]any{"name": "over-the-cap"})
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "plan_limit_exceeded" {
		t.Errorf("error code = %q, want plan_limit_exceeded", code)
	}
	if !strings.Contains(rr.Body.String(), "every API token you own, across all workspaces") {
		t.Errorf("refusal does not say the cap is per user: %s", rr.Body.String())
	}
	if got := ownedTokens(t, env); got != before {
		t.Errorf("token count moved from %d to %d on a refused mint", before, got)
	}
}

// Control for the leg above: under the cap the workspace door still mints.
func TestPlanLimit_WorkspaceTokenDoor_AdmittedUnderUserCap(t *testing.T) {
	env, limit := newTokenLimitEnv(t)

	rr := doRequestWithBearer(env.srv, "POST", "/api/v1/workspaces/"+env.ws.Slug+"/tokens", env.sessTok,
		map[string]any{"name": "under-the-cap"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := ownedTokens(t, env); got != limit {
		t.Errorf("user owns %d tokens, want exactly the cap %d", got, limit)
	}
}

// --- U1 on the BUNDLE import door (gzip body, same route) ---

// bundleLimitRace sends a one-entry bundle import from a session-authenticated
// free-plan user whose workspaces cap is one above what they own. compete
// controls whether planLimitAdmittedHook inserts a competing workspace.
func bundleLimitRace(t *testing.T, compete bool) (*httptest.ResponseRecorder, int, int, bool, *Server) {
	t.Helper()
	srv, _ := testServerWithAttachments(t)
	srv.cloudMode = true
	u := mintTestUser(t, srv, "bundle-limit@example.com")
	tok := loginUser(t, srv, "bundle-limit@example.com", "correct-horse-battery-staple")
	if err := srv.store.SetUserPlan(u.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	src, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Bundle Source", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(source): %v", err)
	}
	export, err := srv.store.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	payload, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	owned := func() int {
		var n int
		if err := srv.store.DB().QueryRow(srv.store.D().Rebind(
			`SELECT COUNT(*) FROM workspaces WHERE owner_id = ?`), u.ID).Scan(&n); err != nil {
			t.Fatalf("count workspaces: %v", err)
		}
		return n
	}
	limit := owned() + 1
	if err := srv.store.SetUserPlanOverrides(u.ID, fmt.Sprintf(`{"workspaces":%d}`, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}

	ran := false
	srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "workspaces" || scope != u.ID {
			return
		}
		ran = true
		if !compete {
			return
		}
		if _, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Competitor", OwnerID: u.ID}); err != nil {
			t.Errorf("competing CreateWorkspace: %v", err)
		}
	}

	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{Name: "pad-export.json", Mode: 0o644, Size: int64(len(payload))}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatalf("write export: %v", err)
	}
	tw.Close()
	gzw.Close()

	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=bundle-racer", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/gzip")
	req.RemoteAddr = "192.0.2.1:1234"
	req.AddCookie(&http.Cookie{Name: "pad_session", Value: tok})
	const testCSRF = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.AddCookie(&http.Cookie{Name: "pad_csrf", Value: testCSRF})
	req.Header.Set("X-CSRF-Token", testCSRF)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr, owned(), limit, ran, srv
}

func TestPlanLimitRace_WorkspacesBundle_CompetingCreateInWindow_Refused(t *testing.T) {
	rr, got, limit, ran, srv := bundleLimitRace(t, true)
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for the bundle import; the leg measured nothing")
	}
	if got > limit {
		t.Errorf("user owns %d workspaces under a cap of %d (answered %d)", got, limit, rr.Code)
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "plan_limit_exceeded" {
		t.Errorf("error code = %q, want plan_limit_exceeded", code)
	}
	if ws, err := srv.store.GetWorkspaceBySlug("bundle-racer"); err != nil || ws != nil {
		t.Errorf("refused bundle import left a workspace behind: %+v, %v", ws, err)
	}
}

func TestPlanLimitRace_WorkspacesBundle_NoCompetitor_Admitted(t *testing.T) {
	rr, got, limit, ran, _ := bundleLimitRace(t, false)
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for the bundle import; the leg measured nothing")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got != limit {
		t.Errorf("user owns %d workspaces, want exactly the cap %d", got, limit)
	}
}

// --- PR B: workspace-scoped limits (items, members, webhooks) ---
//
// Every door below runs enforcePlanLimit and then an insert that WithPlanLimit
// makes authoritative. workspaceRace drives one door: the competing leg
// inserts one counted row in the window between the pre-check and the insert,
// and must be refused at the cap. The control inserts nothing, and must be
// admitted at exactly the cap.

type workspaceRace struct {
	feature string
	// count reads the feature's counted rows in the home workspace.
	count func(t *testing.T) int
	// compete inserts one counted row directly through the store, unlimited,
	// as a concurrent request that had already passed its own check would.
	compete func(t *testing.T)
	// door makes the request under test.
	door func(t *testing.T) *httptest.ResponseRecorder
	// admitted is the status the door answers on success.
	admitted int
}

func (e *planLimitEnv) runWorkspaceRace(t *testing.T, r workspaceRace, compete bool) {
	t.Helper()
	limit := e.capAtOneMore(t, r.feature, r.count(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != r.feature || scope != e.home.ID {
			return
		}
		ran = true
		if compete {
			r.compete(t)
		}
	}

	rr := r.door(t)
	if !ran {
		t.Fatalf("planLimitAdmittedHook never ran for %s; the leg measured nothing", r.feature)
	}
	got := r.count(t)
	if compete {
		e.mustBeRefusedAtCap(t, rr, limit, got, r.feature)
		return
	}
	if rr.Code != r.admitted {
		t.Fatalf("status = %d, want %d (body=%s)", rr.Code, r.admitted, rr.Body.String())
	}
	if got != limit {
		t.Errorf("%s count is %d, want exactly the cap %d", r.feature, got, limit)
	}
}

func (e *planLimitEnv) countIn(t *testing.T, query string) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(e.srv.store.D().Rebind(query), e.home.ID).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", query, err)
	}
	return n
}

func (e *planLimitEnv) collectionID(t *testing.T, slug string) string {
	t.Helper()
	coll, err := e.srv.store.GetCollectionBySlug(e.home.ID, slug)
	if err != nil || coll == nil {
		t.Fatalf("GetCollectionBySlug(%s) = %v, %v", slug, coll, err)
	}
	return coll.ID
}

func (e *planLimitEnv) newUser(t *testing.T, email string) *models.User {
	t.Helper()
	u, err := e.srv.store.CreateUser(models.UserCreate{Email: email, Name: email, Password: "pw-limit-12345"})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	return u
}

func (e *planLimitEnv) itemRace(t *testing.T, door func(t *testing.T) *httptest.ResponseRecorder) workspaceRace {
	tasks := e.collectionID(t, "tasks")
	return workspaceRace{
		feature: "items_per_workspace",
		count: func(t *testing.T) int {
			return e.countIn(t, `SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`)
		},
		compete: func(t *testing.T) {
			if _, err := e.srv.store.CreateItem(e.home.ID, tasks, models.ItemCreate{Title: "Competitor"}); err != nil {
				t.Errorf("competing CreateItem: %v", err)
			}
		},
		door:     door,
		admitted: http.StatusCreated,
	}
}

// W1: the item create door.
func (e *planLimitEnv) itemCreateRace(t *testing.T) workspaceRace {
	return e.itemRace(t, func(t *testing.T) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]any{"title": "Racer"})
		return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/collections/tasks/items", "application/json", body, "")
	})
}

// W2: the artifact import door, which mints through the same createItemChecked.
func (e *planLimitEnv) itemImportRace(t *testing.T) workspaceRace {
	return e.itemRace(t, func(t *testing.T) *httptest.ResponseRecorder {
		data, err := artifact.Encode(artifact.Artifact{
			Kind:          artifact.KindConvention,
			FormatVersion: artifact.FormatVersion,
			Title:         "Imported racer",
			Fields:        map[string]any{"status": "active", "trigger": "always", "scope": "all", "priority": "must"},
			Body:          "An imported convention.\n",
		})
		if err != nil {
			t.Fatalf("artifact.Encode: %v", err)
		}
		return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/import-artifact", "text/markdown", data, "")
	})
}

// W3: the invite door's direct add of an existing user.
func (e *planLimitEnv) memberAddRace(t *testing.T) workspaceRace {
	invitee := e.newUser(t, "invitee@example.com")
	rival := e.newUser(t, "rival@example.com")
	return workspaceRace{
		feature: "members_per_workspace",
		count: func(t *testing.T) int {
			return e.countIn(t, `SELECT COUNT(*) FROM workspace_members WHERE workspace_id = ?`)
		},
		compete: func(t *testing.T) {
			if err := e.srv.store.AddWorkspaceMember(e.home.ID, rival.ID, "editor"); err != nil {
				t.Errorf("competing AddWorkspaceMember: %v", err)
			}
		},
		door: func(t *testing.T) *httptest.ResponseRecorder {
			body, _ := json.Marshal(map[string]any{"email": invitee.Email, "role": "editor"})
			return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/members/invite", "application/json", body, "")
		},
		admitted: http.StatusCreated,
	}
}

// W4: the webhook create door. Literal IPs keep ValidateWebhookURL off DNS.
func (e *planLimitEnv) webhookRace(t *testing.T) workspaceRace {
	return workspaceRace{
		feature: "webhooks",
		count: func(t *testing.T) int {
			return e.countIn(t, `SELECT COUNT(*) FROM webhooks WHERE workspace_id = ?`)
		},
		compete: func(t *testing.T) {
			if _, err := e.srv.store.CreateWebhook(e.home.ID, models.WebhookCreate{URL: "https://8.8.4.4/rival"}); err != nil {
				t.Errorf("competing CreateWebhook: %v", err)
			}
		},
		door: func(t *testing.T) *httptest.ResponseRecorder {
			body, _ := json.Marshal(map[string]any{"url": "https://8.8.8.8/hook"})
			return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/webhooks", "application/json", body, "")
		},
		admitted: http.StatusCreated,
	}
}

func TestPlanLimitRace_Items_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.itemCreateRace(t), true)
}

func TestPlanLimitRace_Items_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.itemCreateRace(t), false)
}

func TestPlanLimitRace_ItemsImport_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.itemImportRace(t), true)
}

func TestPlanLimitRace_ItemsImport_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.itemImportRace(t), false)
}

func TestPlanLimitRace_Members_CompetingAddInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.memberAddRace(t), true)
}

func TestPlanLimitRace_Members_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.memberAddRace(t), false)
}

func TestPlanLimitRace_Webhooks_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.webhookRace(t), true)
}

func TestPlanLimitRace_Webhooks_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	e.runWorkspaceRace(t, e.webhookRace(t), false)
}
