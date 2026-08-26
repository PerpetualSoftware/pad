package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Consent gate on workspace CREATION (IDEA-2756, Dave-ruled 2026-08-26).
//
// The OAuth consent screen's `may_create_workspaces` checkbox used to gate
// only maybeAutoAddCreatorConnection's allow-list insert: a connection whose
// user left the box unticked could still create workspaces — invisible to it
// when the connection carried an explicit allow-list, visible when it carried
// the all_current_workspaces wildcard, and unconsented either way. The ruling: the checkbox is a permission on whether the
// connected token may CREATE, so an unset flag refuses outright with a 403,
// mirroring handleAuditLog's consent refusal (BUG-2102).
//
// Two doors let an OAuth-bound caller mint a workspace, both guarded here by
// the shared requireWorkspaceCreationConsent helper. (Not every path to
// store.CreateWorkspace: autoCreateWorkspace mints one at signup, outside this
// gate and deliberately so — no OAuth connection is in context there.)
//
//   - POST /api/v1/workspaces        — handleCreateWorkspace
//   - POST /api/v1/workspaces/import — handleImportWorkspace, which reaches
//     CreateWorkspace via store.ImportWorkspace, and whose gzip Content-Type
//     branch dispatches to handleImportWorkspaceBundle from inside itself.
//
// Every refusal leg whose request COULD have created something asserts the
// wrong behaviour's observable consequence — that no workspace of that NAME
// exists afterwards — and not merely the status code (CONVE-12). A guard that
// 403s AFTER store.CreateWorkspace would pass a status-only assertion while
// leaving the workspace behind, which is the exact failure this endpoint's
// users would report.
//
// The two ORDERING legs are the deliberate exception, and adding the assertion
// there would be worse than omitting it: a malformed body and an empty name are
// rejected before creation under every guard placement, so "no such workspace
// exists" is true of broken and working code alike. Those legs discriminate on
// the STATUS instead — 400 if the gate is late, 403 if it is early — which is
// the only signal that separates the two placements. (Round 8 flagged the
// original blanket claim; the first fix added the vacuous assertions, which is
// the failure that claim was warning about.)
//
// By NAME rather than by slug: the name is what the request supplies verbatim,
// while the slug is derived differently on the two paths (see lookupByName).
//
// Everything drives the real router via srv.ServeHTTP rather than calling the
// handler directly, so the tests have an opinion about the ROUTE and not only
// about the function (CONVE-19).

// consentEnv is a server with one authenticated user, a PAT for it, and an
// OAuth connection row whose may_create_workspaces flag the test chooses.
type consentEnv struct {
	srv       *Server
	user      *models.User
	pat       string
	requestID string
}

func newConsentEnv(t *testing.T, mayCreate bool) *consentEnv {
	t.Helper()
	srv := testServer(t)

	user, err := srv.store.CreateUser(models.UserCreate{
		Email: "consent-test@example.com", Name: "Consent Tester", Password: "pw-consent-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// A PAT does not require a workspace (CreateAPIToken takes WorkspaceID as
	// optional), but binding one keeps this fixture close to a real CLI token
	// and gives the auth chain something ordinary to resolve. Seeded through
	// the store directly because the handler under test is the one that makes
	// workspaces. It is the token's home, never the workspace any test asserts
	// about.
	home, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Consent Home", OwnerID: user.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace(home): %v", err)
	}
	if err := srv.store.AddWorkspaceMember(home.ID, user.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "consent-test-pat", WorkspaceID: home.ID,
	}, 30, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	requestID := "req-consent-" + user.ID
	if err := srv.store.CreateOAuthConnection(store.OAuthConnection{
		RequestID:           requestID,
		UserID:              user.ID,
		Name:                "Consent Test App",
		MayCreateWorkspaces: mayCreate,
	}); err != nil {
		t.Fatalf("CreateOAuthConnection: %v", err)
	}

	return &consentEnv{srv: srv, user: user, pat: tok.Token, requestID: requestID}
}

// do issues a request with Bearer PAT auth. When requestID is non-empty the
// request context is decorated with an OAuth grant identity, which is what
// makes the caller look like an OAuth-bound MCP session.
//
// The wrapper sets the identity BEFORE srv.ServeHTTP, so it is in place before
// TokenAuth runs — and survives, because nothing on the /api/v1 chain writes
// that context key. Only MCPBearerAuth does, and it is mounted on /mcp alone.
// (The sibling helper this is modelled on, handlers_oauth_claim_test.go's
// doClaim, describes the same wrapper as decorating the context "AFTER
// TokenAuth runs"; that ordering claim is wrong for both, and the mechanism
// works for the reason given above instead.)
func (e *consentEnv) do(method, path, contentType string, body []byte, requestID string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != nil {
		req = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Authorization", "Bearer "+e.pat)
	req.RemoteAddr = "192.0.2.1:1234"

	rr := httptest.NewRecorder()
	if requestID == "" {
		e.srv.ServeHTTP(rr, req)
		return rr
	}
	wrap := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(WithMCPTokenIdentity(r.Context(), "oauth", requestID))
		e.srv.ServeHTTP(w, r)
	})
	wrap.ServeHTTP(rr, req)
	return rr
}

