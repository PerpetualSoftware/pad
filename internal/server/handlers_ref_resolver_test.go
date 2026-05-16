package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// newJSONRequest is a local helper for tests that need to thread custom
// headers (Bearer auth, RemoteAddr) through to a request — the package's
// doRequest helpers wrap the request creation, so they can't compose
// with per-call Authorization headers.
func newJSONRequest(t *testing.T, method, path string, body interface{}) *http.Request {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

// refResolverFixture seeds a (user, workspace, bearer-token) triple ready
// for resolver tests that need a real owner-username (so the redirect
// target's leading path segment is non-empty — Codex round-2 P1.3
// otherwise 404s an ownerless workspace). Returns helpers that issue
// authenticated requests against the seeded workspace.
type refResolverFixture struct {
	t      *testing.T
	srv    *Server
	owner  *models.User
	ws     *models.Workspace
	token  string
	doAs   func(method, path string, body interface{}) *httptest.ResponseRecorder
	doAuth func(token, method, path string, body interface{}) *httptest.ResponseRecorder
}

func newRefResolverFixture(t *testing.T) *refResolverFixture {
	t.Helper()
	srv := testServer(t)

	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Username: "owneruser",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Claude", OwnerID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{
		Name: "owner-tok", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	doAuth := func(token, method, path string, body interface{}) *httptest.ResponseRecorder {
		t.Helper()
		req := newJSONRequest(t, method, path, body)
		req.Header.Set("Authorization", "Bearer "+token)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	doAs := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		return doAuth(tok.Token, method, path, body)
	}

	return &refResolverFixture{
		t: t, srv: srv, owner: owner, ws: ws, token: tok.Token,
		doAs: doAs, doAuth: doAuth,
	}
}

// seedItem creates a collection (idempotent on slug) + an item with the
// given title, returning the resolved item record.
func (f *refResolverFixture) seedItem(collSlug, collPrefix, title string) *models.Item {
	f.t.Helper()
	rr := f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections", map[string]interface{}{
		"name": collSlug, "slug": collSlug, "prefix": collPrefix,
	})
	// 201 on fresh create; 409 / 200 acceptable on duplicate-slug retries.
	if rr.Code != http.StatusCreated && rr.Code != http.StatusConflict {
		f.t.Fatalf("create collection %q: %d %s", collSlug, rr.Code, rr.Body.String())
	}
	rr = f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections/"+collSlug+"/items",
		map[string]interface{}{"title": title})
	if rr.Code != http.StatusCreated {
		f.t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(f.t, rr, &item)
	if item.Ref == "" {
		f.t.Fatalf("seeded item has no ref: %+v", item)
	}
	return &item
}

