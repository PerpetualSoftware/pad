package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3982: a user-owned API token minted with a workspace_id is confined to
// that workspace. The tests below run as the token's user, who is a member of
// BOTH workspaces, so only the pin separates what the token may reach.

type pinFixture struct {
	*accessFixture
	bSlug, bID string
	pinned     string // a user PAT pinned to A
}

func newPinFixture(t *testing.T, driver store.DriverType) *pinFixture {
	t.Helper()
	t.Setenv("PAD_DISABLE_RATE_LIMITS", "1")
	f := &pinFixture{accessFixture: newAccessFixture(t, driver)}
	rr := f.do("POST", "/api/v1/workspaces", f.ownerTok, map[string]any{"name": "Other WS", "template": "startup"})
	f.must(rr, http.StatusCreated, "create B")
	var b models.Workspace
	parseJSON(t, rr, &b)
	f.bSlug, f.bID = b.Slug, b.ID
	for _, ws := range []string{f.wsSlug, f.bSlug} {
		rr := f.do("POST", "/api/v1/workspaces/"+ws+"/collections/tasks/items", f.ownerTok, map[string]any{"title": "zebrafish in " + ws})
		f.must(rr, http.StatusCreated, "item in "+ws)
	}
	tok, err := f.srv.store.CreateAPIToken(f.owner.ID, models.APITokenCreate{Name: "pinned-A", WorkspaceID: f.wsID}, 0, 0)
	if err != nil {
		t.Fatalf("mint pinned PAT: %v", err)
	}
	f.pinned = tok.Token
	return f
}

type denial struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Reason         string `json:"reason"`
			TokenWorkspace string `json:"token_workspace"`
		} `json:"details"`
	} `json:"error"`
}

// mustBePinDenial fails unless rr is the 403 a pinned token gets, naming its
// workspace and how to mint an unscoped token.
func (f *pinFixture) mustBePinDenial(rr *httptest.ResponseRecorder, what string) {
	f.t.Helper()
	var d denial
	_ = json.Unmarshal(rr.Body.Bytes(), &d)
	if rr.Code != http.StatusForbidden || d.Error.Details.Reason != "token_workspace_pinned" {
		f.t.Fatalf("%s: %d %s, want 403 token_workspace_pinned", what, rr.Code, rr.Body.String())
	}
	if d.Error.Details.TokenWorkspace != f.wsSlug ||
		!strings.Contains(d.Error.Message, `"`+f.wsSlug+`"`) ||
		!strings.Contains(d.Error.Message, "pad token create --name <name>") {
		f.t.Fatalf("%s: message %q does not name workspace %q and the mint command", what, d.Error.Message, f.wsSlug)
	}
}

