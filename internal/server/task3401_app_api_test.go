package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3401 (SPEC-6 U6a): the app API's door, ceiling and read routes.

const task3401Schema = `{"fields":[
 {"key":"status","label":"Status","type":"select","options":["open","done"],"default":"open"},
 {"key":"size","label":"Size","type":"text"},
 {"key":"owner","label":"Owner","type":"relation","collection":"private"}
]}`

type appAPIFix struct {
	srv                        *Server
	in                         testInstall
	token                      string
	ws                         *models.Workspace
	companion, system, private *models.Collection
	item, privateItem          *models.Item
	comment                    *models.Comment
}

func appAPIFixture(t *testing.T, access string) appAPIFix {
	t.Helper()
	srv := appOAuthServer(t, true)
	srv.store.SetEncryptionKey(bytes32ForTest())
	in := newTestInstall(t, srv, "inst-api")
	ws, err := srv.store.GetWorkspaceByID(in.wsID)
	if err != nil || ws == nil {
		t.Fatal(err)
	}
	f := appAPIFix{srv: srv, in: in, ws: ws}
	mk := func(name, slug, schema string) *models.Collection {
		c, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: name, Slug: slug, Schema: schema})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	f.companion = mk("Requests", "requests", task3401Schema)
	f.system = mk("Conventions App", "conventions-app", `{"fields":[]}`)
	f.private = mk("Private", "private", `{"fields":[]}`)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := srv.store.DB().Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`UPDATE collections SET via_app = ? WHERE id = ?`, in.id, f.companion.ID)
	exec(`UPDATE collections SET is_system = 1 WHERE id = ?`, f.system.ID)
	exec(`UPDATE app_installs SET bot_user_id = ?, service_access = ? WHERE id = ?`, in.bot.ID, access, in.id)
	// The bot is a member with "all" access: the ceiling must not rely on
	// U8b narrowing it.
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role, collection_access, created_at) VALUES (?, ?, 'editor', 'all', ?)`,
		ws.ID, in.bot.ID, time.Now().UTC().Format(time.RFC3339))

	human := createTestUserDirect(t, srv, "adjunct-3401@example.com")
	f.privateItem, err = srv.store.CreateItem(ws.ID, f.private.ID, models.ItemCreate{Title: "Secret", ActorUserID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	// An item laden with every adjunct a DTO must never carry: a relation
	// value, a parent, a lease, a reaction on its comment.
	f.item, err = srv.store.CreateItem(ws.ID, f.companion.ID, models.ItemCreate{Title: "Login broken", Content: "It fails.",
		Fields: `{"status":"open","size":"L","owner":"` + f.privateItem.ID + `","ghost":"undeclared"}`, ActorUserID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE items SET via_app = ?, created_via_app = ? WHERE id = ?`, in.id, in.id, f.item.ID)
	if _, err := srv.store.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: f.privateItem.ID, LinkType: "parent"}, f.item.ID); err != nil {
		t.Fatalf("parent link: %v", err)
	}
	if _, err := srv.store.ClaimItemLease(f.item.ID, "someone", human.ID, time.Hour); err != nil {
		t.Fatalf("lease: %v", err)
	}
	f.comment, err = srv.store.CreateComment(ws.ID, f.item.ID, human.ID, models.CommentCreate{Body: "seen", Author: "Ada", CreatedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.AddReaction(f.comment.ID, human.ID, "user", "👍"); err != nil {
		t.Fatalf("reaction: %v", err)
	}
	f.token = mintServiceToken(t, srv, in)
	return f
}

func createTestUserDirect(t *testing.T, srv *Server, email string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{Email: email, Name: "Ada", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func appGet(srv *Server, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	return rr
}

func (f appAPIFix) path(p string) string { return appAPIPrefix + "/workspaces/" + f.ws.ID + p }

// Every row of the route table is reachable with a valid token, and every
// response's key set is exactly its DTO's, nested keys included (the census).
func TestTask3401_RoutesAndDTOCensus(t *testing.T) {
	f := appAPIFixture(t, "read")
	paths := map[string]string{
		"appListCollections": "/collections",
		"appGetCollection":   "/collections/requests",
		"appListItems":       "/collections/requests/items",
		"appGetItem":         "/items/" + f.item.ID,
		"appListComments":    "/items/" + f.item.ID + "/comments",
		"appMe":              "/me",
	}
	if len(paths) != len(appRoutes) {
		t.Fatalf("the census covers %d routes, the table has %d", len(paths), len(appRoutes))
	}
	for _, rt := range appRoutes {
		p, ok := paths[rt.Name]
		if !ok {
			t.Errorf("route %s has no census path", rt.Name)
			continue
		}
		rr := appGet(f.srv, f.path(p), f.token)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: %d %s", rt.Name, rr.Code, rr.Body.String())
			continue
		}
		if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type %q", rt.Name, ct)
		}
		var body any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", rt.Name, err)
		}
		checkAppDTOKeys(t, rt.Name, body)
	}
}

