package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// contextWithWorkspaceRoleForTest / contextWithResolvedWorkspaceIDForTest
// stash the two values RequireWorkspaceAccess populates for the REQUEST's
// workspace. The cross-workspace helper must ignore both — these exist so
// the tests can poison the context the way a real handler would see it.
func contextWithWorkspaceRoleForTest(ctx context.Context, role string) context.Context {
	return context.WithValue(ctx, ctxWorkspaceRole, role)
}

func contextWithResolvedWorkspaceIDForTest(ctx context.Context, wsID string) context.Context {
	return context.WithValue(ctx, ctxResolvedWorkspaceID, wsID)
}

// Tests for the cross-workspace authorization helper (PLAN-2357 /
// TASK-2358, DR-10). Every case asserts a DENIAL as well as the happy
// path — the helper's whole job is the denial side.

type crossWSFixture struct {
	t   *testing.T
	srv *Server

	// Workspace A is the "request's own" workspace; workspace B is the
	// second workspace every check in this file targets.
	wsA *models.Workspace
	wsB *models.Workspace

	// Collections/items in B.
	collB     *models.Collection
	itemB     *models.Item
	hiddenB   *models.Collection
	hiddenIB  *models.Item
	collA     *models.Collection
	itemA     *models.Item
	ownerBoth *models.User
}

func newCrossWSFixture(t *testing.T) *crossWSFixture {
	t.Helper()
	srv := testServer(t)

	owner := mustUser(t, srv, "owner@example.com", "ownerx", "")
	wsA := mustWorkspace(t, srv, "Alpha", owner.ID)
	wsB := mustWorkspace(t, srv, "Bravo", owner.ID)

	collA := mustCollection(t, srv, wsA.ID, "Tasks A")
	itemA := mustItem(t, srv, wsA.ID, collA.ID, "Item A")

	collB := mustCollection(t, srv, wsB.ID, "Tasks B")
	itemB := mustItem(t, srv, wsB.ID, collB.ID, "Item B")
	hiddenB := mustCollection(t, srv, wsB.ID, "Secrets B")
	hiddenIB := mustItem(t, srv, wsB.ID, hiddenB.ID, "Secret B")

	return &crossWSFixture{
		t: t, srv: srv,
		wsA: wsA, wsB: wsB,
		collA: collA, itemA: itemA,
		collB: collB, itemB: itemB,
		hiddenB: hiddenB, hiddenIB: hiddenIB,
		ownerBoth: owner,
	}
}

func mustUser(t *testing.T, srv *Server, email, username, role string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email: email, Name: username, Username: username, Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	if role != "" {
		if err := srv.store.SetUserRole(u.ID, role); err != nil {
			t.Fatalf("SetUserRole(%s, %s): %v", email, role, err)
		}
		u.Role = role
	}
	return u
}

func mustWorkspace(t *testing.T, srv *Server, name, ownerID string) *models.Workspace {
	t.Helper()
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: ownerID})
	if err != nil {
		t.Fatalf("CreateWorkspace(%s): %v", name, err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, ownerID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember(%s): %v", name, err)
	}
	return ws
}

func mustCollection(t *testing.T, srv *Server, wsID, name string) *models.Collection {
	t.Helper()
	c, err := srv.store.CreateCollection(wsID, models.CollectionCreate{Name: name})
	if err != nil {
		t.Fatalf("CreateCollection(%s): %v", name, err)
	}
	return c
}

func mustItem(t *testing.T, srv *Server, wsID, collID, title string) *models.Item {
	t.Helper()
	it, err := srv.store.CreateItem(wsID, collID, models.ItemCreate{Title: title})
	if err != nil {
		t.Fatalf("CreateItem(%s): %v", title, err)
	}
	return it
}

func (f *crossWSFixture) member(email, username, wsRole string, ws *models.Workspace) *models.User {
	f.t.Helper()
	u := mustUser(f.t, f.srv, email, username, "")
	if err := f.srv.store.AddWorkspaceMember(ws.ID, u.ID, wsRole); err != nil {
		f.t.Fatalf("AddWorkspaceMember: %v", err)
	}
	return u
}

// reqOpts describes the auth surface the synthetic request presents.
type reqOpts struct {
	bearer bool
	// allowed is the OAuth/MCP consent allow-list. nil means "no
	// allow-list stashed at all" (PAT / cookie), which is a distinct
	// state from an empty slice.
	allowed    []string
	setAllowed bool
	// wsRoleCtx simulates RequireWorkspaceAccess having populated the
	// role for workspace A — the poisoned context value the helper must
	// ignore.
	wsRoleCtx string
	wsIDCtx   string
	// tokenWorkspaceID simulates a legacy workspace-scoped API token.
	tokenWorkspaceID string
	noUser           bool
}

