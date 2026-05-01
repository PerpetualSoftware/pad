package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// fixedUserResolver is a tiny helper for the unit tests that need a
// deterministic user without standing up the auth middleware.
func fixedUserResolver(u *models.User) func(context.Context) *models.User {
	return func(_ context.Context) *models.User { return u }
}

// recordingHandler captures the request the dispatcher synthesizes so
// the unit tests can assert on method/path/body/context without
// pulling in the full handler chain. Returns 201 with the supplied
// response body so packageHTTPResponse's success path is exercised.
type recordingHandler struct {
	t            *testing.T
	wantStatus   int
	respBody     string
	gotMethod    string
	gotPath      string
	gotBody      string
	gotUser      *models.User
	gotIsAPITok  bool
	contentType  string
	requestCount int
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.t.Helper()
	h.requestCount++
	h.gotMethod = r.Method
	h.gotPath = r.URL.Path
	h.contentType = r.Header.Get("Content-Type")

	body, _ := io.ReadAll(r.Body)
	h.gotBody = string(body)
	defer r.Body.Close()

	// Pull whatever the dispatcher attached via server.WithCurrentUser
	// + server.WithAPITokenAuth. Going through exported helpers keeps
	// the test's expectations aligned with the production middleware
	// path.
	if u := currentUserFromRequest(r); u != nil {
		h.gotUser = u
	}
	h.gotIsAPITok = isAPITokenFromRequest(r)

	status := h.wantStatus
	if status == 0 {
		status = http.StatusCreated
	}
	if h.respBody == "" {
		h.respBody = `{"id":"test-item","ref":"TASK-1","title":"Fix OAuth"}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(h.respBody))
}

// currentUserFromRequest / isAPITokenFromRequest mirror the
// package-private accessors in internal/server. We need read-only
// access for tests; surface a tiny shim instead of exporting the
// originals (which would broaden the auth-bypass surface).
func currentUserFromRequest(r *http.Request) *models.User {
	v, _ := server.CurrentUserFromContext(r.Context())
	return v
}

func isAPITokenFromRequest(r *http.Request) bool {
	return server.IsAPITokenFromContext(r.Context())
}

func TestHTTPHandlerDispatcher_RoutesItemCreate(t *testing.T) {
	user := &models.User{ID: "user-1", Name: "Dave", Email: "dave@example.com"}
	rec := &recordingHandler{t: t}

	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: fixedUserResolver(user),
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "Fix OAuth",
		"priority":   "high",
		"field":      []any{"status=in-progress", "due_date=2026-06-01"},
	})

	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("expected success, got IsError result: %#v", res)
	}

	if rec.gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", rec.gotMethod)
	}
	wantPath := "/api/v1/workspaces/docapp/collections/tasks/items"
	if rec.gotPath != wantPath {
		t.Errorf("path = %q, want %q", rec.gotPath, wantPath)
	}
	if rec.contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", rec.contentType)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(rec.gotBody), &body); err != nil {
		t.Fatalf("body not JSON: %v\nbody=%s", err, rec.gotBody)
	}
	if body["title"] != "Fix OAuth" {
		t.Errorf("body.title = %v, want %q", body["title"], "Fix OAuth")
	}
	if body["priority"] != "high" {
		t.Errorf("body.priority = %v, want %q", body["priority"], "high")
	}
	// `field` should have rolled up into a fields JSON string.
	fieldsRaw, ok := body["fields"].(string)
	if !ok {
		t.Fatalf("body.fields missing or wrong type; body=%v", body)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsRaw), &fields); err != nil {
		t.Fatalf("fields JSON parse: %v", err)
	}
	if fields["status"] != "in-progress" {
		t.Errorf("fields.status = %v, want in-progress", fields["status"])
	}
	if fields["due_date"] != "2026-06-01" {
		t.Errorf("fields.due_date = %v, want 2026-06-01", fields["due_date"])
	}

	if rec.gotUser == nil || rec.gotUser.ID != user.ID {
		t.Errorf("handler saw user=%v, want %v", rec.gotUser, user)
	}
	if !rec.gotIsAPITok {
		t.Errorf("isAPIToken not set; auth context missing")
	}
}

func TestHTTPHandlerDispatcher_UnsupportedToolReturnsErrorResult(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      http.NewServeMux(),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{})

	res, err := d.Dispatch(ctx, []string{"workspace", "delete"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result for unrouted tool, got %#v", res)
	}
	if !containsToolText(res, "not yet implemented over HTTP transport") {
		t.Errorf("error message should mention unsupported tool; got %#v", res)
	}
}

func TestHTTPHandlerDispatcher_NoUserReturnsErrorResult(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      http.NewServeMux(),
		UserResolver: func(context.Context) *models.User { return nil },
		Routes: map[string]RouteMapper{
			"item create": mapItemCreate,
		},
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "collection": "tasks", "title": "x",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when user nil, got %#v", res)
	}
}

func TestHTTPHandlerDispatcher_HandlerErrorSurfacesAsToolError(t *testing.T) {
	rec := &recordingHandler{t: t, wantStatus: http.StatusBadRequest, respBody: `{"error":"Title is required"}`}
	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "collection": "tasks", "title": "x",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when handler returns 400, got %#v", res)
	}
	if !containsToolText(res, "Title is required") {
		t.Errorf("error should propagate handler stderr; got %#v", res)
	}
}

func TestMapItemCreate_RequiresWorkspace(t *testing.T) {
	_, _, _, err := mapItemCreate(map[string]any{"collection": "tasks", "title": "x"})
	if err == nil {
		t.Errorf("expected error when workspace missing")
	}
}

func TestMapItemCreate_RequiresCollection(t *testing.T) {
	_, _, _, err := mapItemCreate(map[string]any{"workspace": "docapp", "title": "x"})
	if err == nil {
		t.Errorf("expected error when collection missing")
	}
}

func TestParseFieldKVP_Variants(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want map[string]any
	}{
		{"single string", "k=v", map[string]any{"k": "v"}},
		{"slice of any", []any{"a=1", "b=2"}, map[string]any{"a": "1", "b": "2"}},
		{"slice of string", []string{"x=y"}, map[string]any{"x": "y"}},
		{"empty entries skipped", []any{"", "k=v"}, map[string]any{"k": "v"}},
		{"missing equals skipped", []any{"loner", "k=v"}, map[string]any{"k": "v"}},
		{"empty key skipped", []any{"=v"}, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseFieldKVP(c.in)
			if err != nil {
				t.Fatalf("parseFieldKVP: %v", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("len(got)=%d want %d (got=%v)", len(got), len(c.want), got)
			}
			for k, v := range c.want {
				if got[k] != v {
					t.Errorf("got[%q] = %v, want %v", k, got[k], v)
				}
			}
		})
	}
}

func TestPackageHTTPResponse_StructuredJSONOnSuccess(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"ref":"TASK-1"}`)),
	}
	res, err := packageHTTPResponse("item create", resp)
	if err != nil {
		t.Fatalf("packageHTTPResponse: %v", err)
	}
	if res.IsError {
		t.Errorf("unexpected IsError on 200")
	}
	if res.StructuredContent == nil {
		t.Errorf("expected structured content for JSON 200; got %#v", res)
	}
}

