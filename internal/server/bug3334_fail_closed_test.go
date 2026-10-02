package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3334: a store error on an access decision fails CLOSED. Faults are real:
// a renamed table makes the store query genuinely fail, except where an earlier
// check runs the same query (see visibleCollectionIDsFault).

// breakTable renames table so every query reading it errors, restoring it at
// cleanup.
func breakTable(t *testing.T, srv *Server, table string) {
	t.Helper()
	if _, err := srv.store.DB().Exec(`ALTER TABLE ` + table + ` RENAME TO ` + table + `_bug3334`); err != nil {
		t.Fatalf("rename %s: %v", table, err)
	}
	t.Cleanup(func() {
		_, _ = srv.store.DB().Exec(`ALTER TABLE ` + table + `_bug3334 RENAME TO ` + table)
	})
}

// initializedWorkspace is a workspace on an instance that has a user, so the
// fresh-install bypass does not apply.
func initializedWorkspace(t *testing.T) (*Server, string) {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	if _, err := srv.store.CreateUser(models.UserCreate{Email: "owner-3334@example.com", Name: "Owner", Password: "pw-test-12345"}); err != nil {
		t.Fatal(err)
	}
	return srv, slug
}

// failUserCount makes every userCount call fail, and counts them: a leg
// asserts its site was reached through the seam, so a site that stops
// calling userCount cannot pass by never meeting the fault.
func failUserCount(t *testing.T, srv *Server) *int {
	t.Helper()
	calls := 0
	srv.userCountFault = func() error {
		calls++
		return errors.New("bug3334: injected user-count fault")
	}
	t.Cleanup(func() { srv.userCountFault = nil })
	return &calls
}

func reachedSeam(t *testing.T, calls *int) {
	t.Helper()
	if *calls == 0 {
		t.Fatal("the site never called userCount, so the fault was never met")
	}
}

var reachedNext = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusTeapot)
})

// (A1) RequireAuth: an anonymous API request during a user-count fault is
// refused, not let through as if the instance had no users.
func TestBUG3334_RequireAuthFailsClosedOnUserCountError(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	breakTable(t, srv, "users")
	rr := httptest.NewRecorder()
	srv.RequireAuth(reachedNext).ServeHTTP(rr, httptest.NewRequest("GET", "/api/v1/workspaces/"+slug+"/collections", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous request during a user-count fault: %d, want 401", rr.Code)
	}
}

// (A2) RequireWorkspaceAccess: the same fault never grants the fresh-install
// owner role.
// The workspace lookup ahead of the check reads the users table too, so the
// fault is on the count alone (userCountFault).
func TestBUG3334_RequireWorkspaceAccessFailsClosedOnUserCountError(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	calls := failUserCount(t, srv)
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+slug+"/collections", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("slug", slug)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	var role string
	rr := httptest.NewRecorder()
	srv.RequireWorkspaceAccess(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role = workspaceRole(r)
		w.WriteHeader(http.StatusTeapot)
	})).ServeHTTP(rr, req)
	reachedSeam(t, calls)
	if rr.Code == http.StatusTeapot {
		t.Fatalf("anonymous request during a user-count fault reached the handler with role %q", role)
	}
}

// (A1+A2) Through the real router: the anonymous read that both holes
// together turned into owner access.
func TestBUG3334_AnonymousReadDuringUserCountFaultIsRefused(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	calls := failUserCount(t, srv)
	if rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections", nil); rr.Code == http.StatusOK {
		t.Fatalf("anonymous collections read during a user-count fault: 200 %s", rr.Body.String())
	}
	reachedSeam(t, calls)
}