// The key sets of each DTO, nested ones included. A field added to a DTO
// without a spec amendment fails here.
var appDTOKeys = map[string][]string{
	"collection":   {"icon", "is_system", "name", "schema", "slug"},
	"schema":       {"fields"},
	"schema_field": {"key", "label", "options", "required", "type", "default"}, // options/default only when set
	"item":         {"collection", "content", "created_at", "created_by_display", "etag", "fields", "id", "title", "updated_at", "via_app"},
	"comment":      {"author_display", "author_kind", "body", "created_at", "deleted", "edited", "id", "item_id", "parent_comment_id", "updated_at"},
	"me":           {"display_name", "is_app", "role", "user_id"},
}

func appKeysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func assertKeysWithin(t *testing.T, where string, got map[string]any, dto string, required []string) {
	t.Helper()
	allowed := map[string]bool{}
	for _, k := range appDTOKeys[dto] {
		allowed[k] = true
	}
	for _, k := range appKeysOf(got) {
		if !allowed[k] {
			t.Errorf("%s: key %q is not in the %s DTO", where, k, dto)
		}
	}
	for _, k := range required {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: key %q missing from the %s DTO", where, k, dto)
		}
	}
}

func checkCollection(t *testing.T, where string, c map[string]any) {
	assertKeysWithin(t, where, c, "collection", appDTOKeys["collection"])
	schema, _ := c["schema"].(map[string]any)
	assertKeysWithin(t, where+".schema", schema, "schema", appDTOKeys["schema"])
	fields, _ := schema["fields"].([]any)
	for _, fd := range fields {
		m := fd.(map[string]any)
		assertKeysWithin(t, where+".schema.fields[]", m, "schema_field", []string{"key", "label", "required", "type"})
		if m["type"] == "relation" || m["type"] == "multi_relation" {
			t.Errorf("%s: a relation field is in the schema projection", where)
		}
	}
}

func checkItem(t *testing.T, where string, it map[string]any) {
	assertKeysWithin(t, where, it, "item", appDTOKeys["item"])
	fields, _ := it["fields"].(map[string]any)
	for k := range fields {
		if k != "status" && k != "size" {
			t.Errorf("%s.fields carries %q: only declared non-relation keys may appear", where, k)
		}
	}
}

func checkAppDTOKeys(t *testing.T, route string, body any) {
	t.Helper()
	m, _ := body.(map[string]any)
	switch route {
	case "appListCollections":
		if strings.Join(appKeysOf(m), ",") != "collections" {
			t.Errorf("%s: top-level keys %v", route, appKeysOf(m))
		}
		for _, c := range m["collections"].([]any) {
			checkCollection(t, route+".collections[]", c.(map[string]any))
		}
	case "appGetCollection":
		checkCollection(t, route, m)
	case "appListItems":
		for _, it := range m["items"].([]any) {
			checkItem(t, route+".items[]", it.(map[string]any))
		}
	case "appGetItem":
		checkItem(t, route, m)
	case "appListComments":
		for _, c := range m["comments"].([]any) {
			assertKeysWithin(t, route+".comments[]", c.(map[string]any), "comment", appDTOKeys["comment"])
		}
	case "appMe":
		assertKeysWithin(t, route, m, "me", appDTOKeys["me"])
	}
}