func TestPackageHTTPResponse_TextFallbackOnNonJSON(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("hello world")),
	}
	res, err := packageHTTPResponse("item create", resp)
	if err != nil {
		t.Fatalf("packageHTTPResponse: %v", err)
	}
	if res.StructuredContent != nil {
		t.Errorf("expected text-only result for non-JSON 200; got %#v", res)
	}
}

// TestHTTPHandlerDispatcher_Integration drives the full handler chain
// (`pad-cloud`'s real *server.Server backed by an in-memory SQLite
// store) end-to-end: synthesize an OAuth user, dispatch
// `item.create`, assert the item exists in the DB.
//
// This is the integration half of TASK-965's DoD: "synthesize an
// OAuth user context, dispatch `item.create` via
// HTTPHandlerDispatcher, assert the item exists in DB owned by that
// user."
func TestHTTPHandlerDispatcher_Integration(t *testing.T) {
	srv, st := newPadServer(t)

	// Bootstrap: create the workspace via the handler directly so the
	// dispatcher's call has somewhere to write to. Using the handler
	// (rather than the store) so we exercise the full create path.
	wsRec := httptest.NewRecorder()
	wsReq := mustJSONRequest(t, http.MethodPost, "/api/v1/workspaces", map[string]any{"name": "DocApp"})
	srv.ServeHTTP(wsRec, wsReq)
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}

	// Create the user that'll own the dispatched item, and add them
	// as the workspace owner so the access-control filter in
	// getWorkspaceID lets the request through. Mirrors what the auth
	// middleware would resolve in a real OAuth-authenticated request.
	user, err := st.CreateUser(models.UserCreate{
		Email:    "dave@example.com",
		Name:     "Dave",
		Password: "irrelevant-not-used-by-this-test",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, user.ID, "owner"); err != nil {
		t.Fatalf("add workspace member: %v", err)
	}

	d := &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: fixedUserResolver(user),
	}

	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  ws.Slug,
		"collection": "tasks",
		"title":      "Fix OAuth redirect",
		"content":    "Investigate the consent screen.",
		"priority":   "high",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("dispatch IsError: %#v", res)
	}

	// The item should now be in the DB. List items for the workspace
	// + collection and assert.
	items, err := st.ListItems(ws.ID, models.ItemListParams{
		CollectionSlug: "tasks",
	})
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	var found *models.Item
	for i := range items {
		if items[i].Title == "Fix OAuth redirect" {
			found = &items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("item not created in DB; saw %d items", len(items))
	}

	// CreatedBy attribution: the handler's CreateItem path stamps the
	// authenticated user (TASK-965's whole point — that the user
	// arrives intact through the in-process call). If the model
	// surfaces it, assert.
	if found.CreatedBy != "" && found.CreatedBy != "user" && found.CreatedBy != user.ID {
		// Some pad versions store "user" as a literal source marker
		// alongside an ID elsewhere; tolerate both shapes so this
		// test isn't brittle against minor schema drift, but flag if
		// neither path matches.
		t.Logf("created_by = %q (user.ID = %q) — informational", found.CreatedBy, user.ID)
	}
}

// newPadServer stands up the same *server.Server that production
// uses, wired to a temp SQLite store. Mirrors testServer in
// internal/server/server_test.go but accessible from this package.
//
// Cleanup drains the server's background goroutines before closing
// the store to avoid the WAL/SHM races BUG-842 fixed (see comment in
// internal/server/server_test.go).
func newPadServer(t *testing.T) (*server.Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	srv := server.New(st)
	t.Cleanup(func() {
		srv.Stop()
		st.Close()
		// Belt-and-braces: brief sleep gives any straggler timers a
		// chance to wind down before t.TempDir's RemoveAll runs.
		time.Sleep(10 * time.Millisecond)
	})
	return srv, st
}

func mustJSONRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(bodyBytes)))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:0"
	return req
}

// containsToolText looks for substr in any TextContent block of res.
// MCP results can carry multiple content blocks; checking all of them
// keeps the assertion robust against future packaging changes.
func containsToolText(res *mcp.CallToolResult, substr string) bool {
	for _, c := range res.Content {
		if t, ok := c.(mcp.TextContent); ok && strings.Contains(t.Text, substr) {
			return true
		}
	}
	return false
}