func (f *crossWSFixture) request(user *models.User, o reqOpts) *http.Request {
	f.t.Helper()
	r := httptest.NewRequest("POST", "/api/v1/workspaces/"+f.wsA.Slug+"/items", nil)
	ctx := r.Context()
	if !o.noUser && user != nil {
		ctx = WithCurrentUser(ctx, user)
	}
	if o.setAllowed {
		ctx = WithTokenAllowedWorkspaces(ctx, o.allowed)
	}
	if o.tokenWorkspaceID != "" {
		ctx = WithTokenWorkspaceID(ctx, o.tokenWorkspaceID)
	}
	if o.wsRoleCtx != "" {
		ctx = contextWithWorkspaceRoleForTest(ctx, o.wsRoleCtx)
	}
	if o.wsIDCtx != "" {
		ctx = contextWithResolvedWorkspaceIDForTest(ctx, o.wsIDCtx)
	}
	r = r.WithContext(ctx)
	if o.bearer {
		r.Header.Set("Authorization", "Bearer test-token")
	}
	return r
}

func assertDenied(t *testing.T, got CrossWorkspaceAccess, want CrossWorkspaceDenialReason, label string) {
	t.Helper()
	if got.Allowed {
		t.Fatalf("%s: expected denial (%s), got ALLOWED (role=%q)", label, want, got.Role)
	}
	if got.Reason != want {
		t.Fatalf("%s: expected reason %q, got %q (err=%v)", label, want, got.Reason, got.Err)
	}
}

func assertAllowed(t *testing.T, got CrossWorkspaceAccess, label string) {
	t.Helper()
	if !got.Allowed {
		t.Fatalf("%s: expected allowed, got denial %q (err=%v)", label, got.Reason, got.Err)
	}
	if got.Reason != CrossWorkspaceAllowed {
		t.Fatalf("%s: allowed verdict carried reason %q", label, got.Reason)
	}
}

// --- Membership --------------------------------------------------------

// TestCrossWorkspace_EditorInAOnly is the headline case: a caller who is
// an editor in the REQUEST's workspace and a stranger to the target gets
// nothing there, even with the request context claiming "editor".
func TestCrossWorkspace_EditorInAOnly(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("a-editor@example.com", "aeditor", "editor", f.wsA)
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	// A non-admin stranger doesn't even get B to resolve — resolveWorkspace
	// is ACL-scoped for them, so absence and forbidden-ness are conflated
	// at the earliest possible point. Denied either way.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceWorkspaceNotFound, "edit into B collection")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceWorkspaceNotFound, "edit B item")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceWorkspaceNotFound, "read B item")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()),
		CrossWorkspaceWorkspaceNotFound, "workspace-only read of B")
	// Addressing B by UUID skips resolveWorkspace's ACL scoping (it is a
	// direct GetWorkspaceByID), so this is the path where the ROLE check is
	// the only thing standing between the caller and workspace B.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.ID, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceNoWorkspaceAccess, "edit B item addressed by UUID")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.ID, CrossWorkspaceWorkspaceOnlyScope()),
		CrossWorkspaceNoWorkspaceAccess, "workspace-only read of B by UUID")

	// Sanity: the same caller IS authorized in their own workspace, so
	// the denials above aren't an artifact of a broken fixture.
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsA.Slug, CrossWorkspaceCollectionScope(f.collA.ID)),
		"edit into A collection")
}

// TestCrossWorkspace_RequireEditPermissionIsWrongForSecondWorkspace pins
// the reason this helper exists. requireEditPermission answers "yes" for
// workspace B purely because the request context says the caller is an
// editor in A. If this test ever starts failing because
// requireEditPermission got fixed, delete it — but do NOT start calling
// requireEditPermission cross-workspace.
func TestCrossWorkspace_RequireEditPermissionIsWrongForSecondWorkspace(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("trap@example.com", "trapuser", "editor", f.wsA)
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	rec := httptest.NewRecorder()
	if !f.srv.requireEditPermission(rec, r, f.wsB.ID, f.itemB.ID, f.collB.ID) {
		t.Skip("requireEditPermission no longer leaks the request workspace's role; " +
			"the cross-workspace helper is still the only supported path")
	}
	// The trap fired, as documented. The helper must disagree — checked on
	// the UUID form, which is the shape requireEditPermission takes and the
	// one that bypasses resolveWorkspace's ACL scoping.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.ID, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceNoWorkspaceAccess, "helper disagrees with requireEditPermission")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceWorkspaceNotFound, "helper disagrees with requireEditPermission (slug form)")
}

func TestCrossWorkspace_EditorInBoth(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("both@example.com", "bothuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	got := f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID))
	assertAllowed(t, got, "edit into B collection")
	if got.Role != "editor" {
		t.Errorf("expected role editor in B, got %q", got.Role)
	}
	if got.WorkspaceID() != f.wsB.ID || got.WorkspaceSlug() != f.wsB.Slug {
		t.Errorf("verdict points at the wrong workspace: %s/%s", got.WorkspaceID(), got.WorkspaceSlug())
	}
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"edit B item")
}