// (A3) CSRFProtect: a cookie-borne mutation during a user-count fault is held
// to the CSRF check rather than waved through as a fresh install.
func TestBUG3334_CSRFFailsClosedOnUserCountError(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	breakTable(t, srv, "users")
	req := httptest.NewRequest("POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", strings.NewReader(`{}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: "some-session"})
	rr := httptest.NewRecorder()
	srv.CSRFProtect(reachedNext).ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "csrf_error") {
		t.Fatalf("cookie mutation during a user-count fault: %d %s, want 403 csrf_error", rr.Code, rr.Body.String())
	}
}

// (A4) SSE revalidation: a user-count fault does not short-circuit the
// recheck into "everyone has access"; an anonymous, tokenless stream ends.
func TestBUG3334_SSERecheckFailsClosedOnUserCountError(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatal(err)
	}
	// The count alone: a fault on the users table would first fail the
	// workspace lookup, which deliberately keeps the stream (BUG-3273).
	calls := failUserCount(t, srv)
	if srv.sseSubscriberStillHasAccess(httptest.NewRequest("GET", "/api/v1/events", nil), ws.ID) {
		t.Fatal("an anonymous stream kept access through a user-count fault")
	}
	reachedSeam(t, calls)
}

// (A5) Collab: the same fault does not grant an anonymous socket write access.
func TestBUG3334_CollabFailsClosedOnUserCountError(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	item, err := srv.store.CreateItem(ws.ID, mustCollectionID(t, srv, ws.ID, "tasks"), models.ItemCreate{Title: "Doc", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	calls := failUserCount(t, srv)
	access, err := srv.authorizeCollabAccess(httptest.NewRequest("GET", "/api/v1/collab/"+item.ID, nil), item)
	if err == nil {
		t.Fatalf("anonymous collab access during a user-count fault: granted (canWrite=%v)", access.canWrite)
	}
	reachedSeam(t, calls)
}

func mustCollectionID(t *testing.T, srv *Server, wsID, slug string) string {
	t.Helper()
	c, err := srv.store.GetCollectionBySlug(wsID, slug)
	if err != nil || c == nil {
		t.Fatalf("collection %s: %v", slug, err)
	}
	return c.ID
}

// --- B: visibleCollectionIDs errors ---

type restrictedFixture struct {
	srv          *Server
	slug, wsID   string
	pat          string
	hiddenParent models.Item // in ideas, which the member cannot see
	child        models.Item // in tasks, parented to hiddenParent
	visibleTask  models.Item // in tasks, with a child in ideas
}

// restrictedMember builds a member whose access is tasks only, a task whose
// parent sits in ideas (its title is the leak signal), and a task with a
// hidden child (its progress count is the leak signal).
func restrictedMember(t *testing.T) restrictedFixture {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	post := func(coll, title, fields string) models.Item {
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/"+coll+"/items",
			map[string]any{"title": title, "fields": fields})
		if rr.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", title, rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		return it
	}
	hidden := post("ideas", "Hidden parent title", `{"status":"new"}`)
	child := post("tasks", "Child task", `{"status":"open","parent":"`+hidden.ID+`"}`)
	visible := post("tasks", "Visible task", `{"status":"open"}`)
	post("ideas", "Hidden child", `{"status":"new","parent":"`+visible.ID+`"}`)

	member, err := srv.store.CreateUser(models.UserCreate{Email: "member-3334@example.com", Name: "Member", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, member.ID, "specific", []string{mustCollectionID(t, srv, ws.ID, "tasks")}); err != nil {
		t.Fatal(err)
	}
	tok, err := srv.store.CreateAPIToken(member.ID, models.APITokenCreate{Name: "bug3334", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return restrictedFixture{srv: srv, slug: slug, wsID: ws.ID, pat: tok.Token, hiddenParent: hidden, child: child, visibleTask: visible}
}

func (f restrictedFixture) do(method, path string, body any) *httptest.ResponseRecorder {
	return doRequestWithHeaders(f.srv, method, "/api/v1/workspaces/"+f.slug+path, body,
		map[string]string{"Authorization": "Bearer " + f.pat})
}

var errVisibilityFault = errors.New("bug3334: injected visibility fault")

// faultFrom makes visibleCollectionIDs fail from its nth call on (1-based),
// and counts calls.
func (f restrictedFixture) faultFrom(t *testing.T, n int) *int {
	t.Helper()
	calls := 0
	f.srv.visibleCollectionIDsFault = func() error {
		calls++
		if calls >= n {
			return errVisibilityFault
		}
		return nil
	}
	t.Cleanup(func() { f.srv.visibleCollectionIDsFault = nil })
	return &calls
}

func noLeak(t *testing.T, what string, rr *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(rr.Body.String(), "Hidden parent title") || strings.Contains(rr.Body.String(), "Hidden child") {
		t.Fatalf("%s leaked a hidden item through a visibility fault: %d %s", what, rr.Code, rr.Body.String())
	}
}

// The fixture itself: without a fault, the member is shown neither hidden
// item, so a leak below is the fault's doing.
func TestBUG3334_FixtureHidesBothSignals(t *testing.T) {
	f := restrictedMember(t)
	rr := f.do("GET", "/items/"+f.child.Slug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("member get: %d %s", rr.Code, rr.Body.String())
	}
	noLeak(t, "get without a fault", rr)
	rr = f.do("GET", "/items/"+f.visibleTask.Slug+"/progress", nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"total":0`) {
		t.Fatalf("member progress without a fault: %d %s, want total 0", rr.Code, rr.Body.String())
	}
}

func TestBUG3334_GetItemFailsClosedOnVisibilityError(t *testing.T) {
	f := restrictedMember(t)
	f.faultFrom(t, 1)
	rr := f.do("GET", "/items/"+f.child.Slug, nil)
	noLeak(t, "get", rr)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("get during a visibility fault: %d, want 500", rr.Code)
	}
}

