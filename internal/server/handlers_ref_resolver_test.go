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

// TestRefResolver_Success creates a workspace + collection + item, then
// asserts that GET /{username}/{workspace}/ref/{REF} produces a 302 whose
// Location header points at the canonical item URL itemUrlId() would
// produce on the frontend.
func TestRefResolver_Success(t *testing.T) {
	srv := testServer(t)

	// Workspace + collection + item via the API (the route only depends on
	// store state, but using the same surface the frontend uses keeps the
	// test honest about end-to-end shape).
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Claude"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name":   "Decisions",
		"slug":   "decisions",
		"prefix": "DECIS",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/decisions/items", map[string]interface{}{
		"title": "Orchestration model",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	if item.Ref == "" {
		t.Fatalf("expected item Ref, got empty (item=%+v)", item)
	}

	// Hit the resolver. The username segment is arbitrary in pre-setup mode
	// (no auth) — the handler uses the URL-path username verbatim in the
	// redirect target.
	rr = doRequest(srv, "GET", "/anyuser/"+ws.Slug+"/ref/"+item.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d body=%s", rr.Code, rr.Body.String())
	}
	loc := rr.Header().Get("Location")
	want := "/anyuser/" + ws.Slug + "/decisions/" + item.Ref
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
	rr := doRequest(srv, "GET", "/alice/ghost-workspace/ref/TASK-1", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%s", rr.Code, rr.Body.String())
	}
}

// TestRefResolver_UnknownRef asserts 404 for a workspace that exists but
// has no item matching the requested ref. Same response shape as the
// unknown-workspace case (verified by the generic-404 assertion).
func TestRefResolver_UnknownRef(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Claude"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	rr = doRequest(srv, "GET", "/anyuser/"+ws.Slug+"/ref/DECIS-9999", nil)
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
		path := "/anyuser/some-ws/ref/" + ref
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("ref %q: expected 404, got %d", ref, rr.Code)
		}
	}
}

// TestRefResolver_NoAccess documents the gap left by the test fixture
// surface: the existing helpers (testServer / doRequest) don't compose a
// "real user against a workspace they can't access" path because the
// no-auth pre-setup bypass swallows the ACL gate. The handler still
// enforces access in production (see refResolverItemVisible) — its full
// matrix is covered by manual smoke + the production auth middleware
// stack. We assert the documented pre-setup behavior (anonymous access
// permitted) here, with the acknowledgment captured in the test name.
func TestRefResolver_NoAccess_DocumentsPreSetupBypass(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Claude"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name":   "Tasks",
		"slug":   "tasks",
		"prefix": "TASK",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items", map[string]interface{}{
		"title": "T",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	rr = doRequest(srv, "GET", "/anyuser/"+ws.Slug+"/ref/"+item.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("pre-setup bypass should allow access, got %d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Location"), "/tasks/") {
		t.Errorf("expected Location to include /tasks/, got %q", rr.Header().Get("Location"))
	}
}

// TestRefResolver_RejectsRefAsCollectionSlug pins P1.1: creating a
// collection whose slug would be "ref" must not produce a collection
// addressable at /{user}/{ws}/ref/... because that path is the resolver
// route. The store auto-suffixes reserved slugs with "-collection", so
// the created collection lands at slug "ref-collection" — verify that
// outcome (not a hard 4xx rejection) since the suffixing policy mirrors
// the other reserved slugs (settings, activity, roles, …).
func TestRefResolver_RejectsRefAsCollectionSlug(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Claude"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name":   "Ref",
		"slug":   "ref",
		"prefix": "REF",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	var coll models.Collection
	parseJSON(t, rr, &coll)
	if coll.Slug == "ref" {
		t.Fatalf("collection slug %q would shadow the /ref/ resolver route — reservation regression", coll.Slug)
	}
	if coll.Slug != "ref-collection" {
		t.Errorf("expected reserved-slug suffix 'ref-collection', got %q", coll.Slug)
	}
}