func TestCrossWorkspace_ViewerInB(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("view@example.com", "viewuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"viewer reads B item")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceInsufficientPermission, "viewer edits B item")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceInsufficientPermission, "viewer creates into B collection")
}

// --- Token consent allow-list -----------------------------------------

// TestCrossWorkspace_TokenAllowlist is the regression test that matters
// most: a naive implementation passes every membership case and fails
// this one, because tokenAllowedWorkspaceMatches runs in exactly one
// place (RequireWorkspaceAccess) and a second workspace never goes
// through it.
func TestCrossWorkspace_TokenAllowlist(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("tok@example.com", "tokuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}

	cases := []struct {
		name    string
		opts    reqOpts
		allowed bool
		reason  CrossWorkspaceDenialReason
	}{
		{
			name:   "consented to A only, member of B",
			opts:   reqOpts{bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug}},
			reason: CrossWorkspaceTokenNotAllowed,
		},
		{
			name:    "consented to A and B",
			opts:    reqOpts{bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug, f.wsB.Slug}},
			allowed: true,
		},
		{
			name:    "wildcard consent",
			opts:    reqOpts{bearer: true, setAllowed: true, allowed: []string{"*"}},
			allowed: true,
		},
		{
			name:    "no allow-list stashed (PAT)",
			opts:    reqOpts{bearer: true},
			allowed: true,
		},
		{
			name:   "empty allow-list fails closed",
			opts:   reqOpts{bearer: true, setAllowed: true, allowed: []string{}},
			reason: CrossWorkspaceTokenNotAllowed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.wsRoleCtx = "editor"
			opts.wsIDCtx = f.wsA.ID
			r := f.request(u, opts)
			for _, scope := range map[string]CrossWorkspaceScope{
				"collection": CrossWorkspaceCollectionScope(f.collB.ID),
				"item":       CrossWorkspaceItemScope(f.itemB),
				"workspace":  CrossWorkspaceWorkspaceOnlyScope(),
			} {
				got := f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, scope)
				if tc.allowed {
					assertAllowed(t, got, tc.name)
				} else {
					assertDenied(t, got, tc.reason, tc.name)
				}
				got = f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, scope)
				if tc.allowed {
					assertAllowed(t, got, tc.name+" (read)")
				} else {
					assertDenied(t, got, tc.reason, tc.name+" (read)")
				}
			}
		})
	}
}

// The allow-list is tested against the CANONICAL slug, so addressing the
// workspace by UUID must not bypass it.
func TestCrossWorkspace_TokenAllowlistAppliesToUUIDAddressing(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("uuid@example.com", "uuiduser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug},
		wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.ID, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceTokenNotAllowed, "UUID-addressed B with A-only consent")
}

// A stranger to B must never receive the distinct "token not authorized"
// message, since that message confirms the workspace exists. The role
// check is ranked ahead of the allow-list check precisely for this.
func TestCrossWorkspace_TokenDenialNeverConfirmsExistenceToStranger(t *testing.T) {
	f := newCrossWSFixture(t)
	admin := mustUser(t, f.srv, "admin@example.com", "adminuser", "admin")
	r := f.request(admin, reqOpts{bearer: true, setAllowed: true, allowed: []string{f.wsA.Slug}})

	// Bearer-borne admin is a stranger to B (membership-only stance) and
	// resolveWorkspace does a GLOBAL slug lookup for admins — so the
	// allow-list denial would leak existence if it were reported first.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()),
		CrossWorkspaceNoWorkspaceAccess, "bearer admin stranger to B")
}

// --- Bearer-vs-cookie admin split (BUG-1616/1617) ---------------------

func TestCrossWorkspace_AdminBearerVsCookieSplit(t *testing.T) {
	f := newCrossWSFixture(t)
	admin := mustUser(t, f.srv, "padmin@example.com", "padmin", "admin")

	cookieReq := f.request(admin, reqOpts{})
	bearerReq := f.request(admin, reqOpts{bearer: true})

	// Cookie session: platform admin gets owner-equivalent access to a
	// workspace they never joined.
	got := f.srv.AuthorizeCrossWorkspaceEdit(cookieReq, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID))
	assertAllowed(t, got, "cookie admin edits into B")
	if got.Role != "owner" {
		t.Errorf("cookie admin: expected owner-equivalent role, got %q", got.Role)
	}
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(cookieReq, f.wsB.Slug, CrossWorkspaceItemScope(f.hiddenIB)),
		"cookie admin reads a B item")

	// Bearer: the same admin is a stranger to B.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(bearerReq, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceNoWorkspaceAccess, "bearer admin edits into B")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(bearerReq, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceNoWorkspaceAccess, "bearer admin reads B item")
}

// A bearer-borne admin holding a stray grant in B still gets nothing:
// the membership-only stance skips the guest-grants fallback entirely.
func TestCrossWorkspace_BearerAdminWithStrayGrantStillDenied(t *testing.T) {
	f := newCrossWSFixture(t)
	admin := mustUser(t, f.srv, "strayadmin@example.com", "strayadmin", "admin")
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, admin.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	bearerReq := f.request(admin, reqOpts{bearer: true})

	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(bearerReq, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceNoWorkspaceAccess, "bearer admin with stray grant")

	// A NON-admin with the same grant is a legitimate guest.
	guest := f.member("guest@example.com", "guestuser", "editor", f.wsA)
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, guest.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant guest: %v", err)
	}
	gReq := f.request(guest, reqOpts{bearer: true, wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	got := f.srv.AuthorizeCrossWorkspaceEdit(gReq, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB))
	assertAllowed(t, got, "guest with item grant edits the granted item")
	if got.Role != "guest" {
		t.Errorf("expected guest role, got %q", got.Role)
	}
}