func TestBUG3334_ProgressFailsClosedOnVisibilityError(t *testing.T) {
	f := restrictedMember(t)
	f.faultFrom(t, 1)
	rr := f.do("GET", "/items/"+f.visibleTask.Slug+"/progress", nil)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("progress during a visibility fault: %d %s, want 500 (an unfiltered count counts the hidden child)", rr.Code, rr.Body.String())
	}
}

// Update resolves visibility BEFORE writing: a fault is a 500 with nothing
// written, never a committed write answered with an unfiltered response.
func TestBUG3334_UpdateFailsClosedBeforeWriting(t *testing.T) {
	f := restrictedMember(t)
	f.faultFrom(t, 1)
	rr := f.do("PATCH", "/items/"+f.child.Slug, map[string]any{"title": "Renamed"})
	noLeak(t, "update", rr)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("update during a visibility fault: %d, want 500", rr.Code)
	}
	got, _ := f.srv.store.GetItem(f.child.ID)
	if got == nil || got.Title != "Child task" {
		t.Fatalf("update wrote through a visibility fault: %+v", got)
	}
}

func TestBUG3334_RestoreFailsClosedBeforeWriting(t *testing.T) {
	f := restrictedMember(t)
	if err := f.srv.store.DeleteItem(f.child.ID); err != nil {
		t.Fatal(err)
	}
	f.faultFrom(t, 1)
	rr := f.do("POST", "/items/"+f.child.Slug+"/restore", nil)
	noLeak(t, "restore", rr)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("restore during a visibility fault: %d, want 500", rr.Code)
	}
	got, _ := f.srv.store.ResolveItemIncludeDeleted(f.wsID, f.child.Slug)
	if got == nil || got.DeletedAt == nil {
		t.Fatalf("restore wrote through a visibility fault: %+v", got)
	}
}

// Create and move already resolve visibility before their write, with the
// error checked; the response must be built from THAT result, not from a
// second, unchecked resolution.
func TestBUG3334_CreateResolvesVisibilityOnce(t *testing.T) {
	f := restrictedMember(t)
	calls := f.faultFrom(t, 2)
	rr := f.do("POST", "/collections/tasks/items", map[string]any{"title": "New", "fields": `{"status":"open"}`})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	if *calls != 1 {
		t.Fatalf("create resolved visibility %d times; a second, unchecked resolution is the BUG-3334 site", *calls)
	}
}

func TestBUG3334_MoveBuildsItsResponseFromTheCheckedResolution(t *testing.T) {
	f := restrictedMember(t)
	if err := f.srv.store.SetMemberCollectionAccess(f.wsID, mustUserID(t, f.srv, "member-3334@example.com"), "specific",
		[]string{mustCollectionID(t, f.srv, f.wsID, "tasks"), mustCollectionID(t, f.srv, f.wsID, "docs")}); err != nil {
		t.Fatal(err)
	}
	// The first two resolutions (the target check and the open-children
	// guard) are checked already; the third built the response unchecked.
	calls := f.faultFrom(t, 3)
	rr := f.do("POST", "/items/"+f.child.Slug+"/move", map[string]any{"target_collection": "docs"})
	noLeak(t, "move", rr)
	if rr.Code != http.StatusOK && rr.Code != http.StatusInternalServerError {
		t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
	}
	if rr.Code == http.StatusOK && *calls > 2 {
		t.Fatalf("move answered 200 after %d visibility resolutions; the response came from an unchecked one", *calls)
	}
}

