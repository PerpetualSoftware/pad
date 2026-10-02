package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3353: a user-less legacy workspace token carries one workspace and no
// one's authority. It is admitted only where its reach is that workspace.

type legacyFixture struct {
	srv        *Server
	slug, wsID string
	otherSlug  string
	otherWSID  string
	token      string
	victim     *models.User
}

func newLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	other := createWSWithCollections(t, srv)
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	ows, _ := srv.store.GetWorkspaceBySlug(other)
	// The instance has users, so no fresh-install bypass applies.
	victim, err := srv.store.CreateUser(models.UserCreate{Email: "victim-3353@example.com", Name: "Victim", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	// A legacy token row predates users: user_id NULL. Minted for a user and
	// then detached, because the insert path requires one.
	tok, err := srv.store.CreateAPIToken(victim.ID, models.APITokenCreate{Name: "legacy", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatalf("legacy token: %v", err)
	}
	if _, err := srv.store.DB().Exec(`UPDATE api_tokens SET user_id = NULL WHERE id = ?`, tok.ID); err != nil {
		t.Fatalf("detach legacy token: %v", err)
	}
	return legacyFixture{srv: srv, slug: slug, wsID: ws.ID, otherSlug: other, otherWSID: ows.ID, token: tok.Token, victim: victim}
}

func (f legacyFixture) do(method, path string, body any) *httptest.ResponseRecorder {
	return doRequestWithBearer(f.srv, method, path, f.token, body)
}

func allWorkspacesCount(t *testing.T, srv *Server) int {
	t.Helper()
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM workspaces`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The bug as filed: a workspace token mints a workspace attributed to anyone,
// with no plan limits. Refused, and nothing is created.
func TestBUG3353_LegacyTokenCannotMintAWorkspace(t *testing.T) {
	f := newLegacyFixture(t)
	before := allWorkspacesCount(t, f.srv)
	for _, c := range []struct{ path string }{{"/api/v1/workspaces"}, {"/api/v1/workspaces/import"}} {
		rr := f.do("POST", c.path, map[string]any{"name": "Planted", "slug": "planted", "owner_id": f.victim.ID})
		if rr.Code != http.StatusForbidden || errorCode(t, rr) != "legacy_token_not_allowed" {
			t.Errorf("legacy token POST %s: %d %s, want 403 legacy_token_not_allowed", c.path, rr.Code, rr.Body.String())
		}
	}
	if after := allWorkspacesCount(t, f.srv); after != before {
		t.Fatalf("workspaces %d → %d: a legacy token minted one", before, after)
	}
}

// The mint door's own answer, without RequireAuth in front of it: a caller
// with no user mints only on a fresh install with no token.
func TestBUG3353_MintDoorRefusesAUserlessCallerOnAnInitializedInstance(t *testing.T) {
	f := newLegacyFixture(t)
	req := newRequestWithTokenWorkspace("POST", "/api/v1/workspaces", f.wsID)
	rr := httptest.NewRecorder()
	if _, ok := f.srv.beginWorkspaceMint(rr, req); ok || rr.Code != http.StatusForbidden {
		t.Fatalf("beginWorkspaceMint with a legacy token: ok=%v %d", ok, rr.Code)
	}
}

// A signed-in caller's body owner_id is ignored: the owner is the caller.
func TestBUG3353_BodyOwnerIDIsIgnored(t *testing.T) {
	f := newLegacyFixture(t)
	creator, sess := loginTestUser(t, f.srv)
	rr := doRequestWithBearer(f.srv, "POST", "/api/v1/workspaces", sess, map[string]any{"name": "Mine", "owner_id": f.victim.ID})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	if ws.OwnerID != creator.ID {
		t.Fatalf("owner = %s, want the creator %s (the body named %s)", ws.OwnerID, creator.ID, f.victim.ID)
	}
}

// GET /workspaces with a legacy token lists its own workspace, not every
// tenant.
func TestBUG3353_LegacyTokenListsOnlyItsWorkspace(t *testing.T) {
	f := newLegacyFixture(t)
	rr := f.do("GET", "/api/v1/workspaces", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var got []models.Workspace
	parseJSON(t, rr, &got)
	if len(got) != 1 || got[0].ID != f.wsID {
		t.Fatalf("legacy token listed %d workspaces (%+v), want only its own", len(got), got)
	}
}

// A foreign workspace answers exactly what a missing one does, on the
// workspace routes and the event stream, so the token learns no slugs.
func TestBUG3353_ForeignWorkspaceIsIndistinguishableFromMissing(t *testing.T) {
	f := newLegacyFixture(t)
	for _, tmpl := range []string{"/api/v1/workspaces/%s/collections", "/api/v1/events?workspace=%s"} {
		foreign := f.do("GET", strings.Replace(tmpl, "%s", f.otherSlug, 1), nil)
		missing := f.do("GET", strings.Replace(tmpl, "%s", "no-such-workspace-3353", 1), nil)
		if foreign.Code != http.StatusNotFound || foreign.Code != missing.Code || foreign.Body.String() != missing.Body.String() {
			t.Errorf("%s: foreign %d %s vs missing %d %s, want the same 404", tmpl, foreign.Code, foreign.Body.String(), missing.Code, missing.Body.String())
		}
	}
	// Its own workspace still works.
	if rr := f.do("GET", "/api/v1/workspaces/"+f.slug+"/collections", nil); rr.Code != http.StatusOK {
		t.Fatalf("own workspace: %d %s", rr.Code, rr.Body.String())
	}
}

// The collab socket answers an item outside the token's workspace as it
// answers a missing item.
func TestBUG3353_ForeignCollabItemIsNotFound(t *testing.T) {
	f := newLegacyFixture(t)
	item, err := f.srv.store.CreateItem(f.otherWSID, mustCollectionID(t, f.srv, f.otherWSID, "tasks"), models.ItemCreate{Title: "Theirs", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	req := newRequestWithTokenWorkspace("GET", "/api/v1/collab/"+item.ID, f.wsID)
	if _, err := f.srv.authorizeCollabAccess(req, item); err == nil {
		t.Fatal("collab granted a legacy token an item outside its workspace")
	} else if se, ok := err.(*statusError); !ok || se.code != http.StatusNotFound {
		t.Fatalf("collab on a foreign item: %v, want the item 404", err)
	}
}

// A user-owned token whose user cannot be read is never downgraded to a
// user-less one: that would hand it a legacy token's reach for the request.
func TestBUG3353_UserOwnedTokenIsNeverDowngraded(t *testing.T) {
	f := newLegacyFixture(t)
	owner, _ := loginTestUser(t, f.srv)
	if err := f.srv.store.AddWorkspaceMember(f.wsID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	tok, err := f.srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{Name: "owned", WorkspaceID: f.wsID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The user is gone (the row points at nobody): 401, not a user-less pass.
	pointAtNobody(t, f.srv, tok.ID)
	if rr := doRequestWithBearer(f.srv, "GET", "/api/v1/workspaces/"+f.slug+"/collections", tok.Token, nil); rr.Code != http.StatusUnauthorized {
		t.Fatalf("a token whose user is gone: %d %s, want 401", rr.Code, rr.Body.String())
	}
	// The user cannot be read (a fault): 500, not a user-less pass.
	if _, err := f.srv.store.DB().Exec(`UPDATE api_tokens SET user_id = ? WHERE id = ?`, owner.ID, tok.ID); err != nil {
		t.Fatal(err)
	}
	breakTable(t, f.srv, "users")
	if rr := doRequestWithBearer(f.srv, "GET", "/api/v1/workspaces/"+f.slug+"/collections", tok.Token, nil); rr.Code != http.StatusInternalServerError {
		t.Fatalf("a token whose user cannot be read: %d %s, want 500", rr.Code, rr.Body.String())
	}
}

// Every global route the router registers refuses a legacy token unless it is
// on the allowlist, so a route added later fails closed by test, not only by
// code. Workspace-scoped routes are pinned to the token's workspace by
// RequireWorkspaceAccess and are covered above.
func TestBUG3353_EveryGlobalRouteRefusesALegacyToken(t *testing.T) {
	// Every route is hit once from one address; the API burst is not what
	// is under test.
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newLegacyFixture(t)
	f.srv.ensureRouter()
	param := regexp.MustCompile(`\{[^}]*\}`)
	checked := 0
	err := chi.Walk(f.srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") || isPublicAPIPath(route) {
			return nil
		}
		path := strings.TrimSuffix(param.ReplaceAllString(route, "x"), "/*")
		path = strings.ReplaceAll(path, "*", "x")
		req := newRequestWithTokenWorkspace(method, path, f.wsID)
		if legacyTokenRouteAllowed(req) {
			return nil
		}
		checked++
		rr := doRequestWithBearer(f.srv, method, path, f.token, nil)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "legacy_token_not_allowed") {
			t.Errorf("%s %s (%s): %d %s, want 403 legacy_token_not_allowed", method, route, path, rr.Code, rr.Body.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d global routes were checked; the walk is not seeing the router", checked)
	}
}

// newRequestWithTokenWorkspace is a request as TokenAuth leaves a user-less
// legacy workspace token: a workspace and no user.
func newRequestWithTokenWorkspace(method, path, wsID string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	return req.WithContext(context.WithValue(req.Context(), ctxTokenWorkspaceID, wsID))
}

// pointAtNobody makes a token's user_id name no user, as a row left behind
// by a user removed without the cascade would. Foreign keys are off for that
// one statement on one connection.
func pointAtNobody(t *testing.T, srv *Server, tokenID string) {
	t.Helper()
	ctx := context.Background()
	conn, err := srv.store.DB().Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for _, q := range []string{`PRAGMA foreign_keys = OFF`, `UPDATE api_tokens SET user_id = 'ghost-3353' WHERE id = '` + tokenID + `'`, `PRAGMA foreign_keys = ON`} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// The global routes a legacy token must be refused on, named independently
// of legacyTokenRouteAllowed: the walk above consults that function, so a
// route it wrongly allows would be skipped there, not caught (codex review).
func TestBUG3353_NamedGlobalRoutesRefuseALegacyToken(t *testing.T) {
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := newLegacyFixture(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/workspaces"},
		{"POST", "/api/v1/workspaces/import"},
		{"GET", "/api/v1/workspaces/deleted"},
		{"PUT", "/api/v1/workspaces/reorder"},
		{"POST", "/api/v1/workspaces/" + f.otherSlug + "/restore"},
		{"POST", "/api/v1/import/url"},
		{"GET", "/api/v1/admin/users"},
		{"GET", "/api/v1/me/workspace-tabs"},
		{"GET", "/api/v1/oauth/clients/x/public-info"},
	} {
		rr := f.do(c.method, c.path, nil)
		if rr.Code != http.StatusForbidden || errorCode(t, rr) != "legacy_token_not_allowed" {
			t.Errorf("legacy token %s %s: %d %s, want 403 legacy_token_not_allowed", c.method, c.path, rr.Code, rr.Body.String())
		}
	}
}

// The setup-mode mint (no users, no token) still works, and a body owner_id
// is dropped there too: the owner is always the authenticated user, and
// there is none.
func TestBUG3353_SetupMintIgnoresABodyOwnerID(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]any{"name": "First", "owner_id": "someone-3353"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("setup-mode create: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	if ws.OwnerID != "" {
		t.Fatalf("setup-mode create kept the body owner_id %q", ws.OwnerID)
	}
}