// The request DTO shapes are reflected, so a Go field added without a JSON
// key, or with an unlisted one, fails here too.
func TestTask3401_ResponseDTOFieldSets(t *testing.T) {
	jsonKeys := func(v any) []string {
		var out []string
		tp := reflect.TypeOf(v)
		for i := 0; i < tp.NumField(); i++ {
			name := strings.Split(tp.Field(i).Tag.Get("json"), ",")[0]
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	check := func(name string, v any, want []string) {
		w := append([]string(nil), want...)
		sort.Strings(w)
		if got := jsonKeys(v); strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s fields = %v, want %v", name, got, w)
		}
	}
	check("AppCollection", AppCollection{}, appDTOKeys["collection"])
	check("AppSchemaField", AppSchemaField{}, appDTOKeys["schema_field"])
	check("AppItem", AppItem{}, appDTOKeys["item"])
	check("AppComment", AppComment{}, appDTOKeys["comment"])
	check("AppMe", AppMe{}, appDTOKeys["me"])
}

// The ceiling: a bot with "all" membership reads only its companions and the
// system collections, wherever it asks.
func TestTask3401_TheCeiling(t *testing.T) {
	f := appAPIFixture(t, "read")
	rr := appGet(f.srv, f.path("/collections"), f.token)
	var list struct {
		Collections []AppCollection `json:"collections"`
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &list)
	var slugs []string
	for _, c := range list.Collections {
		slugs = append(slugs, c.Slug)
	}
	sort.Strings(slugs)
	if strings.Join(slugs, ",") != "conventions-app,requests" {
		t.Errorf("collections = %v, want the companion and the system collection only", slugs)
	}
	for _, p := range []string{"/collections/private", "/collections/private/items", "/items/" + f.privateItem.ID, "/items/" + f.privateItem.ID + "/comments"} {
		if rr := appGet(f.srv, f.path(p), f.token); rr.Code != http.StatusNotFound {
			t.Errorf("%s outside the ceiling: %d %s, want 404", p, rr.Code, rr.Body.String())
		}
	}
	if rr := appGet(f.srv, f.path("/collections/requests/items"), f.token); !strings.Contains(rr.Body.String(), f.item.ID) {
		t.Errorf("the companion item is not listed: %s", rr.Body.String())
	}
}

// The door: every refusal before a handler runs.
func TestTask3401_TheDoorRefuses(t *testing.T) {
	f := appAPIFixture(t, "read")
	me := f.path("/me")
	if rr := appGet(f.srv, me, ""); rr.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rr.Code)
	}
	if rr := appGet(f.srv, me, "garbage"); rr.Code != http.StatusUnauthorized {
		t.Errorf("garbage token: %d", rr.Code)
	}
	// An MCP token and an MCP refresh token are not app tokens.
	sess := newOAuthSession(t, f.srv)
	mcpTok, code := mintWithResource(t, f.srv, sess, testCanonicalAudience)
	if code != http.StatusOK {
		t.Fatalf("mcp mint: %d", code)
	}
	for name, tok := range map[string]string{"an MCP token": mcpTok, "a refresh token": lastRefresh} {
		if rr := appGet(f.srv, me, tok); rr.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401", name, rr.Code)
		}
	}
	// A slug in {ws}, or another workspace's id: the workspace 404.
	if rr := appGet(f.srv, appAPIPrefix+"/workspaces/"+f.ws.Slug+"/me", f.token); rr.Code != http.StatusNotFound {
		t.Errorf("{ws} as a slug: %d, want 404", rr.Code)
	}
	if rr := appGet(f.srv, appAPIPrefix+"/workspaces/00000000-0000-0000-0000-000000000000/me", f.token); rr.Code != http.StatusNotFound {
		t.Errorf("another {ws}: %d, want 404", rr.Code)
	}
	// An app token never reaches the human API.
	if rr := appGet(f.srv, "/api/v1/workspaces", f.token); rr.Code != http.StatusUnauthorized {
		t.Errorf("an app token on /api/v1: %d, want 401", rr.Code)
	}
	// No service access, a delegated binding, a stale epoch: refused.
	exec := func(q string, args ...any) {
		if _, err := f.srv.store.DB().Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE app_installs SET service_access = NULL WHERE id = ?`, f.in.id)
	if rr := appGet(f.srv, me, f.token); rr.Code != http.StatusUnauthorized {
		t.Errorf("no service access: %d, want 401", rr.Code)
	}
	exec(`UPDATE app_installs SET service_access = 'read' WHERE id = ?`, f.in.id)
	exec(`UPDATE app_token_bindings SET auth_kind = 'delegated' WHERE install_id = ?`, f.in.id)
	if rr := appGet(f.srv, me, f.token); rr.Code != http.StatusUnauthorized {
		t.Errorf("a delegated binding before TASK-3399: %d, want 401", rr.Code)
	}
	exec(`UPDATE app_token_bindings SET auth_kind = 'service' WHERE install_id = ?`, f.in.id)
	if rr := appGet(f.srv, me, f.token); rr.Code != http.StatusOK {
		t.Fatalf("control: %d %s", rr.Code, rr.Body.String())
	}
	exec(`UPDATE app_installs SET auth_epoch = auth_epoch + 1 WHERE id = ?`, f.in.id)
	if rr := appGet(f.srv, me, f.token); rr.Code != http.StatusUnauthorized {
		t.Errorf("a stale epoch: %d, want 401", rr.Code)
	}
}

// A write row refuses a read token before anything is resolved: the access
// step runs first, on every write row (U6b adds them).
func TestTask3401_AWriteRowRefusesAReadToken(t *testing.T) {
	f := appAPIFixture(t, "read")
	reached := false
	h := f.srv.requireAppAccess("write", http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	req := httptest.NewRequest("POST", "/x", nil)
	req = req.WithContext(withAppContextForTest(req, &appContext{Access: "read"}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden || reached {
		t.Errorf("read token on a write row: %d reached=%v, want 403 and no handler", rr.Code, reached)
	}
	rr = httptest.NewRecorder()
	req = req.WithContext(withAppContextForTest(req, &appContext{Access: "write"}))
	h.ServeHTTP(rr, req)
	if !reached {
		t.Error("control: a write token did not reach the handler")
	}
}

// The self-check refuses a request missing any key the helpers read.
func TestTask3401_ContextSelfCheck(t *testing.T) {
	f := appAPIFixture(t, "read")
	var seen *http.Request
	h := f.srv.requireAppWorkspace(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r }))
	grant := &AppTokenGrant{InstallID: f.in.id, WorkspaceID: f.ws.ID, AuthKind: "service", Subject: f.in.bot.ID}
	req := httptest.NewRequest("GET", f.path("/me"), nil)
	rctx := newChiCtx(map[string]string{"ws": f.ws.ID})
	req = req.WithContext(withChiCtx(req, rctx))
	req = req.WithContext(withAppContextForTest(req, &appContext{Grant: grant, InstallID: f.in.id, WorkspaceID: f.ws.ID, Access: "read", AuthKind: "service", Actor: f.in.bot}))
	req = req.WithContext(WithCurrentUser(req.Context(), f.in.bot))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen == nil {
		t.Fatalf("a complete request was refused: %d %s", rr.Code, rr.Body.String())
	}
	if err := appContextSelfCheck(seen); err != nil {
		t.Fatalf("a complete context fails the self-check: %v", err)
	}
	// Each key, removed, fails it.
	ac := appContextFrom(seen)
	broken := *ac
	broken.ReadCeiling = nil
	if appContextSelfCheck(seen.WithContext(withAppContextForTest(seen, &broken))) == nil {
		t.Error("a missing ceiling passed the self-check")
	}
	if appContextSelfCheck(seen.WithContext(WithTokenAllowedWorkspaces(seen.Context(), nil))) == nil {
		t.Error("a missing allow-list passed the self-check")
	}
	if appContextSelfCheck(seen.WithContext(WithCurrentUser(seen.Context(), &models.User{ID: "someone-else"}))) == nil {
		t.Error("a current user other than the actor passed the self-check")
	}
}

// Cross-workspace helpers refuse outright under app context. Each leg has a
// control without the app context that IS allowed, so the refusal is the
// app context's, not some other denial.
func TestTask3401_CrossWorkspaceHelpersRefuseApps(t *testing.T) {
	mf := newMovedToFixture(t)
	mf.record(mf.destB, true, 1)
	mf.archiveSource()
	base := func() *http.Request {
		req := httptest.NewRequest("GET", "/x", nil)
		ctx := WithCurrentUser(req.Context(), mf.owner)
		ctx = context.WithValue(ctx, ctxResolvedWorkspaceID, mf.wsA.ID)
		ctx = context.WithValue(ctx, ctxWorkspaceRole, "owner")
		return req.WithContext(ctx)
	}
	asApp := func(r *http.Request) *http.Request {
		return r.WithContext(withAppContextForTest(r, &appContext{InstallID: "inst-x"}))
	}
	if a := mf.srv.AuthorizeCrossWorkspaceRead(base(), mf.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()); !a.Allowed {
		t.Fatalf("control: the owner's cross-workspace read was refused: %v", a.Reason)
	}
	if a := mf.srv.AuthorizeCrossWorkspaceRead(asApp(base()), mf.wsB.Slug, CrossWorkspaceWorkspaceOnlyScope()); a.Allowed {
		t.Error("a cross-workspace read was allowed under app context")
	}
	now := time.Now()
	source := &models.Item{ID: mf.source.ID, WorkspaceID: mf.wsA.ID, CollectionID: mf.collA.ID, DeletedAt: &now}
	if got := mf.srv.movedToDestinations(base(), source); len(got) == 0 {
		t.Fatal("control: the moved item names no destination")
	}
	if got := mf.srv.movedToDestinations(asApp(base()), source); got != nil {
		t.Errorf("moved_to under app context: %v", got)
	}
}

// A read token acts as a viewer whatever the bot's membership; a write token
// keeps its membership's role.
func TestTask3401_TheRoleFollowsTheAccess(t *testing.T) {
	for access, want := range map[string]string{"read": "viewer", "write": "editor"} {
		f := appAPIFixture(t, access)
		var me AppMe
		rr := appGet(f.srv, f.path("/me"), f.token)
		_ = json.Unmarshal(rr.Body.Bytes(), &me)
		if me.Role != want {
			t.Errorf("%s token: role %q, want %q", access, me.Role, want)
		}
	}
}

// A comment whose parent is on ANOTHER item projects parent_comment_id null.
func TestTask3401_ACrossItemParentProjectsToNull(t *testing.T) {
	f := appAPIFixture(t, "read")
	human := createTestUserDirect(t, f.srv, "elsewhere-3401@example.com")
	elsewhere, err := f.srv.store.CreateComment(f.ws.ID, f.privateItem.ID, human.ID, models.CommentCreate{Body: "other", Author: "B", CreatedBy: "user"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.srv.store.DB().Exec(`UPDATE comments SET parent_id = ? WHERE id = ?`, elsewhere.ID, f.comment.ID); err != nil {
		t.Fatal(err)
	}
	reply, err := f.srv.store.CreateComment(f.ws.ID, f.item.ID, human.ID, models.CommentCreate{Body: "reply", Author: "C", CreatedBy: "user", ParentID: f.comment.ID})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Comments []AppComment `json:"comments"`
	}
	_ = json.Unmarshal(appGet(f.srv, f.path("/items/"+f.item.ID+"/comments"), f.token).Body.Bytes(), &out)
	got := map[string]*string{}
	for _, c := range out.Comments {
		got[c.ID] = c.ParentCommentID
	}
	if p, ok := got[f.comment.ID]; !ok || p != nil {
		t.Errorf("a cross-item parent projected as %v, want null", p)
	}
	if p := got[reply.ID]; p == nil || *p != f.comment.ID {
		t.Errorf("control: a same-item parent projected as %v", p)
	}
}

// Lead ruling R2: every caller of the store's visibility, listed with how
// the ceiling reaches it. A new caller fails until it is reviewed.
func TestTask3401_EveryVisibilityCallerIsCeilinged(t *testing.T) {
	reviewed := map[string]string{
		"server.go::visibleCollectionIDs":               "applies the request's app ceiling; the store ceilings a bot",
		"server.go::checkItemVisibleQ":                  "reached from app routes only through requireItemVisible, which checks the app ceiling first; the store ceilings a bot",
		"handlers_me.go::handleGetMe":                   "not an app route (TokenAuth refuses an app token); the store ceilings a bot",
		"handlers_reports.go::reportVisibleCollections": "not an app route; the store ceilings a bot",
		"handlers_collab.go::authorizeCollabAccess":     "not an app route; the store ceilings a bot",
		"handlers_events.go::computeSSEVisibility":      "not an app route; the store ceilings a bot",
	}
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	found := map[string]bool{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		af, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "VisibleCollectionIDs", "VisibleCollectionIDsQ", "GuestVisibleCollectionIDs":
					found[file+"::"+fn.Name.Name] = true
				}
				return true
			})
		}
	}
	for site := range found {
		if _, ok := reviewed[site]; !ok {
			t.Errorf("%s calls the store's visibility and is not reviewed for the app ceiling", site)
		}
	}
	for site := range reviewed {
		if !found[site] {
			t.Errorf("%s is reviewed but no longer calls the store's visibility; remove it", site)
		}
	}
}

// The etag key is server-only and mandatory: no encryption key, no app store.
func TestTask3401_ETagKeyIsMandatory(t *testing.T) {
	srv := testServer(t)
	if _, err := srv.appStore(); err == nil {
		t.Error("an app store was built with no encryption key")
	}
	f := appAPIFixture(t, "read")
	a := appGet(f.srv, f.path("/items/"+f.item.ID), f.token)
	b := appGet(f.srv, f.path("/items/"+f.item.ID), f.token)
	var ia, ib AppItem
	_ = json.Unmarshal(a.Body.Bytes(), &ia)
	_ = json.Unmarshal(b.Body.Bytes(), &ib)
	if ia.ETag == "" || ia.ETag != ib.ETag {
		t.Errorf("etag unstable or empty: %q %q", ia.ETag, ib.ETag)
	}
}

var _ = url.Values{}

func withAppContextForTest(r *http.Request, ac *appContext) context.Context {
	return context.WithValue(r.Context(), appCtxKey{}, ac)
}

func newChiCtx(params map[string]string) *chi.Context {
	rctx := chi.NewRouteContext()
	for k, v := range params {
		rctx.URLParams.Add(k, v)
	}
	return rctx
}

func withChiCtx(r *http.Request, rctx *chi.Context) context.Context {
	return context.WithValue(r.Context(), chi.RouteCtxKey, rctx)
}

// The request-level ceiling, which a delegated token (TASK-3399) will rely
// on: the actor is a PERSON with every collection visible, and the app
// context alone narrows it, in visibleCollectionIDs and in item visibility.
func TestTask3401_TheRequestCeilingBindsAPersonActor(t *testing.T) {
	f := appAPIFixture(t, "read")
	person := createTestUserDirect(t, f.srv, "delegate-3401@example.com")
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, person.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/x", nil)
	ctx := withAppContextForTest(req, &appContext{InstallID: f.in.id, ReadCeiling: []string{f.companion.ID, f.system.ID}})
	ctx = WithCurrentUser(ctx, person)
	ctx = WithAPITokenAuth(ctx)
	req = req.WithContext(context.WithValue(ctx, ctxWorkspaceRole, "editor"))
	ids, err := f.srv.visibleCollectionIDs(req, f.ws.ID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(ids)
	want := []string{f.companion.ID, f.system.ID}
	sort.Strings(want)
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Errorf("visible under a request ceiling = %v, want %v", ids, want)
	}
	rr := httptest.NewRecorder()
	if f.srv.requireItemVisible(rr, req, f.ws.ID, f.privateItem) {
		t.Error("an item outside the request ceiling was visible to a person actor")
	}
	if !f.srv.requireItemVisible(httptest.NewRecorder(), req, f.ws.ID, f.item) {
		t.Error("control: the companion item is not visible")
	}
}