// --- Soft-deleted target ----------------------------------------------

func TestCrossWorkspace_SoftDeletedWorkspaceDenied(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("sd@example.com", "sduser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		"pre-delete baseline")

	if err := f.srv.store.DeleteWorkspace(f.wsB.Slug); err != nil {
		t.Fatalf("DeleteWorkspace: %v", err)
	}

	for _, addr := range []string{f.wsB.Slug, f.wsB.ID} {
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, addr, CrossWorkspaceCollectionScope(f.collB.ID)),
			CrossWorkspaceWorkspaceNotFound, "soft-deleted B via "+addr)
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, addr, CrossWorkspaceItemScope(f.itemB)),
			CrossWorkspaceWorkspaceNotFound, "soft-deleted B read via "+addr)
	}

	// Even a cookie-session platform admin can't reach a soft-deleted
	// workspace — resolution filters it before any role is computed.
	admin := mustUser(t, f.srv, "sdadmin@example.com", "sdadmin", "admin")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(f.request(admin, reqOpts{}), f.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()),
		CrossWorkspaceWorkspaceNotFound, "cookie admin vs soft-deleted B")
}

// --- Restricted members (collection_access = specific) ----------------

// DR-10a: a restricted editor in B must not be cleared to create into a
// collection hidden from them — including the case where their only
// claim on that collection is an item-level grant inside it, which the
// nav-lenient VisibleCollectionIDs set (and hence ResolveUserPermission's
// own gate) would wave through.
func TestCrossWorkspace_RestrictedEditorHiddenCollection(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("restricted@example.com", "restricteduser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.wsB.ID, u.ID, "specific", []string{f.collB.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		"restricted editor, permitted collection")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.hiddenB.ID)),
		CrossWorkspaceCollectionNotVisible, "restricted editor, hidden collection")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.hiddenB.ID)),
		CrossWorkspaceCollectionNotVisible, "restricted editor, hidden collection schema disclosure")

	// DR-10b: reading OUT of the hidden collection is the exfiltration
	// direction and must refuse too.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.hiddenIB)),
		CrossWorkspaceItemNotVisible, "restricted editor, hidden item")

	// Now give them an item grant INSIDE the hidden collection. That makes
	// the collection nav-visible, and ResolveUserPermission's restricted-
	// member gate consults that same lenient set — so this is exactly the
	// DR-10a escalation. The granted item becomes readable; the collection
	// as a whole must not.
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.hiddenIB.ID, u.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.hiddenIB)),
		"restricted editor reads the specifically granted item")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.hiddenB.ID)),
		CrossWorkspaceCollectionNotVisible, "restricted editor, item-grant-only collection")

	// And a sibling item in that collection, which the grant does not
	// cover, stays invisible.
	sibling := mustItem(t, f.srv, f.wsB.ID, f.hiddenB.ID, "Secret Sibling")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(sibling)),
		CrossWorkspaceItemNotVisible, "restricted editor, ungranted sibling")

	// The workspace-level-only scope PASSES throughout — which is exactly
	// why it is never sufficient on its own.
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()),
		"workspace-only scope is not a substitute for the scoped check")
}

// --- Guests holding only item grants ----------------------------------

func TestCrossWorkspace_GuestItemGrantOnly(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("g@example.com", "guser", "editor", f.wsA)
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, u.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"guest edits the granted item")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.hiddenIB)),
		CrossWorkspaceItemNotVisible, "guest reads an ungranted item")
	// The grant makes itemB's collection nav-visible, but a guest whose
	// only claim is one item must not be cleared to create into it.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceCollectionNotVisible, "guest creates into the item-grant collection")

	// A view-only item grant reads but does not write.
	viewer := f.member("gv@example.com", "gvuser", "editor", f.wsA)
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, viewer.ID, "view", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant view: %v", err)
	}
	vr := f.request(viewer, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(vr, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"view-grant guest reads")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(vr, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceInsufficientPermission, "view-grant guest writes")
}

// A guest with a full COLLECTION grant may create into that collection.
func TestCrossWorkspace_GuestCollectionGrant(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("gc@example.com", "gcuser", "editor", f.wsA)
	if _, err := f.srv.store.CreateCollectionGrant(f.wsB.ID, f.collB.ID, u.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateCollectionGrant: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		"collection-grant guest creates into the granted collection")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.hiddenB.ID)),
		CrossWorkspaceCollectionNotVisible, "collection-grant guest creates into another collection")
}

// --- Scope hygiene -----------------------------------------------------

