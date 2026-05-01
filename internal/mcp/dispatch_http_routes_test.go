package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// parseQuery is a thin wrapper around url.ParseQuery that returns the
// values directly. Convenience for the route tests where the function
// signature is the bottleneck.
func parseQuery(s string) (url.Values, error) {
	return url.ParseQuery(s)
}

// mustParseQueryFromPath extracts the query-string portion of path
// and parses it. Test-only — fails the test on malformed input.
func mustParseQueryFromPath(t *testing.T, path string) url.Values {
	t.Helper()
	idx := strings.IndexByte(path, '?')
	if idx < 0 {
		t.Fatalf("path %q has no query string", path)
	}
	values, err := url.ParseQuery(path[idx+1:])
	if err != nil {
		t.Fatalf("parse query in %q: %v", path, err)
	}
	return values
}

// doJSONReq drives a JSON request through srv and returns the
// recorder. Mirrors the local pattern in
// internal/server/server_test.go::doRequest, kept package-local so
// internal/mcp's tests don't depend on test-only code from another
// package.
func doJSONReq(t *testing.T, srv *server.Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := mustJSONRequest(t, method, path, body)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// --- Framework helpers ---

func TestExpandPath_BasicSubstitution(t *testing.T) {
	got, err := expandPath(
		"/api/v1/workspaces/{workspace}/items/{ref}",
		map[string]any{"workspace": "docapp", "ref": "TASK-5"},
	)
	if err != nil {
		t.Fatalf("expandPath: %v", err)
	}
	want := "/api/v1/workspaces/docapp/items/TASK-5"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestExpandPath_NormalizesCollection(t *testing.T) {
	got, err := expandPath(
		"/api/v1/workspaces/{workspace}/collections/{collection}/items",
		map[string]any{"workspace": "docapp", "collection": "task"},
	)
	if err != nil {
		t.Fatalf("expandPath: %v", err)
	}
	if !strings.Contains(got, "/collections/tasks/") {
		t.Errorf("expected `task` to normalize to `tasks`, got %q", got)
	}
}

func TestExpandPath_PathEscapesSpecials(t *testing.T) {
	got, err := expandPath(
		"/api/v1/workspaces/{workspace}/items/{ref}",
		map[string]any{"workspace": "ws/with/slashes", "ref": "weird ref"},
	)
	if err != nil {
		t.Fatalf("expandPath: %v", err)
	}
	if strings.Contains(got, " ") {
		t.Errorf("space not escaped in path: %q", got)
	}
	if strings.Count(got, "/") != strings.Count("/api/v1/workspaces//items/", "/") {
		// 4 literal slashes — the escaped workspace must NOT introduce more.
		// (`/` in the value should be percent-encoded.)
		t.Errorf("workspace slashes leaked: %q", got)
	}
}

func TestExpandPath_MissingPlaceholderErrors(t *testing.T) {
	_, err := expandPath("/items/{ref}", map[string]any{})
	if err == nil {
		t.Errorf("expected error for missing placeholder")
	}
	if !strings.Contains(err.Error(), "ref") {
		t.Errorf("error should mention missing key %q; got %v", "ref", err)
	}
}

func TestExpandPath_NonStringPlaceholderErrors(t *testing.T) {
	_, err := expandPath("/items/{ref}", map[string]any{"ref": 42})
	if err == nil {
		t.Errorf("expected error for non-string placeholder value")
	}
}

func TestExpandPath_EmptyPlaceholderErrors(t *testing.T) {
	_, err := expandPath("/items/{ref}", map[string]any{"ref": ""})
	if err == nil {
		t.Errorf("expected error for empty placeholder value")
	}
}

func TestExpandPath_UnclosedPlaceholderErrors(t *testing.T) {
	_, err := expandPath("/items/{ref", map[string]any{"ref": "x"})
	if err == nil {
		t.Errorf("expected error for unclosed placeholder")
	}
}

func TestBuildQuery_SkipsEmptyAndMissing(t *testing.T) {
	got := buildQuery(
		map[string]any{"a": "x", "b": "", "c": nil, "d": "y"},
		map[string]string{"a": "a", "b": "b", "c": "c", "d": "d", "e": "e"},
	)
	values, err := parseQuery(got)
	if err != nil {
		t.Fatalf("parse query %q: %v", got, err)
	}
	if values.Get("a") != "x" || values.Get("d") != "y" {
		t.Errorf("expected a=x and d=y; got %v", values)
	}
	for _, gone := range []string{"b", "c", "e"} {
		if values.Has(gone) {
			t.Errorf("expected %q to be skipped; got %v", gone, values)
		}
	}
}

func TestBuildQuery_RenamesAndTypes(t *testing.T) {
	got := buildQuery(
		map[string]any{"query": "OAuth", "limit": float64(50), "all": true, "skip": false},
		map[string]string{"q": "query", "limit": "limit", "include_archived": "all", "_skip": "skip"},
	)
	values, err := parseQuery(got)
	if err != nil {
		t.Fatalf("parse query: %v", err)
	}
	if values.Get("q") != "OAuth" {
		t.Errorf("rename q→query lost: %v", values)
	}
	if values.Get("limit") != "50" {
		t.Errorf("expected limit=50 (int form), got %q", values.Get("limit"))
	}
	if values.Get("include_archived") != "true" {
		t.Errorf("expected include_archived=true, got %q", values.Get("include_archived"))
	}
	if values.Has("_skip") {
		t.Errorf("false bool should be skipped; got %v", values)
	}
}

func TestBuildQuery_HandlesJSONNumber(t *testing.T) {
	// JSON decoders configured with UseNumber() produce json.Number.
	// Make sure the framework handles that without losing precision.
	dec := json.NewDecoder(strings.NewReader(`{"limit":50}`))
	dec.UseNumber()
	var input map[string]any
	if err := dec.Decode(&input); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := buildQuery(input, map[string]string{"limit": "limit"})
	if got != "limit=50" {
		t.Errorf("got %q, want limit=50", got)
	}
}

func TestFlatJSONBody_OmitsEmptyAndNil(t *testing.T) {
	body, err := flatJSONBody(
		map[string]any{"a": "x", "b": "", "c": nil, "d": 42, "ignored": "z"},
		[]string{"a", "b", "c", "d", "missing"},
	)
	if err != nil {
		t.Fatalf("flatJSONBody: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	want := map[string]any{"a": "x", "d": float64(42)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- Per-command mapper tests ---

func TestRoute_ItemShow(t *testing.T) {
	m, p, body, err := routeTable["item show"](map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/items/TASK-5" {
		t.Errorf("path = %q", p)
	}
	if body != nil {
		t.Errorf("expected nil body for GET; got %s", body)
	}
}

func TestRoute_ItemDelete(t *testing.T) {
	m, p, _, err := routeTable["item delete"](map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodDelete {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/items/TASK-5" {
		t.Errorf("path = %q", p)
	}
}

func TestRoute_ItemList_AllItemsPath_AppliesDefaultActiveStatusFilter(t *testing.T) {
	// CLI parity: bare `pad item list` hides terminal statuses by
	// default. The HTTP mapper must apply the same broad inclusion
	// list — Codex caught a regression where it returned no status
	// filter and would have leaked done items to agents.
	m, p, _, err := routeTable["item list"](map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if !strings.HasPrefix(p, "/api/v1/workspaces/docapp/items?") {
		t.Errorf("expected cross-collection path with query, got %q", p)
	}
	values := mustParseQueryFromPath(t, p)
	got := values.Get("status")
	if got == "" {
		t.Errorf("default status filter missing; got query %v", values)
	}
	// Spot-check a few well-known active statuses.
	for _, want := range []string{"open", "in_progress", "draft", "exploring"} {
		if !strings.Contains(got, want) {
			t.Errorf("default status filter missing %q; got %q", want, got)
		}
	}
	// And confirm terminal statuses are NOT included.
	for _, blocked := range []string{"done", "completed", "archived"} {
		if strings.Contains(got, blocked) {
			t.Errorf("default status filter incorrectly includes %q; got %q", blocked, got)
		}
	}
}

func TestRoute_ItemList_AllFlagDropsDefaultStatus(t *testing.T) {
	// `--all` overrides the active-status default. Both
	// include_archived=true must be set AND the default status
	// inclusion list must NOT be present (otherwise --all wouldn't
	// actually let through done items).
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace": "docapp", "all": true,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("include_archived") != "true" {
		t.Errorf("include_archived not set under --all; got %v", values)
	}
	if values.Has("status") {
		t.Errorf("status filter must be dropped under --all; got %v", values)
	}
}

func TestRoute_ItemList_ExplicitStatusOverridesDefault(t *testing.T) {
	// An explicit --status pin replaces the active-status default —
	// not appended to it.
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace": "docapp", "status": "done",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("status") != "done" {
		t.Errorf("explicit --status not honored; got %q", values.Get("status"))
	}
}

func TestRoute_ItemList_CollectionScopedPath(t *testing.T) {
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace": "docapp", "collection": "task", // shorthand
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Path may carry the default-active-status query string from the
	// no-explicit-status branch — we only assert the path prefix here.
	if !strings.HasPrefix(p, "/api/v1/workspaces/docapp/collections/tasks/items") {
		t.Errorf("expected collection-scoped + normalized path, got %q", p)
	}
}

func TestRoute_ItemList_FiltersAsQuery(t *testing.T) {
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace": "docapp",
		"status":    "open",
		"priority":  "high",
		"limit":     float64(20),
		"all":       true,
		"parent":    "PLAN-3",
		"role":      "implementer",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	for k, want := range map[string]string{
		"status":           "open",
		"priority":         "high",
		"limit":            "20",
		"include_archived": "true",
		// parent goes through the field-filter path (parseItemListParams
		// treats unknown keys as fields → resolveParentFilter resolves
		// "PLAN-3" → UUID). Sending parent_id would skip ref-resolution
		// and break refs (Codex review #344 finding 2).
		"parent":        "PLAN-3",
		"agent_role_id": "implementer",
	} {
		if got := values.Get(k); got != want {
			t.Errorf("query %q = %q, want %q (full path: %s)", k, got, want, p)
		}
	}
	if values.Has("parent_id") {
		t.Errorf("parent_id incorrectly sent; should use parent (full path: %s)", p)
	}
}

func TestRoute_ItemList_PassesThroughAssignedUserID(t *testing.T) {
	// TASK-967: --assign is preprocessed at the dispatcher level
	// (resolveAssignName) before the mapper runs. By the time
	// mapItemList sees the input, only `assigned_user_id` should be
	// present; the mapper adds it to the query string for the
	// store filter to pick up.
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace":        "docapp",
		"assigned_user_id": "user-uuid-456",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("assigned_user_id") != "user-uuid-456" {
		t.Errorf("assigned_user_id missing from query: %v (path: %q)", values, p)
	}
}

func TestRoute_ItemList_RawAssignKeyIsIgnoredByMapper(t *testing.T) {
	// Belt-and-braces: if a test or upstream caller bypasses
	// Dispatch's preprocess and passes raw `assign` directly to the
	// mapper, it should be silently dropped (matching the existing
	// "unknown input keys are ignored" behaviour) rather than
	// erroring. The dispatcher-level preprocess is what runs the
	// resolution in production.
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace": "docapp", "assign": "Dave",
	})
	if err != nil {
		t.Fatalf("expected raw assign to pass through silently; got err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Has("assigned_user_id") {
		t.Errorf("raw assign incorrectly synthesized assigned_user_id: %v", values)
	}
	if values.Has("assign") {
		t.Errorf("raw assign leaked into query: %v", values)
	}
}

func TestRoute_ItemList_NumericLimitFromJSONNumber(t *testing.T) {
	// JSON decoders configured with UseNumber() produce json.Number
	// instead of float64. The mapper's numeric helper must handle both.
	dec := json.NewDecoder(strings.NewReader(`{"workspace":"docapp","limit":50}`))
	dec.UseNumber()
	var input map[string]any
	if err := dec.Decode(&input); err != nil {
		t.Fatalf("decode: %v", err)
	}
	_, p, _, err := routeTable["item list"](input)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("limit") != "50" {
		t.Errorf("limit lost from json.Number input; got %q (path %q)", values.Get("limit"), p)
	}
}

func TestRoute_ItemList_FieldKVPLandsAsQueryParam(t *testing.T) {
	_, p, _, err := routeTable["item list"](map[string]any{
		"workspace": "docapp",
		"field":     []any{"trigger=on-implement", "scope=all"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("trigger") != "on-implement" {
		t.Errorf("--field trigger lost: %v", values)
	}
	if values.Get("scope") != "all" {
		t.Errorf("--field scope lost: %v", values)
	}
}

func TestRoute_ItemMove(t *testing.T) {
	m, p, body, err := routeTable["item move"](map[string]any{
		"workspace":         "docapp",
		"ref":               "BUG-3",
		"target_collection": "task", // shorthand
		"field":             []any{"priority=high"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodPost {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/items/BUG-3/move" {
		t.Errorf("path = %q", p)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v\n%s", err, body)
	}
	if payload["target_collection"] != "tasks" {
		t.Errorf("target_collection not normalized: %v", payload)
	}
	if payload["source"] != "cli" {
		t.Errorf("source not stamped: %v", payload)
	}
	overrides, ok := payload["field_overrides"].(map[string]any)
	if !ok || overrides["priority"] != "high" {
		t.Errorf("field_overrides missing priority=high: %v", payload)
	}
}

func TestRoute_ItemMove_RequiresAllThree(t *testing.T) {
	for _, missing := range []string{"workspace", "ref", "target_collection"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{
				"workspace": "ws", "ref": "TASK-1", "target_collection": "tasks",
			}
			delete(input, missing)
			_, _, _, err := routeTable["item move"](input)
			if err == nil {
				t.Errorf("expected error for missing %q", missing)
			}
		})
	}
}

func TestRoute_ItemSearch(t *testing.T) {
	m, p, _, err := routeTable["item search"](map[string]any{
		"workspace": "docapp",
		"query":     "OAuth redirect",
		"limit":     float64(25),
		"status":    "open",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if !strings.HasPrefix(p, "/api/v1/search?") {
		t.Errorf("expected /api/v1/search?... ; got %q", p)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("q") != "OAuth redirect" {
		t.Errorf("q rename lost: %v", values)
	}
	if values.Get("workspace") != "docapp" {
		t.Errorf("workspace not in query: %v", values)
	}
	if values.Get("limit") != "25" {
		t.Errorf("limit lost: %v", values)
	}
}

func TestRoute_ItemSearch_NormalizesCollectionAlias(t *testing.T) {
	// CLI parity: `pad item search foo --collection task` normalizes
	// to "tasks" before calling /search. The store's search filter
	// matches exact c.slug = ?, so the alias would 0-match without
	// normalization (Codex review #344 round 2 finding).
	_, p, _, err := routeTable["item search"](map[string]any{
		"workspace":  "docapp",
		"query":      "OAuth",
		"collection": "task",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	values := mustParseQueryFromPath(t, p)
	if values.Get("collection") != "tasks" {
		t.Errorf("collection alias not normalized; got %q", values.Get("collection"))
	}
}

func TestRoute_ItemSearch_DoesNotMutateInput(t *testing.T) {
	// The mapper clones the input before mutating to avoid
	// surprising the caller (the registry attaches the same input
	// map via WithDispatchInput; downstream code reads it).
	input := map[string]any{
		"workspace": "ws", "query": "x", "collection": "task",
	}
	_, _, _, err := routeTable["item search"](input)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if input["collection"] != "task" {
		t.Errorf("input was mutated; got collection=%v", input["collection"])
	}
}

func TestRoute_ItemSearch_RequiresQuery(t *testing.T) {
	_, _, _, err := routeTable["item search"](map[string]any{"workspace": "ws"})
	if err == nil {
		t.Errorf("expected error when query missing")
	}
}

func TestRoute_ItemComment(t *testing.T) {
	m, p, body, err := routeTable["item comment"](map[string]any{
		"workspace": "docapp",
		"ref":       "TASK-5",
		"message":   "Looks good to me.",
		"reply_to":  "comment-id-7",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodPost {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/items/TASK-5/comments" {
		t.Errorf("path = %q", p)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if payload["body"] != "Looks good to me." {
		t.Errorf("message → body rename lost: %v", payload)
	}
	if payload["parent_id"] != "comment-id-7" {
		t.Errorf("reply_to → parent_id rename lost: %v", payload)
	}
	if payload["source"] != "cli" {
		t.Errorf("source not stamped: %v", payload)
	}
}

func TestRoute_ItemComments(t *testing.T) {
	m, p, _, err := routeTable["item comments"](map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/items/TASK-5/comments" {
		t.Errorf("path = %q", p)
	}
}

func TestRoute_ProjectDashboard(t *testing.T) {
	m, p, _, err := routeTable["project dashboard"](map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet || p != "/api/v1/workspaces/docapp/dashboard" {
		t.Errorf("got %s %s", m, p)
	}
}

func TestRoute_CollectionList(t *testing.T) {
	m, p, _, err := routeTable["collection list"](map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet || p != "/api/v1/workspaces/docapp/collections" {
		t.Errorf("got %s %s", m, p)
	}
}

func TestRoute_RoleList(t *testing.T) {
	m, p, _, err := routeTable["role list"](map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet || p != "/api/v1/workspaces/docapp/agent-roles" {
		t.Errorf("got %s %s", m, p)
	}
}

// TestRouteTable_ContainsExpectedCommands locks the set of commands
// the table claims to support, so an accidental deletion fails loudly
// rather than silently flipping a tool back to "not yet implemented."
func TestRouteTable_ContainsExpectedCommands(t *testing.T) {
	want := []string{
		"item create", "item show", "item list", "item delete",
		"item move", "item search", "item comment", "item comments",
		"project dashboard", "collection list", "role list",
	}
	missing := []string{}
	for _, w := range want {
		if _, ok := routeTable[w]; !ok {
			missing = append(missing, w)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("routeTable missing entries: %v", missing)
	}
}

// --- Integration smoke for the new commands ---
//
// Drives a small subset (item show + collection list + project
// dashboard) end-to-end against a real *server.Server. Expanded
// coverage for the simple read paths is via the unit tests above;
// this is the "did we wire it together right" check.

func TestHTTPHandlerDispatcher_Integration_ReadPaths(t *testing.T) {
	srv, st := newPadServer(t)

	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces",
		map[string]any{"name": "DocApp"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}

	user, err := st.CreateUser(models.UserCreate{
		Email: "dave@example.com", Name: "Dave",
		Password: "irrelevant",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	d := &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: fixedUserResolver(user),
	}

	// Seed: create one item via the dispatcher (proves item.create
	// still works after the route-table refactor) and then read it
	// back via item.show + project.dashboard + collection.list.
	createCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  ws.Slug,
		"collection": "tasks",
		"title":      "Smoke",
		"priority":   "high",
	})
	if res, err := d.Dispatch(createCtx, []string{"item", "create"}, nil); err != nil || res.IsError {
		t.Fatalf("seed item create: err=%v IsError=%v %#v", err, res != nil && res.IsError, res)
	}

	// item show — by listing first to grab the ref.
	listCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "collection": "tasks",
	})
	listRes, err := d.Dispatch(listCtx, []string{"item", "list"}, nil)
	if err != nil || listRes.IsError {
		t.Fatalf("item list: err=%v IsError=%v %#v", err, listRes != nil && listRes.IsError, listRes)
	}
	listed := unwrapItems(t, listRes.StructuredContent)
	if len(listed) == 0 {
		t.Fatalf("item list returned empty items array: %#v", listRes.StructuredContent)
	}
	first, _ := listed[0].(map[string]any)
	ref, _ := first["ref"].(string)
	if ref == "" {
		t.Fatalf("item list result missing ref: %#v", first)
	}

	// item show
	showCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "ref": ref,
	})
	showRes, err := d.Dispatch(showCtx, []string{"item", "show"}, nil)
	if err != nil || showRes.IsError {
		t.Fatalf("item show: err=%v IsError=%v %#v", err, showRes != nil && showRes.IsError, showRes)
	}

	// project dashboard
	dashCtx := WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug})
	dashRes, err := d.Dispatch(dashCtx, []string{"project", "dashboard"}, nil)
	if err != nil || dashRes.IsError {
		t.Fatalf("project dashboard: err=%v IsError=%v %#v", err, dashRes != nil && dashRes.IsError, dashRes)
	}

	// collection list
	collCtx := WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug})
	collRes, err := d.Dispatch(collCtx, []string{"collection", "list"}, nil)
	if err != nil || collRes.IsError {
		t.Fatalf("collection list: err=%v IsError=%v %#v", err, collRes != nil && collRes.IsError, collRes)
	}
}