func (e *consentEnv) createWorkspace(name, requestID string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"name": name})
	return e.do("POST", "/api/v1/workspaces", "application/json", body, requestID)
}

// lookupByName finds a workspace by its NAME rather than its slug. Name is
// what the request supplies verbatim; slug is derived, and the derivation
// differs between the create path (normalizeWorkspaceInput) and the import
// path, which passes the ?name= override through as the SLUG — and
// CreateWorkspace slugifies only when the supplied slug is empty, so an
// imported workspace keeps that value verbatim. Asserting on the name keeps
// these tests about the guard instead of about slug derivation.
func (e *consentEnv) lookupByName(t *testing.T, name string) *models.Workspace {
	t.Helper()
	all, err := e.srv.store.ListWorkspaces()
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i]
		}
	}
	return nil
}

// mustNotExist is the counterfactual half of every refusal leg: the thing a
// 403-after-the-write would have left behind.
func (e *consentEnv) mustNotExist(t *testing.T, name string) {
	t.Helper()
	if ws := e.lookupByName(t, name); ws != nil {
		t.Errorf("workspace %q exists after a refused create — the guard ran too late "+
			"(id=%s slug=%s); the refusal must happen before store.CreateWorkspace",
			name, ws.ID, ws.Slug)
	}
}

func (e *consentEnv) mustExist(t *testing.T, name string) *models.Workspace {
	t.Helper()
	ws := e.lookupByName(t, name)
	if ws == nil {
		t.Fatalf("workspace %q does not exist, want created", name)
	}
	return ws
}

func errorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("parse error envelope: %v (body=%s)", err, rr.Body.String())
	}
	return env.Error.Code
}

// --- POST /api/v1/workspaces ---

func TestCreateWorkspace_OAuthConnectionWithoutCreateConsent_403(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.createWorkspace("Refused WS", e.requestID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "forbidden" {
		t.Errorf("error code = %q, want %q", code, "forbidden")
	}
	e.mustNotExist(t, "Refused WS")
}

// The gate must sit above the body decode, so a refused caller gets the
// refusal and not a validation error. This is what distinguishes a guard at
// the top of the handler from one placed after decodeJSON: with the guard
// lower, this request answers 400 and the caller learns its body shape was
// wrong rather than that it may not create workspaces at all.
func TestCreateWorkspace_ConsentRefusalPrecedesBodyValidation(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.do("POST", "/api/v1/workspaces", "application/json",
		[]byte(`{"name":`), e.requestID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on a malformed body from a connection "+
			"that may not create (body=%s)", rr.Code, rr.Body.String())
	}
}

// An empty name is the OTHER validation the handler does, and it is checked
// after the decode succeeds — same ordering claim, second instance, because a
// guard placed between decodeJSON and the name check would pass the test above
// and fail this one.
func TestCreateWorkspace_ConsentRefusalPrecedesNameValidation(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.do("POST", "/api/v1/workspaces", "application/json",
		[]byte(`{"name":""}`), e.requestID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on an empty name from a connection "+
			"that may not create (body=%s)", rr.Code, rr.Body.String())
	}
}

// Control leg 1: the flag SET is the unchanged path — creates AND auto-adds.
// Without this, a guard that refused every OAuth caller would pass every
// refusal test in this file.
func TestCreateWorkspace_OAuthConnectionWithCreateConsent_CreatesAndAutoAdds(t *testing.T) {
	e := newConsentEnv(t, true)

	rr := e.createWorkspace("Allowed WS", e.requestID)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	ws := e.mustExist(t, "Allowed WS")

	slugs, err := e.srv.store.ListConnectionWorkspaceSlugs(e.requestID)
	if err != nil {
		t.Fatalf("ListConnectionWorkspaceSlugs: %v", err)
	}
	found := false
	for _, s := range slugs {
		if s == ws.Slug {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("connection allow-list = %v, want it to contain %q — the auto-add "+
			"behaviour for flag=true is explicitly unchanged by this unit", slugs, ws.Slug)
	}
}

// Control leg 2: a caller that is not an OAuth grant. A guard that read the
// flag off the wrong identity source — or defaulted a missing one to false —
// would break every CLI `pad init` on the platform, which is the most
// expensive way this change could go wrong.
//
// Scope of this fixture, stated because the name is broader than it: it drives
// ONE non-OAuth caller, a PAT. CLI session tokens and local stdio are not
// separately exercised. They are the same case by construction rather than by
// coincidence — the guard branches on the MCP token identity, which only
// MCPBearerAuth sets, so every caller that did not pass through it is
// indistinguishable here. A second fixture would re-test the same branch.
// (Codex round 3 flagged the original comment for claiming all three.)
func TestCreateWorkspace_NonOAuthCallerUnaffected(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.createWorkspace("Pat Made This", "")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a PAT caller (body=%s)", rr.Code, rr.Body.String())
	}
	e.mustExist(t, "Pat Made This")
}