func TestCrossWorkspace_ScopeMismatch(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("sm@example.com", "smuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	// An item from A, authorized against B (which the caller owns).
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemA)),
		CrossWorkspaceScopeMismatch, "item from A against B")
	// A collection from A, likewise.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collA.ID)),
		CrossWorkspaceScopeMismatch, "collection from A against B")

	// Codex round 1 P1: a sparse / hand-built item must NOT fail open.
	// An unset WorkspaceID once meant "can't tell, assume fine", which
	// let an item from a third workspace be laundered through a
	// workspace the caller owns. Same for an unset CollectionID, which
	// makes both isCollectionVisible and ResolveUserPermission degrade
	// to a workspace-level answer.
	sparse := []struct {
		name string
		item *models.Item
	}{
		{"no workspace id", &models.Item{ID: f.itemA.ID, CollectionID: f.collA.ID}},
		{"no collection id", &models.Item{ID: f.itemB.ID, WorkspaceID: f.wsB.ID}},
		{"no item id", &models.Item{WorkspaceID: f.wsB.ID, CollectionID: f.collB.ID}},
		{"empty item", &models.Item{}},
		// Claims workspace B but points its collection at A. Only the
		// PARENT-COLLECTION workspace check catches this one — the
		// item's own WorkspaceID matches the target.
		{"collection from another workspace", &models.Item{ID: f.itemB.ID, WorkspaceID: f.wsB.ID, CollectionID: f.collA.ID}},
	}
	for _, tc := range sparse {
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(tc.item)),
			CrossWorkspaceScopeMismatch, "sparse item scope: "+tc.name)
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(tc.item)),
			CrossWorkspaceScopeMismatch, "sparse item scope (read): "+tc.name)
	}
}

func TestCrossWorkspace_InvalidAndMissingInputs(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("iv@example.com", "ivuser", "owner", f.wsB)
	r := f.request(u, reqOpts{})

	// The zero scope names nothing and must never degrade to a
	// workspace-level check.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceScope{}),
		CrossWorkspaceInvalidScope, "zero scope")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceScope{}),
		CrossWorkspaceInvalidScope, "zero scope (edit)")
	// A nil item is not a scope either.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(nil)),
		CrossWorkspaceInvalidScope, "nil item scope")
	// Empty target.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, "", CrossWorkspaceWorkspaceOnlyScope()),
		CrossWorkspaceWorkspaceNotFound, "empty target")
	// Nonexistent target.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, "no-such-workspace", CrossWorkspaceWorkspaceOnlyScope()),
		CrossWorkspaceWorkspaceNotFound, "nonexistent target")
	// Nonexistent / soft-deleted collection in a workspace the caller owns.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope("00000000-0000-0000-0000-000000000000")),
		CrossWorkspaceCollectionNotVisible, "nonexistent collection")
}

// --- Grant / role precedence (Codex round 5) --------------------------

// Grants widen, never narrow. requireEditPermission lets an
// editor/owner base role win outright and only consults grants when the
// role is insufficient; the cross-workspace helper must do the same, or
// an incidental view grant silently demotes a member the front door
// would allow.
func TestCrossWorkspace_GrantsWidenNeverNarrow(t *testing.T) {
	f := newCrossWSFixture(t)

	// Viewer in B plus an edit grant → the grant wins.
	viewer := f.member("vg@example.com", "vguser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	vr := f.request(viewer, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(vr, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceInsufficientPermission, "viewer before the grant")
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, viewer.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(vr, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"viewer plus an edit grant")

	// Editor/owner in B plus a VIEW grant → still allowed. Resolving
	// grants ahead of membership (ResolveUserPermission's own order)
	// would demote them here.
	for _, tc := range []struct{ email, name, role string }{
		{"eg@example.com", "eguser", "editor"},
		{"og@example.com", "oguser", "owner"},
	} {
		u := f.member(tc.email, tc.name, "editor", f.wsA)
		if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, tc.role); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
		if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, u.ID, "view", f.ownerBoth.ID); err != nil {
			t.Fatalf("CreateItemGrant: %v", err)
		}
		r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
		assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
			tc.role+" in B is not demoted by a view grant")
	}
}

// --- System collections ------------------------------------------------

// Restricted members keep access to system collections (conventions,
// playbooks). That allowance survives the item-grant filtering branch,
// which is the shape that used to 404 them.
func TestCrossWorkspace_RestrictedMemberSystemCollection(t *testing.T) {
	f := newCrossWSFixture(t)
	sysColl, err := f.srv.store.CreateCollection(f.wsB.ID, models.CollectionCreate{Name: "Conventions B", IsSystem: true})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	sysItem := mustItem(t, f.srv, f.wsB.ID, sysColl.ID, "Convention B")

	u := f.member("sys@example.com", "sysuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Restricted to collB only — the system collection is NOT listed.
	if err := f.srv.store.SetMemberCollectionAccess(f.wsB.ID, u.ID, "specific", []string{f.collB.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(sysItem)),
		"restricted member reads a system-collection item")
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(sysColl.ID)),
		"restricted member creates into a system collection")
	// The non-system collection they were NOT granted stays hidden — the
	// system allowance must not blanket the workspace.
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.hiddenIB)),
		CrossWorkspaceItemNotVisible, "system allowance is not a blanket pass")

	// Activate the item-grant filtering branch with an unrelated grant.
	// The system allowance must survive it.
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.hiddenIB.ID, u.ID, "view", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(sysItem)),
		"system-collection item with item-grant filtering active")
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(sysColl.ID)),
		"system collection scope with item-grant filtering active")
}