func (f *pinFixture) searchSlugs(tok, path string) (int, []string) {
	f.t.Helper()
	rr := f.do("GET", path, tok, nil)
	var body struct {
		Results []struct {
			Workspace *struct {
				Slug string `json:"slug"`
			} `json:"workspace"`
		} `json:"results"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &body)
	var out []string
	for _, r := range body.Results {
		if r.Workspace != nil {
			out = append(out, r.Workspace.Slug)
		}
	}
	return rr.Code, out
}

func TestBUG3982_RESTReadRefusedOutsidePin(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	for _, p := range []string{"/items", "/collections", "/dashboard"} {
		f.mustBePinDenial(f.do("GET", "/api/v1/workspaces/"+f.bSlug+p, f.pinned, nil), "pinned GET B"+p)
		// The same request by UUID is the same refusal.
		f.mustBePinDenial(f.do("GET", "/api/v1/workspaces/"+f.bID+p, f.pinned, nil), "pinned GET B-by-id"+p)
	}
	f.must(f.do("GET", "/api/v1/workspaces/"+f.wsSlug+"/items", f.pinned, nil), http.StatusOK, "pinned GET A items")
	f.must(f.do("GET", "/api/v1/workspaces/"+f.bSlug+"/items", f.ownerTok, nil), http.StatusOK, "unpinned PAT GET B items")
}

func TestBUG3982_RESTWriteRefusedOutsidePin(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	count := func() int {
		items, err := f.srv.store.ListItems(f.bID, models.ItemListParams{})
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	before := count()
	f.mustBePinDenial(f.do("POST", "/api/v1/workspaces/"+f.bSlug+"/collections/tasks/items", f.pinned,
		map[string]any{"title": "written by pinned-A"}), "pinned POST item in B")
	if after := count(); after != before {
		t.Fatalf("B has %d items after a refused write, had %d", after, before)
	}
	f.must(f.do("POST", "/api/v1/workspaces/"+f.wsSlug+"/collections/tasks/items", f.pinned,
		map[string]any{"title": "written by pinned-A in A"}), http.StatusCreated, "pinned POST item in A")
}

func TestBUG3982_SearchFanOutConfined(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)

	code, slugs := f.searchSlugs(f.pinned, "/api/v1/search?q=zebrafish")
	if code != http.StatusOK || len(slugs) != 1 || slugs[0] != f.wsSlug {
		t.Fatalf("unscoped search by a pinned token: %d %v, want 200 only %s", code, slugs, f.wsSlug)
	}
	code, slugs = f.searchSlugs(f.pinned, "/api/v1/search?q=zebrafish&workspace="+f.wsSlug)
	if code != http.StatusOK || len(slugs) != 1 || slugs[0] != f.wsSlug {
		t.Fatalf("search scoped to the pin: %d %v, want 200 only %s", code, slugs, f.wsSlug)
	}
	f.mustBePinDenial(f.do("GET", "/api/v1/search?q=zebrafish&workspace="+f.bSlug, f.pinned, nil), "pinned search naming B")
	// Controls: an unpinned PAT sees both, and so does the owner scoped to B.
	if _, slugs = f.searchSlugs(f.ownerTok, "/api/v1/search?q=zebrafish"); len(slugs) != 2 {
		t.Fatalf("unpinned PAT search: %v, want both workspaces", slugs)
	}
	if _, slugs = f.searchSlugs(f.ownerTok, "/api/v1/search?q=zebrafish&workspace="+f.bSlug); len(slugs) != 1 {
		t.Fatalf("unpinned PAT scoped search: %v, want B", slugs)
	}
}

// A refused search names no more than the token already knew: a workspace that
// exists and one that does not get the same bytes.
func TestBUG3982_SearchRefusalIsNotAnExistenceOracle(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	existing := f.do("GET", "/api/v1/search?q=zebrafish&workspace="+f.bSlug, f.pinned, nil)
	missing := f.do("GET", "/api/v1/search?q=zebrafish&workspace=no-such-workspace-3982", f.pinned, nil)
	f.mustBePinDenial(existing, "search naming an existing non-pin workspace")
	f.mustBePinDenial(missing, "search naming a missing workspace")
	if existing.Body.String() != missing.Body.String() {
		t.Fatalf("existing %s vs missing %s: the refusals differ", existing.Body.String(), missing.Body.String())
	}
}

func TestBUG3982_WorkspaceListAndExportConfined(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	rr := f.do("GET", "/api/v1/workspaces", f.pinned, nil)
	f.must(rr, http.StatusOK, "pinned GET /workspaces")
	var list []models.Workspace
	parseJSON(t, rr, &list)
	if len(list) != 1 || list[0].ID != f.wsID {
		t.Fatalf("pinned GET /workspaces listed %d workspaces, want exactly A", len(list))
	}
	rr = f.do("GET", "/api/v1/workspaces", f.ownerTok, nil)
	parseJSON(t, rr, &list)
	if len(list) < 2 {
		t.Fatalf("control: the unpinned PAT lists %d workspaces, want both", len(list))
	}
	f.mustBePinDenial(f.do("GET", "/api/v1/auth/export", f.pinned, nil), "pinned GET /auth/export")
}

func TestBUG3982_SSEAndCollabRefuseOtherWorkspace(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	f.srv.SetEventBus(events.New())
	f.mustBePinDenial(f.do("GET", "/api/v1/events?workspace="+f.bSlug, f.pinned, nil), "pinned SSE on B")

	items, err := f.srv.store.ListItems(f.bID, models.ItemListParams{})
	if err != nil || len(items) == 0 {
		t.Fatalf("B items: %v %d", err, len(items))
	}
	f.mustBePinDenial(f.do("GET", "/api/v1/collab/"+items[0].ID, f.pinned, nil), "pinned collab on a B item")
}

func TestBUG3982_RefResolver404sOtherWorkspace(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	items, err := f.srv.store.ListItems(f.bID, models.ItemListParams{})
	if err != nil || len(items) == 0 {
		t.Fatalf("B items: %v %d", err, len(items))
	}
	// The redirect needs the owner's username.
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`UPDATE users SET username = ? WHERE id = ?`), "pin-owner-3982", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	ref := items[0].CollectionPrefix + "-" + itoa(*items[0].ItemNumber)
	// Control: the unpinned PAT is redirected.
	if rr := f.do("GET", "/-/r/"+f.bSlug+"/"+ref, f.ownerTok, nil); rr.Code != http.StatusFound {
		t.Fatalf("control: unpinned resolver %d %s, want 302", rr.Code, rr.Body.String())
	}
	if rr := f.do("GET", "/-/r/"+f.bSlug+"/"+ref, f.pinned, nil); rr.Code != http.StatusNotFound {
		t.Fatalf("pinned resolver on B: %d %s, want 404", rr.Code, rr.Body.String())
	}
}

func TestBUG3982_CrossWorkspaceCopyRefused(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	items, err := f.srv.store.ListItems(f.wsID, models.ItemListParams{})
	if err != nil || len(items) == 0 {
		t.Fatalf("A items: %v %d", err, len(items))
	}
	path := "/api/v1/workspaces/" + f.wsSlug + "/items/" + items[0].Slug + "/copy"
	body := map[string]any{"target_workspace": f.bSlug, "target_collection": "tasks"}
	f.mustBePinDenial(f.do("POST", path, f.pinned, body), "pinned copy A to B")
	f.mustBePinDenial(f.do("POST", path+"/preflight", f.pinned, body), "pinned copy preflight A to B")
	// Control: the unpinned PAT may.
	f.must(f.do("POST", path, f.ownerTok, body), http.StatusCreated, "unpinned copy A to B")
}

// pinnedAllowedGlobalRoutes is every route outside /workspaces/{slug}/ that a
// pinned token may use, written out by hand (method + chi pattern) rather than
// derived from pinnedTokenRouteAllowed. Each one either confines itself to the
// pin (the workspace list, search, events, collab, the token doors, the needs
// inbox), reads nothing of the caller's (pre-auth flows, health, libraries,
// share links), or only says who the token is. A route the gate admits that is
// not here fails the walk below; that is how import-status was found.
var pinnedAllowedGlobalRoutes = map[string]bool{
	"DELETE /api/v1/auth/tokens/{tokenID}":             true,
	"GET /api/v1/auth/check-username":                  true,
	"GET /api/v1/auth/cli/sessions/{code}":             true,
	"GET /api/v1/auth/me":                              true,
	"GET /api/v1/auth/session":                         true,
	"GET /api/v1/auth/tokens":                          true,
	"GET /api/v1/collab/{itemID}":                      true,
	"GET /api/v1/convention-library":                   true,
	"GET /api/v1/events":                               true,
	"GET /api/v1/health":                               true,
	"GET /api/v1/health/live":                          true,
	"GET /api/v1/health/ready":                         true,
	"GET /api/v1/invitations/{code}/preview":           true,
	"GET /api/v1/library/entry":                        true,
	"GET /api/v1/mcp/tool-surface":                     true,
	"GET /api/v1/me/needs":                             true,
	"GET /api/v1/plan-limits":                          true,
	"GET /api/v1/playbook-library":                     true,
	"GET /api/v1/s/{token}":                            true,
	"GET /api/v1/s/{token}/attachments/{attachmentID}": true,
	"GET /api/v1/search":                               true,
	"GET /api/v1/server/capabilities":                  true,
	"GET /api/v1/templates":                            true,
	"GET /api/v1/workspaces":                           true,
	"POST /api/v1/auth/2fa/login-verify":               true,
	"POST /api/v1/auth/bootstrap":                      true,
	"POST /api/v1/auth/cli/sessions":                   true,
	"POST /api/v1/auth/forgot-password":                true,
	"POST /api/v1/auth/local-reset":                    true,
	"POST /api/v1/auth/login":                          true,
	"POST /api/v1/auth/logout":                         true,
	"POST /api/v1/auth/register":                       true,
	"POST /api/v1/auth/resend-verification":            true,
	"POST /api/v1/auth/reset-password":                 true,
	"POST /api/v1/auth/verify-email":                   true,
}

// pinConfinedRoute reports whether chi pattern route sits inside the
// /workspaces/{slug} subrouter, where RequireWorkspaceAccess holds the pin.
// Restore is registered beside it, not in it.
func pinConfinedRoute(route string) bool {
	rest, ok := strings.CutPrefix(route, "/api/v1/workspaces/{slug}")
	return ok && (rest == "" || strings.HasPrefix(rest, "/")) && rest != "/restore"
}

// Every route the router registers, public ones included, either sits inside
// the pin-confined workspace subrouter, is in pinnedAllowedGlobalRoutes, or
// refuses a pinned token. Both directions are checked: an admitted route must
// be expected, and a refused one must refuse.
func TestBUG3982_EveryGlobalRouteRefusesAPinnedPAT(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	f.srv.ensureRouter()
	param := regexp.MustCompile(`\{[^}]*\}`)
	checked, admitted := 0, map[string]bool{}
	err := chi.Walk(f.srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if !strings.HasPrefix(route, "/api/v1/") {
			return nil
		}
		route = strings.TrimSuffix(route, "/")
		path := strings.TrimSuffix(param.ReplaceAllString(route, "x"), "/*")
		path = strings.ReplaceAll(path, "*", "x")
		key := method + " " + route
		if pinnedTokenRouteAllowed(httptest.NewRequest(method, path, nil)) {
			if !pinConfinedRoute(route) && !pinnedAllowedGlobalRoutes[key] {
				t.Errorf("a pinned token is admitted to %s, which is outside the workspace subrouter and not in pinnedAllowedGlobalRoutes", key)
			}
			admitted[key] = true
			return nil
		}
		if pinnedAllowedGlobalRoutes[key] {
			t.Errorf("%s is expected open to a pinned token but the gate refuses it", key)
		}
		checked++
		f.mustBePinDenial(f.do(method, path, f.pinned, nil), key)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("only %d global routes were checked; the walk is not seeing the router", checked)
	}
	for key := range pinnedAllowedGlobalRoutes {
		if !admitted[key] {
			t.Errorf("pinnedAllowedGlobalRoutes lists %s, which the router no longer registers or the gate refuses", key)
		}
	}
}

// The routes a pinned token must be refused on, named independently of
// pinnedTokenRouteAllowed: the walk consults that function, so a route it
// wrongly allows would be skipped there, not caught.
func TestBUG3982_NamedGlobalRoutesRefuseAPinnedPAT(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/v1/workspaces"},
		{"POST", "/api/v1/workspaces/import"},
		{"GET", "/api/v1/workspaces/deleted"},
		{"GET", "/api/v1/workspaces/import-status?key=abcdefgh"},
		{"PUT", "/api/v1/workspaces/reorder"},
		{"POST", "/api/v1/workspaces/" + f.bSlug + "/restore"},
		{"GET", "/api/v1/auth/export"},
		{"PATCH", "/api/v1/auth/me"},
		{"POST", "/api/v1/auth/tokens"},
		{"POST", "/api/v1/auth/tokens/x/rotate"},
		{"GET", "/api/v1/me/invitations"},
		{"POST", "/api/v1/invitations/x/accept"},
		{"GET", "/api/v1/me/workspace-tabs"},
		{"GET", "/api/v1/events/stream"},
		{"GET", "/api/v1/watches"},
		{"POST", "/api/v1/import/url"},
		{"GET", "/api/v1/admin/users"},
	} {
		f.mustBePinDenial(f.do(c.method, c.path, f.pinned, nil), c.method+" "+c.path)
	}
}

// What a pinned token must keep: who it is, its owner's token list, and
// revoking itself (the incident path).
func TestBUG3982_AccountDoorsStayOpen(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	f.must(f.do("GET", "/api/v1/auth/me", f.pinned, nil), http.StatusOK, "GET /auth/me")
	rr := f.do("GET", "/api/v1/auth/tokens", f.pinned, nil)
	f.must(rr, http.StatusOK, "GET /auth/tokens")
	var toks []models.APIToken
	parseJSON(t, rr, &toks)
	var pinnedID string
	for _, tk := range toks {
		if tk.Name == "pinned-A" {
			pinnedID = tk.ID
		}
	}
	if pinnedID == "" {
		t.Fatalf("pinned token missing from its owner's list: %s", rr.Body.String())
	}
	f.must(f.do("GET", "/api/v1/me/needs", f.pinned, nil), http.StatusOK, "GET /me/needs")
	f.must(f.do("DELETE", "/api/v1/auth/tokens/"+pinnedID, f.pinned, nil), http.StatusNoContent, "DELETE own token")
}

// The token doors a pinned token keeps are narrowed to the tokens pinned to its
// own workspace: it lists only those, and revoking any other id is the 404 a
// missing id gets, so the doors say nothing about the user's other tokens.
func TestBUG3982_TokenDoorsNarrowToThePin(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	mint := func(u *models.User, name, wsID string) string {
		t.Helper()
		tok, err := f.srv.store.CreateAPIToken(u.ID, models.APITokenCreate{Name: name, WorkspaceID: wsID}, 0, 0)
		if err != nil {
			t.Fatalf("mint %s: %v", name, err)
		}
		return tok.ID
	}
	siblingID := mint(f.owner, "pinned-A-2", f.wsID)
	pinnedBID := mint(f.owner, "pinned-B", f.bID)
	unpinnedID := mint(f.owner, "unpinned", "")
	// Another member's token pinned to the same workspace: same pin, not the
	// caller's, so it is neither listed nor revocable.
	other := mkUser(t, f.srv, "other@example.com")
	if err := f.srv.store.AddWorkspaceMember(f.wsID, other.ID, "editor"); err != nil {
		t.Fatalf("add other to A: %v", err)
	}
	otherID := mint(other, "other-pinned-A", f.wsID)

	names := func(tok string) map[string]bool {
		t.Helper()
		rr := f.do("GET", "/api/v1/auth/tokens", tok, nil)
		f.must(rr, http.StatusOK, "GET /auth/tokens")
		var toks []models.APIToken
		parseJSON(t, rr, &toks)
		out := map[string]bool{}
		for _, tk := range toks {
			out[tk.Name] = true
		}
		return out
	}
	if got := names(f.pinned); len(got) != 2 || !got["pinned-A"] || !got["pinned-A-2"] {
		t.Fatalf("pinned token lists %v, want exactly pinned-A and pinned-A-2", got)
	}
	// Control: the unpinned PAT still lists every token of its user.
	if got := names(f.ownerTok); !got["pinned-B"] || !got["unpinned"] || !got["pinned-A"] {
		t.Fatalf("unpinned PAT lists %v, want all of the owner's tokens", got)
	}

	missing := f.do("DELETE", "/api/v1/auth/tokens/no-such-token", f.pinned, nil)
	f.must(missing, http.StatusNotFound, "DELETE a missing id")
	for name, id := range map[string]string{"pinned-B": pinnedBID, "unpinned": unpinnedID, "other-pinned-A": otherID} {
		rr := f.do("DELETE", "/api/v1/auth/tokens/"+id, f.pinned, nil)
		if rr.Code != http.StatusNotFound || rr.Body.String() != missing.Body.String() {
			t.Fatalf("pinned DELETE %s: %d %s, want the missing-id 404 %s", name, rr.Code, rr.Body.String(), missing.Body.String())
		}
	}
	if got := names(f.ownerTok); !got["pinned-B"] || !got["unpinned"] {
		t.Fatalf("a refused revoke deleted a token anyway: owner now lists %v", got)
	}
	if tk, err := f.srv.store.ListUserAPITokens(other.ID); err != nil || len(tk) != 1 {
		t.Fatalf("other member's pinned token after the refused revoke: %v, %v", tk, err)
	}
	f.must(f.do("DELETE", "/api/v1/auth/tokens/"+siblingID, f.pinned, nil), http.StatusNoContent, "DELETE a sibling pinned to A")
	// Control: the unpinned PAT still revokes a token pinned elsewhere.
	f.must(f.do("DELETE", "/api/v1/auth/tokens/"+pinnedBID, f.ownerTok, nil), http.StatusNoContent, "unpinned DELETE pinned-B")
}

func TestBUG3982_DeletedPinFailsClosed(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	if err := f.srv.store.DeleteWorkspace(f.wsSlug); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	rr := f.do("GET", "/api/v1/workspaces/"+f.bSlug+"/items", f.pinned, nil)
	var d denial
	_ = json.Unmarshal(rr.Body.Bytes(), &d)
	if rr.Code != http.StatusForbidden || d.Error.Details.Reason != "token_workspace_pinned" ||
		!strings.Contains(d.Error.Message, "no longer exists") {
		t.Fatalf("pinned to a deleted workspace, GET B items: %d %s", rr.Code, rr.Body.String())
	}
	rr = f.do("GET", "/api/v1/workspaces", f.pinned, nil)
	f.must(rr, http.StatusOK, "GET /workspaces")
	var list []models.Workspace
	parseJSON(t, rr, &list)
	if len(list) != 0 {
		t.Fatalf("a token pinned to a deleted workspace lists %d workspaces", len(list))
	}
	if _, slugs := f.searchSlugs(f.pinned, "/api/v1/search?q=zebrafish"); len(slugs) != 0 {
		t.Fatalf("unscoped search by a token pinned to a deleted workspace: %v", slugs)
	}
	f.must(f.do("GET", "/api/v1/auth/me", f.pinned, nil), http.StatusOK, "GET /auth/me stays open")
}

// Tokens that carry no pin keep today's reach.
func TestBUG3982_UnpinnedCredentialsUnchanged(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	for _, path := range []string{"/api/v1/workspaces/" + f.wsSlug + "/items", "/api/v1/workspaces/" + f.bSlug + "/items", "/api/v1/auth/export"} {
		f.must(f.do("GET", path, f.ownerTok, nil), http.StatusOK, "unpinned PAT "+path)
	}
	// A legacy user-less workspace token: its own confinement (BUG-3353) is
	// not the pin's, and the pin does not touch it.
	legacy := newLegacyFixture(t)
	if rr := legacy.do("GET", "/api/v1/workspaces/"+legacy.slug+"/collections", nil); rr.Code != http.StatusOK {
		t.Fatalf("legacy token on its own workspace: %d %s", rr.Code, rr.Body.String())
	}
	rr := legacy.do("GET", "/api/v1/workspaces/"+legacy.otherSlug+"/collections", nil)
	if rr.Code != http.StatusNotFound || strings.Contains(rr.Body.String(), "token_workspace_pinned") {
		t.Fatalf("legacy token on a foreign workspace: %d %s, want its own 404", rr.Code, rr.Body.String())
	}
}

// The pin holds on PostgreSQL as it does on SQLite.
func TestBUG3982_PinnedTokenOnPostgres(t *testing.T) {
	f := newPinFixture(t, store.DriverPostgres)
	f.mustBePinDenial(f.do("GET", "/api/v1/workspaces/"+f.bSlug+"/items", f.pinned, nil), "pinned GET B")
	if _, slugs := f.searchSlugs(f.pinned, "/api/v1/search?q=zebrafish"); len(slugs) != 1 || slugs[0] != f.wsSlug {
		t.Fatalf("unscoped search by a pinned token: %v", slugs)
	}
}

// The pin is compared by workspace ID as well as by slug, so the check does
// not rest on slugs being unique. Simulated by a pin whose slug names B while
// its ID is A's, with no allow-list to catch it first.
func TestBUG3982_WorkspaceAccessComparesThePinByID(t *testing.T) {
	f := newPinFixture(t, store.DriverSQLite)
	r := chi.NewRouter()
	r.With(f.srv.RequireWorkspaceAccess).Get("/w/{slug}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest("GET", "/w/"+f.bSlug, nil)
	ctx := WithCurrentUser(req.Context(), f.owner)
	ctx = context.WithValue(ctx, ctxTokenPin, &tokenPin{ID: f.wsID, Slug: f.bSlug})
	rr := httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "token_workspace_pinned") {
		t.Fatalf("a pin for A's ID, naming B's slug, on B: %d %s, want 403 token_workspace_pinned", rr.Code, rr.Body.String())
	}
	// Control: the pin's own ID passes.
	ctx = context.WithValue(WithCurrentUser(req.Context(), f.owner), ctxTokenPin, &tokenPin{ID: f.bID, Slug: f.bSlug})
	rr = httptest.NewRecorder()
	r.ServeHTTP(rr, req.WithContext(ctx))
	if rr.Code != http.StatusOK {
		t.Fatalf("control: a pin for B on B: %d %s", rr.Code, rr.Body.String())
	}
}