// Control leg 3: an OAuth grant that predates Phase C has no oauth_connections
// row. The backfill mints those rows with may_create_workspaces ON, so the
// missing-row case must ALLOW — treating "no row" as "no permission" would
// refuse every un-backfilled grant.
func TestCreateWorkspace_PrePhaseCGrantWithoutConnectionRow_Allowed(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.createWorkspace("Legacy Grant WS", "req-no-such-connection")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for an OAuth grant with no connection row "+
			"(body=%s)", rr.Code, rr.Body.String())
	}
	e.mustExist(t, "Legacy Grant WS")
}

// --- POST /api/v1/workspaces/import ---

// exportBody produces a valid JSON export payload by round-tripping a
// workspace the test seeded through the store.
func (e *consentEnv) exportBody(t *testing.T) []byte {
	t.Helper()
	src, err := e.srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Export Source", OwnerID: e.user.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace(export source): %v", err)
	}
	export, err := e.srv.store.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	body, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}
	return body
}

func TestImportWorkspace_OAuthConnectionWithoutCreateConsent_403(t *testing.T) {
	e := newConsentEnv(t, false)
	body := e.exportBody(t)

	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-Refused",
		"application/json", body, e.requestID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body=%s)", rr.Code, rr.Body.String())
	}
	if code := errorCode(t, rr); code != "forbidden" {
		t.Errorf("error code = %q, want %q", code, "forbidden")
	}
	e.mustNotExist(t, "Imported-Refused")
}

// The JSON leg above proves the refusal but NOT its placement: it sends a valid
// export, which a gate sitting after decodeJSONWithLimit would also refuse. This
// leg sends a malformed body, so a gate below the decode answers 400 and only a
// gate above it answers 403 — the same discrimination the create-side ordering
// legs make, which the import side was missing. (Codex round 3.)
func TestImportWorkspace_ConsentRefusalPrecedesBodyDecode(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.do("POST", "/api/v1/workspaces/import?name=Malformed-Refused",
		"application/json", []byte(`{"version":`), e.requestID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 on a malformed import body from a connection "+
			"that may not create (body=%s)", rr.Code, rr.Body.String())
	}
	e.mustNotExist(t, "Malformed-Refused")
}

// The gzip Content-Type branch dispatches to handleImportWorkspaceBundle from
// inside handleImportWorkspace. The guard sits above that dispatch, so the
// bundle path is covered by the same check — and a refused caller is turned
// away before the 64 MiB body read rather than after uploading a bundle.
// The payload here is deliberately NOT a valid tar.gz: if the guard were below
// the dispatch, this request would fail with a bundle-parse error instead of
// the consent refusal, so the assertion discriminates placement rather than
// merely re-testing the JSON leg.
func TestImportWorkspace_BundlePathRefusedBeforeParsing(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.do("POST", "/api/v1/workspaces/import?name=Bundle-Refused",
		"application/gzip", []byte("not a gzip stream at all"), e.requestID)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 before the bundle is parsed (body=%s)",
			rr.Code, rr.Body.String())
	}
	e.mustNotExist(t, "Bundle-Refused")
}

// Control: import still works for a connection that may create. Without this,
// a guard that refused all imports outright would pass the two legs above.
func TestImportWorkspace_WithCreateConsent_Imports(t *testing.T) {
	e := newConsentEnv(t, true)
	body := e.exportBody(t)

	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-Allowed",
		"application/json", body, e.requestID)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	e.mustExist(t, "Imported-Allowed")
}

// Control: a non-OAuth caller importing is unaffected — same reasoning, and the
// same fixture scope, as the create-side PAT leg: one PAT stands for the whole
// non-OAuth class because the guard branches on an identity only MCPBearerAuth
// sets.
func TestImportWorkspace_NonOAuthCallerUnaffected(t *testing.T) {
	e := newConsentEnv(t, false)
	body := e.exportBody(t)

	rr := e.do("POST", "/api/v1/workspaces/import?name=Imported-By-Pat",
		"application/json", body, "")

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a PAT caller (body=%s)", rr.Code, rr.Body.String())
	}
	e.mustExist(t, "Imported-By-Pat")
}

// The refusal message has to tell the caller what to DO — an agent that reads
// "forbidden" alone will retry. Pinned loosely (the actionable noun, not the
// whole sentence) so rewording stays cheap.
func TestCreateWorkspace_RefusalMessageNamesTheRemedy(t *testing.T) {
	e := newConsentEnv(t, false)

	rr := e.createWorkspace("Message WS", e.requestID)

	body := rr.Body.String()
	if !strings.Contains(strings.ToLower(body), "re-authorize") {
		t.Errorf("refusal message does not tell the caller how to fix it: %s", body)
	}
	e.mustNotExist(t, "Message WS")
}