// --- Grants on soft-deleted resources ----------------------------------

// A grant on a soft-deleted item is not access. UserHasGrantsInWorkspace
// filters archived resources, so the "guest" role must not be
// synthesized from one.
func TestCrossWorkspace_GrantOnArchivedItemGivesNoRole(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("ag@example.com", "aguser", "editor", f.wsA)
	if _, err := f.srv.store.CreateItemGrant(f.wsB.ID, f.itemB.ID, u.ID, "edit", f.ownerBoth.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.ID, CrossWorkspaceItemScope(f.itemB)),
		"live grant baseline")

	if err := f.srv.store.DeleteItem(f.itemB.ID); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}
	// Addressed by UUID so resolveWorkspace's ACL scoping isn't what
	// produces the denial — the role derivation is.
	got := f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.ID, CrossWorkspaceItemScope(f.itemB))
	assertDenied(t, got, CrossWorkspaceNoWorkspaceAccess, "grant on an archived item")
	if got.Role != "" {
		t.Errorf("expected no role from an archived-item grant, got %q", got.Role)
	}
}

// --- Archived collections (Codex round 2 P1) --------------------------

// Soft-deleting a collection leaves its items in place, and neither
// GetItem nor VisibleCollectionIDs filters on the collection's
// deleted_at — so nothing downstream of the helper would notice. Both
// scopes must refuse.
func TestCrossWorkspace_ArchivedCollectionDenied(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("ac@example.com", "acuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"pre-archive baseline (item)")
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		"pre-archive baseline (collection)")

	if err := f.srv.store.DeleteCollection(f.collB.ID, ""); err != nil {
		t.Fatalf("DeleteCollection: %v", err)
	}
	// The item row is untouched by the collection archive, which is
	// exactly why the helper has to look.
	if live, err := f.srv.store.GetItem(f.itemB.ID); err != nil || live == nil {
		t.Fatalf("expected the item to survive its collection's archive (item=%v err=%v)", live, err)
	}

	for label, scope := range map[string]CrossWorkspaceScope{
		"item":       CrossWorkspaceItemScope(f.itemB),
		"collection": CrossWorkspaceCollectionScope(f.collB.ID),
	} {
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, scope),
			CrossWorkspaceCollectionNotVisible, "archived collection, edit, "+label)
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, scope),
			CrossWorkspaceCollectionNotVisible, "archived collection, read, "+label)
	}

	// A cookie-session platform admin doesn't get a pass either — the
	// collection check runs before any per-caller visibility filter.
	admin := mustUser(t, f.srv, "acadmin@example.com", "acadmin", "admin")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(f.request(admin, reqOpts{}), f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceCollectionNotVisible, "cookie admin vs archived collection")
}

// --- Never grant more than the front door (Codex round 2 P1) ----------

// Ownership and membership are separate persisted state.
// RequireWorkspaceAccess has no ws.OwnerID exception — it requires the
// membership row — so a workspace owner with no member row is refused
// at /api/v1/workspaces/{slug} and must be refused sideways too. The
// ref resolver's resolverWorkspaceRole DOES take that shortcut
// (BUG-1618, a deliberate widening for its cookie-only redirect route);
// the cross-workspace helper deliberately does not reuse it.
func TestCrossWorkspace_OwnerWithoutMembershipRowMatchesFrontDoor(t *testing.T) {
	f := newCrossWSFixture(t)
	owner := mustUser(t, f.srv, "solo@example.com", "solouser", "")
	ws, err := f.srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Unjoined", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll := mustCollection(t, f.srv, ws.ID, "Tasks")
	if m, mErr := f.srv.store.GetWorkspaceMember(ws.ID, owner.ID); mErr != nil || m != nil {
		t.Fatalf("fixture broken: expected no membership row (m=%v err=%v)", m, mErr)
	}

	for _, tc := range []struct {
		name string
		opts reqOpts
	}{
		{"bearer", reqOpts{bearer: true}},
		{"cookie", reqOpts{}},
	} {
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(f.request(owner, tc.opts), ws.Slug, CrossWorkspaceCollectionScope(coll.ID)),
			CrossWorkspaceNoWorkspaceAccess, "member-less owner via "+tc.name)
		assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(f.request(owner, tc.opts), ws.Slug, CrossWorkspaceWorkspaceOnlyScope()),
			CrossWorkspaceNoWorkspaceAccess, "member-less owner read via "+tc.name)
	}

	// Adding the membership row restores access on both surfaces.
	if err := f.srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(f.request(owner, reqOpts{bearer: true}), ws.Slug, CrossWorkspaceCollectionScope(coll.ID)),
		"owner with membership row, bearer")
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(f.request(owner, reqOpts{}), ws.Slug, CrossWorkspaceCollectionScope(coll.ID)),
		"owner with membership row, cookie")
}