func mustUserID(t *testing.T, srv *Server, email string) string {
	t.Helper()
	u, err := srv.store.GetUserByEmail(email)
	if err != nil || u == nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestVisibleCollectionIDsFaultIsNilInProduction(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Skipf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if New(s).visibleCollectionIDsFault != nil {
		t.Fatal("New left the BUG-3334 test-only fault seam set")
	}
}

func TestUserCountFaultIsNilInProduction(t *testing.T) {
	s, err := store.New(":memory:")
	if err != nil {
		t.Skipf("store.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if New(s).userCountFault != nil {
		t.Fatal("New left the BUG-3334 test-only user-count seam set")
	}
}

// (C) /audit-log is a platform-wide admin surface. An admin's PAT is refused
// with session_required (the BUG-2890 line); the admin's own browser session
// and CLI session are not.
func TestBUG3334_AuditLogRefusesAnAdminPAT(t *testing.T) {
	srv := testServer(t)
	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin-3334@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Audit WS", OwnerID: admin.ID})
	if err != nil {
		t.Fatal(err)
	}
	pat, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{Name: "admin-pat", WorkspaceID: ws.ID}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	sess := loginUser(t, srv, "admin-3334@example.com", "correct-horse-battery-staple")

	rr := doRequestWithBearer(srv, "GET", "/api/v1/audit-log", pat.Token, nil)
	if rr.Code != http.StatusForbidden || errorCode(t, rr) != "session_required" {
		t.Fatalf("admin PAT on /audit-log: %d %s, want 403 session_required", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithBearer(srv, "GET", "/api/v1/audit-log", sess, nil); rr.Code != http.StatusOK {
		t.Fatalf("admin CLI session on /audit-log: %d %s, want 200", rr.Code, rr.Body.String())
	}
	if rr := doRequestWithCookie(srv, "GET", "/api/v1/audit-log", nil, sess); rr.Code != http.StatusOK {
		t.Fatalf("admin browser session on /audit-log: %d %s, want 200", rr.Code, rr.Body.String())
	}
}

// A signed-in member keeps working through a user-count fault at every site
// the fault no longer short-circuits: the fall-through, not a 500 (the
// lead's A1 ruling).
func TestBUG3334_SignedInMemberWorksThroughUserCountFault(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	item, err := srv.store.CreateItem(ws.ID, mustCollectionID(t, srv, ws.ID, "tasks"), models.ItemCreate{Title: "Doc", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	user, tok := loginTestUser(t, srv)
	if err := srv.store.AddWorkspaceMember(ws.ID, user.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	calls := failUserCount(t, srv)
	if rr := doAuthedRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections", nil, tok); rr.Code != http.StatusOK {
		t.Fatalf("signed-in member during a user-count fault: %d %s, want 200", rr.Code, rr.Body.String())
	}
	reachedSeam(t, calls)
	req := httptest.NewRequest("GET", "/api/v1/events", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, user))
	if !srv.sseSubscriberStillHasAccess(req, ws.ID) {
		t.Error("a member's stream was ended by a user-count fault")
	}
	if access, err := srv.authorizeCollabAccess(req, item); err != nil || !access.canWrite {
		t.Errorf("a member's collab access during a user-count fault: %+v, %v", access, err)
	}
}

// SSE visibility: when the fresh user row cannot be read, the cached identity
// (as old as the connection) grants nothing: neither a cookie admin's bypass
// nor a full-access member's unfiltered view (a demotion or a disable would
// not show). Every event is denied until a recompute reads the row.
func TestBUG3334_SSEVisibilityDeniesWithoutAFreshUser(t *testing.T) {
	srv, slug := initializedWorkspace(t)
	ws, _ := srv.store.GetWorkspaceBySlug(slug)
	admin, err := srv.store.CreateUser(models.UserCreate{Email: "sse-admin-3334@example.com", Name: "Admin", Password: "pw-test-12345", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := srv.store.CreateUser(models.UserCreate{Email: "sse-member-3334@example.com", Name: "Member", Password: "pw-test-12345"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, member.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	breakTable(t, srv, "users")
	for name, u := range map[string]*models.User{"cookie admin": admin, "full-access member": member} {
		req := httptest.NewRequest("GET", "/api/v1/events", nil)
		req = req.WithContext(context.WithValue(req.Context(), ctxCurrentUser, u))
		v := srv.computeSSEVisibility(req, ws.ID)
		if v.visibleSlugSet == nil {
			t.Errorf("%s: unfiltered SSE visibility through a user-row fault", name)
			continue
		}
		for _, ev := range []events.Event{{Type: "item_updated", Collection: "tasks", ItemID: "x"}, {Type: "member_added"}} {
			if sseEventVisibleFor(v, u.ID, ev) {
				t.Errorf("%s: %s delivered through a user-row fault", name, ev.Type)
			}
		}
	}
}