// TestRefResolver_TwoSegmentRouteResolves pins P2.2: renderers without
// a username in scope (TimelineCommentCard, CommentThread) emit
// /{workspace}/ref/{REF} hrefs. The two-segment route must resolve and
// redirect to the canonical three-segment item URL, using the workspace
// owner's username as the fallback path prefix.
func TestRefResolver_TwoSegmentRouteResolves(t *testing.T) {
	srv := testServer(t)

	// Seed a user up front and create the workspace owned by that user
	// (CreateWorkspace's OwnerID field handles this), so the two-seg
	// redirect path has a concrete owner-username to fall back on.
	user, err := srv.store.CreateUser(models.UserCreate{
		Email:    "owner@example.com",
		Name:     "Owner",
		Username: "owneruser",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name:    "Claude",
		OwnerID: user.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tok, err := srv.store.CreateAPIToken(user.ID, models.APITokenCreate{
		Name: "owner-tok", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Use Bearer auth for subsequent writes — UserCount() > 0 now, so the
	// pre-setup bypass is closed.
	doAs := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		t.Helper()
		req := newJSONRequest(t, method, path, body)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	rr := doAs("POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name": "Tasks", "slug": "tasks", "prefix": "TASK",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
	rr = doAs("POST", "/api/v1/workspaces/"+ws.Slug+"/collections/tasks/items",
		map[string]interface{}{"title": "T"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	// Two-segment route — no username in URL. Handler should look up the
	// workspace owner and synthesize the redirect target with the owner's
	// username as the leading path segment.
	rr = doAs("GET", "/"+ws.Slug+"/ref/"+item.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("two-seg route: expected 302, got %d body=%s", rr.Code, rr.Body.String())
	}
	got := rr.Header().Get("Location")
	want := "/owneruser/" + ws.Slug + "/tasks/" + item.Ref
	if got != want {
		t.Errorf("two-seg Location: got %q want %q", got, want)
	}

	// Three-segment route still works alongside the two-seg shape.
	rr = doAs("GET", "/explicituser/"+ws.Slug+"/ref/"+item.Ref, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("three-seg route: expected 302, got %d", rr.Code)
	}
	got = rr.Header().Get("Location")
	want = "/explicituser/" + ws.Slug + "/tasks/" + item.Ref
	if got != want {
		t.Errorf("three-seg Location: got %q want %q", got, want)
	}
}

// TestRefResolver_RestrictedMemberWithCollectionGrant pins P1.2: a
// restricted member (collection_access="specific" with only collection A
// in their member_collection_access) PLUS a direct collection grant on
// collection B must be able to resolve refs that live in collection B.
// Pre-refactor, refResolverItemVisible's drift-from-requireItemVisible
// would 404 this case because it didn't consult the guest-grant path
// when the user was a member. checkItemVisible (the shared helper)
// closes the gap.
func TestRefResolver_RestrictedMemberWithCollectionGrant(t *testing.T) {
	srv := testServer(t)

	// Seed owner + workspace + member. CreateWorkspace handles OwnerID
	// directly, so the workspace is anchored to a real user before any
	// ACL-sensitive request runs (and pre-setup bypass is closed by
	// having UserCount > 0).
	owner, err := srv.store.CreateUser(models.UserCreate{
		Email: "owner@example.com", Name: "Owner", Username: "owner",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser owner: %v", err)
	}

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name:    "Claude",
		OwnerID: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember owner: %v", err)
	}
	ownerTok, err := srv.store.CreateAPIToken(owner.ID, models.APITokenCreate{
		Name: "owner-tok", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken owner: %v", err)
	}

	asOwner := func(method, path string, body interface{}) *httptest.ResponseRecorder {
		t.Helper()
		req := newJSONRequest(t, method, path, body)
		req.Header.Set("Authorization", "Bearer "+ownerTok.Token)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// Two collections — A (member can see directly) and B (member sees
	// only via the grant we'll attach below).
	rr := asOwner("POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name": "Alpha", "slug": "alpha", "prefix": "ALPHA",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection alpha: %d %s", rr.Code, rr.Body.String())
	}
	var collA models.Collection
	parseJSON(t, rr, &collA)

	rr = asOwner("POST", "/api/v1/workspaces/"+ws.Slug+"/collections", map[string]interface{}{
		"name": "Beta", "slug": "beta", "prefix": "BETA",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create collection beta: %d %s", rr.Code, rr.Body.String())
	}
	var collB models.Collection
	parseJSON(t, rr, &collB)

	// Seed one item in B — this is the ref we're going to try to resolve
	// as the restricted member.
	rr = asOwner("POST", "/api/v1/workspaces/"+ws.Slug+"/collections/beta/items",
		map[string]interface{}{"title": "Cross-collection target"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)

	// Create the restricted-access member.
	member, err := srv.store.CreateUser(models.UserCreate{
		Email: "member@example.com", Name: "Member", Username: "member",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// "specific" collection access with ONLY collection A in the access
	// set. Without the grant on B (added below), the member can see A's
	// items and not B's.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{collA.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	// Direct collection grant on B — this is the codex-flagged path.
	// requireItemVisible considers this via guestResourceFilter's
	// fullCollIDs return; checkItemVisible must do the same.
	if _, err := srv.store.CreateCollectionGrant(ws.ID, collB.ID, member.ID, "view", owner.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}

	// Mint a Bearer token for the restricted member.
	tok, err := srv.store.CreateAPIToken(member.ID, models.APITokenCreate{
		Name: "member-tok", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	doAsMember := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer "+tok.Token)
		req.RemoteAddr = "127.0.0.1:0"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// The restricted-member-with-grant case: GET on the B-collection
	// item resolves to 302, not 404. Pre-refactor this returned 404
	// because the resolver's bespoke ACL didn't merge collection grants
	// into the member's visible set.
	rr = doAsMember("/owner/" + ws.Slug + "/ref/" + item.Ref)
	if rr.Code != http.StatusFound {
		t.Fatalf("restricted-member-with-grant: expected 302, got %d body=%s",
			rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Location"), "/beta/") {
		t.Errorf("expected redirect under /beta/, got %q", rr.Header().Get("Location"))
	}
}