// --- Tokenless surfaces -----------------------------------------------

// A legacy workspace-scoped API token is an editor in ITS workspace and a
// stranger everywhere else.
func TestCrossWorkspace_LegacyWorkspaceScopedToken(t *testing.T) {
	f := newCrossWSFixture(t)
	r := f.request(nil, reqOpts{noUser: true, bearer: true, tokenWorkspaceID: f.wsA.ID,
		wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceNoWorkspaceAccess, "legacy token reaching B")
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsA.Slug, CrossWorkspaceCollectionScope(f.collA.ID)),
		"legacy token in its own workspace")

	// An allow-list still applies on top of the pinned workspace — the
	// two gates are independent and both must pass.
	excluded := f.request(nil, reqOpts{noUser: true, bearer: true, tokenWorkspaceID: f.wsA.ID,
		setAllowed: true, allowed: []string{f.wsB.Slug}, wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(excluded, f.wsA.Slug, CrossWorkspaceCollectionScope(f.collA.ID)),
		CrossWorkspaceTokenNotAllowed, "legacy token, own workspace excluded by the allow-list")
	included := f.request(nil, reqOpts{noUser: true, bearer: true, tokenWorkspaceID: f.wsA.ID,
		setAllowed: true, allowed: []string{f.wsA.Slug}, wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(included, f.wsA.Slug, CrossWorkspaceCollectionScope(f.collA.ID)),
		"legacy token, own workspace on the allow-list")
}

// An anonymous caller on an initialized instance gets nothing.
func TestCrossWorkspace_AnonymousDenied(t *testing.T) {
	f := newCrossWSFixture(t)
	r := f.request(nil, reqOpts{noUser: true})
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		CrossWorkspaceNoWorkspaceAccess, "anonymous read")
	assertDenied(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceCollectionScope(f.collB.ID)),
		CrossWorkspaceNoWorkspaceAccess, "anonymous edit")
}

// Fresh install (no users yet): the instance is open, mirroring
// RequireWorkspaceAccess's UserCount == 0 bypass.
func TestCrossWorkspace_FreshInstall(t *testing.T) {
	srv := testServer(t)
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Solo"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll := mustCollection(t, srv, ws.ID, "Tasks")
	r := httptest.NewRequest("GET", "/api/v1/workspaces/other/items", nil)

	got := srv.AuthorizeCrossWorkspaceEdit(r, ws.Slug, CrossWorkspaceCollectionScope(coll.ID))
	assertAllowed(t, got, "fresh install")
	if got.Role != "owner" {
		t.Errorf("fresh install: expected owner, got %q", got.Role)
	}
}

// --- Disclosure posture -------------------------------------------------

// "Absent" and "forbidden" must be byte-identical on the read posture.
func TestCrossWorkspace_WriteHiddenIsIndistinguishable(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("d@example.com", "duser", "editor", f.wsA)
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	// "no such workspace" vs "B exists, you are a stranger, and you
	// addressed it by UUID so resolution succeeded" — two distinct internal
	// reasons that must render identically.
	absent := f.srv.AuthorizeCrossWorkspaceRead(r, "no-such-workspace", CrossWorkspaceWorkspaceOnlyScope())
	forbidden := f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.ID, CrossWorkspaceWorkspaceOnlyScope())
	if absent.Reason == forbidden.Reason {
		t.Fatalf("fixture broken: expected distinct internal reasons, both %q", absent.Reason)
	}

	render := func(a CrossWorkspaceAccess) (int, string) {
		rec := httptest.NewRecorder()
		a.WriteHidden(rec, "Item")
		return rec.Code, rec.Body.String()
	}
	ac, ab := render(absent)
	fc, fb := render(forbidden)
	if ac != http.StatusNotFound || fc != http.StatusNotFound {
		t.Fatalf("expected 404/404, got %d/%d", ac, fc)
	}
	if ab != fb {
		t.Fatalf("response bodies differ — that difference is the leak:\nabsent:    %s\nforbidden: %s", ab, fb)
	}

	// The item-level denials must be indistinguishable from those too:
	// a caller who can read B but not the destination item learns nothing.
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	if err := f.srv.store.SetMemberCollectionAccess(f.wsB.ID, u.ID, "specific", []string{f.collB.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	hidden := f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.Slug, CrossWorkspaceItemScope(f.hiddenIB))
	assertDenied(t, hidden, CrossWorkspaceItemNotVisible, "member of B, hidden destination item")
	hc, hb := render(hidden)
	if hc != ac || hb != ab {
		t.Fatalf("item-hidden response differs from absent-workspace response:\n%d %s\n%d %s", hc, hb, ac, ab)
	}
}