// TestRefResolver_Success creates a workspace + collection + item, then
// asserts that GET /-/r/{workspace}/{REF} produces a 302 whose Location
// header points at the canonical item URL itemUrlId() would produce on
// the frontend. The URL shape is the round-2 sentinel /-/r/... — the
// previous /{username}/{workspace}/ref/{ref} shape risked collision with
// pre-existing `ref`-slugged collections (Codex round-2 P1.4).
func TestRefResolver_Success(t *testing.T) {
	f := newRefResolverFixture(t)
	item := f.seedItem("decisions", "DECIS", "Orchestration model")

	rr := f.doAs("GET", "/-/r/"+f.ws.Slug+"/"+item.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	want := "/owneruser/" + f.ws.Slug + "/decisions/" + item.Ref
	if loc != want {
		t.Errorf("Location header: got %q want %q", loc, want)
	}
}

// TestRefResolver_UnknownWorkspace asserts 404 for a workspace that doesn't
// exist. The body shape is intentionally generic — no fields hint at
// whether the workspace, the ref, or the access check was the failing
// gate, so callers can't probe workspace existence by diffing responses.
func TestRefResolver_UnknownWorkspace(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/-/r/ghost-workspace/TASK-1", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestRefResolver_UnknownRef asserts 404 for a workspace that exists but
// has no item matching the requested ref.
func TestRefResolver_UnknownRef(t *testing.T) {
	f := newRefResolverFixture(t)
	rr := f.doAs("GET", "/-/r/"+f.ws.Slug+"/DECIS-9999", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing ref, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestRefResolver_MalformedRef asserts 404 for refs that don't match the
// `[A-Za-z][A-Za-z0-9]*-\d+` pattern. The validator runs BEFORE workspace
// resolution, so a malformed ref against a real workspace looks the same
// as a malformed ref against a phantom workspace — no oracle.
func TestRefResolver_MalformedRef(t *testing.T) {
	srv := testServer(t)

	cases := []string{
		"not-a-ref", // missing trailing digits
		"TASK-",     // empty number
		"TASK",      // no separator
		"-5",        // empty prefix
		"123-5",     // digit-only prefix (rejected by leading-letter rule)
		"TASK-5abc", // trailing non-digits in number
		"TASK-1.5",  // non-integer number
		"a/b",       // path traversal candidate
		"%2E%2E%2F", // url-encoded traversal
	}
	for _, ref := range cases {
		path := "/-/r/some-ws/" + ref
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("ref %q: expected 404, got %d", ref, rr.Code)
		}
	}
}

// TestRefResolver_PreSetupBypass: pre-setup mode (UserCount == 0) opens
// every workspace, including resolver-route reads. We exercise the
// fresh-install path here — distinct from the seeded-fixture path used
// by other tests.
func TestRefResolver_PreSetupBypass(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Claude"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name": "Tasks", "slug": "tasks", "prefix": "TASK",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items",
		map[string]interface{}{"title": "T"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	// Pre-setup workspace has owner_id="" — Codex round-2 P1.3 says we
	// must 404 in that case rather than emit a `//slug/...` redirect.
	// Verify the no-broken-redirect contract holds.
	rr = doRequest(srv, "GET", "/-/r/"+ws.Slug+"/"+item.Ref, nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("ownerless pre-setup workspace: expected 404, got %d location=%q",
			rr.Code, rr.Header().Get("Location"))
	}
}

// TestRefResolver_RejectsRefAsCollectionSlug pins round-1 P1.1: the
// reservation prevents `ref` from becoming a collection slug. The round-2
// URL shape /-/r/... no longer collides with a `ref` collection, but
// the reservation stays as defense in depth (and to spare ourselves the
// round-3 risk of someone restoring the older shape and rediscovering
// the collision).
func TestRefResolver_RejectsRefAsCollectionSlug(t *testing.T) {
	f := newRefResolverFixture(t)
	rr := f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections", map[string]interface{}{
		"name": "Ref", "slug": "ref", "prefix": "REF",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	if coll.Slug == "ref" {
		t.Fatalf("collection slug %q would shadow the resolver route — reservation regression", coll.Slug)
	}
	if coll.Slug != "ref-collection" {
		t.Errorf("expected reserved-slug suffix 'ref-collection', got %q", coll.Slug)
	}
}

// TestRefResolver_URLShapeNonOverlap pins round-2 P1.4 (Option B): the
// resolver URL shape `/-/r/...` cannot collide with any user-namespace
// page URL under `/{username}/{workspace}/...` because username + slug
// grammar both require a leading letter. We verify by routing a request
// against the shape `/{username}/{workspace}/ref/{slug}` — the URL a
// pre-existing `ref`-slugged collection's items would have — and
// confirming the resolver handler is NOT invoked. (Without SetWebUI
// installed in tests, that URL hits chi's default 404; the assertion
// here is "the resolver did not intercept it", proved by absence of a
// 302 Location header.)
func TestRefResolver_URLShapeNonOverlap(t *testing.T) {
	f := newRefResolverFixture(t)

	// Hit the page-URL shape directly. The resolver lives at /-/r/...
	// only; a 4-segment path under /{user}/{ws}/ref/... must never
	// match the resolver route. We assert "not a 302" — chi's catch-all
	// 404 is the only acceptable outcome here.
	rr := f.doAs("GET", "/owneruser/"+f.ws.Slug+"/ref/some-item-slug", nil)
	if rr.Code == http.StatusFound {
		t.Errorf("URL /{u}/{ws}/ref/{slug} was intercepted by resolver (got 302 with Location=%q); shape should be inert",
			rr.Header().Get("Location"))
	}
}

// TestCheckItemVisible_TokenizedRoleAllowsNilUser pins round-2 P1.1:
// the legacy workspace-scoped API token path admits requests with
// currentUser == nil and a synthesized role ("editor"). Pre-round-2,
// checkItemVisible's nil-user check fired first and false-404'd these
// callers; the round-2 reorder lets tokenized roles bypass the gate.
func TestCheckItemVisible_TokenizedRoleAllowsNilUser(t *testing.T) {
	srv := testServer(t)

	// A real workspace + collection + item — needed because checkItemVisible
	// dereferences item.CollectionID even when the role short-circuit fires
	// first, and we want the assertion to reflect the "role allows nil
	// user" path, not an unrelated DB miss.
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "o@example.com", Name: "O", Username: "o", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "WS", OwnerID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	item := &models.Item{
		ID: "stub", WorkspaceID: ws.ID, CollectionID: "stub-coll",
	}

	for _, role := range []string{"owner", "editor"} {
		ok, err := srv.checkItemVisible(ws.ID, item, nil, role)
		if err != nil {
			t.Errorf("role=%q: unexpected error: %v", role, err)
		}
		if !ok {
			t.Errorf("role=%q + nil user: expected visible=true (legacy token bypass)", role)
		}
	}

	// Sanity: nil user with no role still rejects (this is the path that
	// distinguishes legitimate legacy tokens from anonymous probes).
	if ok, _ := srv.checkItemVisible(ws.ID, item, nil, ""); ok {
		t.Error("nil user + empty role: expected visible=false")
	}
	if ok, _ := srv.checkItemVisible(ws.ID, item, nil, "guest"); ok {
		t.Error("nil user + guest role: expected visible=false (guest needs grants which require a user)")
	}
}

// TestCheckItemVisible_AuthenticatedEditorWithRestrictedAccess pins
// round-3: the round-2 bypass at the top of checkItemVisible originally
// fired for ANY role in {owner, editor}, which silently disabled the
// per-collection visibility filter for real authenticated editor members
// with collection_access="specific". This test seeds exactly that
// scenario — editor + restricted access to collection A only — and
// asserts visibility on an item in collection B returns false.
//
// Pre-round-3 (round-2 buggy state): returned true. Post-round-3: false.
func TestCheckItemVisible_AuthenticatedEditorWithRestrictedAccess(t *testing.T) {
	srv := testServer(t)

	// Seed a workspace under a separate owner so the editor under test
	// isn't accidentally given owner access.
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Username: "owner",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "WS", OwnerID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}

	// Two collections — A is the editor's allowed set, B is forbidden.
	collA, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Alpha", Slug: "alpha", Prefix: "ALPHA",
	})
	if err != nil {
		t.Fatalf("CreateCollection alpha: %v", err)
	}
	collB, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Beta", Slug: "beta", Prefix: "BETA",
	})
	if err != nil {
		t.Fatalf("CreateCollection beta: %v", err)
	}
	itemB, err := srv.store.CreateItem(ws.ID, collB.ID, models.ItemCreate{
		Title: "Forbidden",
	})
	if err != nil {
		t.Fatalf("CreateItem in beta: %v", err)
	}

	// Real authenticated user with workspace role "editor" and
	// collection_access="specific" listing only collection A.
	editor, err := srv.store.CreateUser(models.UserCreate{
		Email: "editor@example.com", Name: "Editor", Username: "editor",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser editor: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember editor: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, editor.ID, "specific", []string{collA.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	// The regression assertion: editor + "editor" role + item in
	// collection B (NOT in their member_collection_access list) must
	// return false. The round-2 bug returned true here, disabling the
	// per-collection gate for every read AND write path through
	// requireItemVisible / the resolver.
	visible, err := srv.checkItemVisible(ws.ID, itemB, editor, "editor")
	if err != nil {
		t.Fatalf("checkItemVisible: unexpected error: %v", err)
	}
	if visible {
		t.Fatal("authenticated editor with collection_access='specific' (only collA) saw item in collB: round-2 bypass regression")
	}

	// Sanity: same editor on an item in collection A (their allowed
	// collection) DOES see it. Confirms the test fixture is wired right
	// and we're not just observing a generic false everywhere.
	itemA, err := srv.store.CreateItem(ws.ID, collA.ID, models.ItemCreate{
		Title: "Allowed",
	})
	if err != nil {
		t.Fatalf("CreateItem in alpha: %v", err)
	}
	visible, err = srv.checkItemVisible(ws.ID, itemA, editor, "editor")
	if err != nil {
		t.Fatalf("checkItemVisible (allowed): %v", err)
	}
	if !visible {
		t.Error("authenticated editor on an allowed-collection item: expected visible=true")
	}
}

// TestRefResolver_RestrictedMemberWithSystemCollection pins round-2 P1.2:
// a restricted member with conventions/playbooks (system collections)
// access plus an item grant in an UNRELATED collection must be able to
// resolve refs that live in the system collection. Pre-round-2,
// checkItemVisible's item-grants branch only consulted direct
// collection_grants + member_collection_access — not system collections —
// so a restricted member could LIST conventions via the existing API
// (VisibleCollectionIDs includes system collections) but 404'd on
// detail-fetch / ref-resolve.
func TestRefResolver_RestrictedMemberWithSystemCollection(t *testing.T) {
	f := newRefResolverFixture(t)

	// Seed a regular collection A (the member's "specific" access list
	// entry) and grab the workspace's default conventions collection
	// (system collection). Default startup-template collections include
	// conventions; we look it up via the list endpoint to avoid relying
	// on internal seed IDs.
	rr := f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections", map[string]interface{}{
		"name": "Alpha", "slug": "alpha", "prefix": "ALPHA",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection alpha: %d %s", rr.Code, rr.Body.String())
	}
	var collA models.Collection
	parseJSON(t, rr, &collA)

	// Direct store insert for the system collection. CreateWorkspace
	// (used by the fixture) doesn't apply a template, so default
	// system-collection seeding doesn't run; seeding manually via the
	// store keeps the test self-contained and independent of which
	// template defaults are wired up at the time the test runs.
	sysColl, err := f.srv.store.CreateCollection(f.ws.ID, models.CollectionCreate{
		Name: "Conventions", Slug: "conventions", Prefix: "CONV", IsSystem: true,
	})
	if err != nil {
		t.Fatalf("CreateCollection (system): %v", err)
	}

	// Item directly in the system collection. We use the store path
	// because some system collections reject API-level item creation.
	sysItem, err := f.srv.store.CreateItem(f.ws.ID, sysColl.ID, models.ItemCreate{
		Title: "System target",
	})
	if err != nil {
		t.Fatalf("CreateItem in system collection: %v", err)
	}

	// And a regular item in A — the unrelated grant target.
	itemA, err := f.srv.store.CreateItem(f.ws.ID, collA.ID, models.ItemCreate{
		Title: "Alpha item",
	})
	if err != nil {
		t.Fatalf("CreateItem in alpha: %v", err)
	}

	// Restricted member: specific access on A only, plus an item grant
	// on itemA. That grant triggers the item-grants branch of
	// checkItemVisible — the branch that pre-round-2 missed system
	// collections.
	member, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "m@example.com", Name: "M", Username: "m", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, member.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, member.ID, "specific", []string{collA.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, itemA.ID, member.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	tok, err := f.srv.store.CreateAPIToken(member.ID, models.APITokenCreate{
		Name: "member-tok", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Resolver should redirect — system collections must remain visible
	// to restricted members even when the item-grants branch fires.
	rr = f.doAuth(tok.Token, "GET", "/-/r/"+f.ws.Slug+"/"+sysItem.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("restricted member on system collection: expected 302, got %d body=%s",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Location"), "/"+sysColl.Slug+"/") {
		t.Errorf("expected redirect under /%s/, got %q", sysColl.Slug, rr.Header().Get("Location"))
	}
}

// TestRefResolver_RestrictedMemberWithCollectionGrant pins round-1 P1.2:
// a restricted member (collection_access="specific" with only collection A
// in their member_collection_access) PLUS a direct collection grant on
// collection B must be able to resolve refs that live in collection B.
func TestRefResolver_RestrictedMemberWithCollectionGrant(t *testing.T) {
	f := newRefResolverFixture(t)

	// Two collections — A (member can see directly) and B (member sees
	// only via the collection grant we'll attach below).
	rr := f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections", map[string]interface{}{
		"name": "Alpha", "slug": "alpha", "prefix": "ALPHA",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection alpha: %d %s", rr.Code, rr.Body.String())
	}
	var collA models.Collection
	parseJSON(t, rr, &collA)

	rr = f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections", map[string]interface{}{
		"name": "Beta", "slug": "beta", "prefix": "BETA",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection beta: %d %s", rr.Code, rr.Body.String())
	}
	var collB models.Collection
	parseJSON(t, rr, &collB)

	rr = f.doAs("POST", "/api/v1/workspaces/"+f.ws.Slug+"/collections/beta/items",
		map[string]interface{}{"title": "Cross-collection target"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	member, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "m@example.com", Name: "M", Username: "m", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, member.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, member.ID, "specific", []string{collA.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	if _, err := f.srv.store.CreateCollectionGrant(f.ws.ID, collB.ID, member.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}

	tok, err := f.srv.store.CreateAPIToken(member.ID, models.APITokenCreate{
		Name: "member-tok", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	rr = f.doAuth(tok.Token, "GET", "/-/r/"+f.ws.Slug+"/"+item.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("restricted-member-with-grant: expected 302, got %d body=%s",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Location"), "/beta/") {
		t.Errorf("expected redirect under /beta/, got %q", rr.Header().Get("Location"))
	}
}