// WriteDenied acknowledges, but still refuses to distinguish absent from
// forbidden.
func TestCrossWorkspace_WriteDeniedDoesNotDistinguishAbsence(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("wd@example.com", "wduser", "editor", f.wsA)
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	render := func(a CrossWorkspaceAccess) (int, string) {
		rec := httptest.NewRecorder()
		a.WriteDenied(rec)
		return rec.Code, rec.Body.String()
	}
	ac, ab := render(f.srv.AuthorizeCrossWorkspaceEdit(r, "no-such-workspace", CrossWorkspaceWorkspaceOnlyScope()))
	fc, fb := render(f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.ID, CrossWorkspaceWorkspaceOnlyScope()))
	if ac != http.StatusForbidden || fc != http.StatusForbidden {
		t.Fatalf("expected 403/403, got %d/%d", ac, fc)
	}
	if ab != fb {
		t.Fatalf("WriteDenied distinguishes absence from forbidden-ness:\n%s\n%s", ab, fb)
	}

	// The token allow-list is the one denial WriteDenied names — see its
	// doc for why that is safe (it is only ever reported to a caller who
	// already has a role in the target).
	tc, _ := render(CrossWorkspaceAccess{Reason: CrossWorkspaceTokenNotAllowed})
	if tc != http.StatusForbidden {
		t.Fatalf("token denial: expected 403, got %d", tc)
	}

	// Lookup failures are the documented 500 variant, and the one
	// observable difference WriteDenied carries. Pinning it here so the
	// trade-off in its doc comment can't drift silently (Codex round 3
	// P2). WriteHidden must NOT have the same variant.
	lc, _ := render(CrossWorkspaceAccess{Reason: CrossWorkspaceLookupFailed, Err: errAuthzTestLookup})
	if lc != http.StatusInternalServerError {
		t.Fatalf("lookup failure via WriteDenied: expected 500, got %d", lc)
	}
	hidden := httptest.NewRecorder()
	CrossWorkspaceAccess{Reason: CrossWorkspaceLookupFailed, Err: errAuthzTestLookup}.WriteHidden(hidden, "Item")
	baseline := httptest.NewRecorder()
	CrossWorkspaceAccess{Reason: CrossWorkspaceWorkspaceNotFound}.WriteHidden(baseline, "Item")
	if hidden.Code != baseline.Code || hidden.Body.String() != baseline.Body.String() {
		t.Fatalf("lookup failure via WriteHidden differs from every other reason: %d %s vs %d %s",
			hidden.Code, hidden.Body.String(), baseline.Code, baseline.Body.String())
	}
}

var errAuthzTestLookup = errors.New("synthetic store failure")

// A denied verdict holds the resolved workspace, the caller's role and
// a reason that separates absence from forbidden-ness. Serializing it
// into a response would hand a caller everything the disclosure rule
// forbids, so every field carries json:"-" as a backstop against a
// stray json.Marshal (Codex round 7).
func TestCrossWorkspace_VerdictDoesNotSerialize(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("ser@example.com", "seruser", "editor", f.wsA)
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})

	// Addressed by UUID so the verdict actually carries a resolved
	// workspace — the shape with something to leak.
	got := f.srv.AuthorizeCrossWorkspaceRead(r, f.wsB.ID, CrossWorkspaceWorkspaceOnlyScope())
	assertDenied(t, got, CrossWorkspaceNoWorkspaceAccess, "stranger to B by UUID")
	if got.Workspace == nil {
		t.Fatal("fixture broken: expected the denial to carry a resolved workspace")
	}

	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(blob) != "{}" {
		t.Fatalf("a denial verdict serialized to %s — every field must be json:\"-\"", blob)
	}
	for _, secret := range []string{f.wsB.ID, f.wsB.Slug, f.wsB.Name, string(got.Reason)} {
		if secret != "" && strings.Contains(string(blob), secret) {
			t.Fatalf("serialized verdict leaks %q: %s", secret, blob)
		}
	}
}

// --- Fail-closed on lookup errors --------------------------------------

// With the store closed underneath it, every path must deny rather than
// allow.
func TestCrossWorkspace_FailsClosedOnStoreError(t *testing.T) {
	f := newCrossWSFixture(t)
	u := f.member("fc@example.com", "fcuser", "editor", f.wsA)
	if err := f.srv.store.AddWorkspaceMember(f.wsB.ID, u.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember B: %v", err)
	}
	r := f.request(u, reqOpts{wsRoleCtx: "editor", wsIDCtx: f.wsA.ID})
	assertAllowed(t, f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, CrossWorkspaceItemScope(f.itemB)),
		"pre-close baseline")

	if err := f.srv.store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, scope := range []CrossWorkspaceScope{
		CrossWorkspaceWorkspaceOnlyScope(),
		CrossWorkspaceCollectionScope(f.collB.ID),
		CrossWorkspaceItemScope(f.itemB),
	} {
		got := f.srv.AuthorizeCrossWorkspaceEdit(r, f.wsB.Slug, scope)
		if got.Allowed {
			t.Fatalf("store error produced an ALLOW verdict for scope %+v", scope)
		}
		if got.Reason != CrossWorkspaceLookupFailed && got.Reason != CrossWorkspaceWorkspaceNotFound {
			t.Fatalf("unexpected reason on store error: %q", got.Reason)
		}
		rec := httptest.NewRecorder()
		got.WriteHidden(rec, "Item")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("WriteHidden on a lookup failure returned %d, want 404", rec.Code)
		}
	}
}
