package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- resolveAssignName ---

func TestResolveAssignName_PassesThroughWhenMissing(t *testing.T) {
	d := &HTTPHandlerDispatcher{Handler: errorHandler(t, "must not call members"), UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	out, err := d.resolveAssignName(context.Background(), &models.User{ID: "u"}, map[string]any{
		"workspace": "ws", "title": "x",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, has := out["assign"]; has {
		t.Errorf("output should not introduce an assign key; got %v", out)
	}
}

func TestResolveAssignName_PassesThroughWhenAssignEmpty(t *testing.T) {
	d := &HTTPHandlerDispatcher{Handler: errorHandler(t, "must not call members"), UserResolver: fixedUserResolver(&models.User{ID: "u"})}
	out, err := d.resolveAssignName(context.Background(), &models.User{ID: "u"}, map[string]any{
		"workspace": "ws", "assign": "",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if v, _ := out["assigned_user_id"].(string); v != "" {
		t.Errorf("expected no resolution for empty assign; got %v", out)
	}
}

func TestResolveAssignName_ResolvesByName(t *testing.T) {
	h := membersHandler(t, []memberRow{
		{UserID: "u1", UserName: "Dave", UserEmail: "dave@example.com"},
		{UserID: "u2", UserName: "Alice", UserEmail: "alice@example.com"},
	})
	d := &HTTPHandlerDispatcher{Handler: h, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}

	out, err := d.resolveAssignName(context.Background(), &models.User{ID: "caller"}, map[string]any{
		"workspace": "docapp", "assign": "Dave",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["assigned_user_id"] != "u1" {
		t.Errorf("expected u1, got %v", out["assigned_user_id"])
	}
	if _, present := out["assign"]; present {
		t.Errorf("assign key should be removed after resolution: %v", out)
	}
}

func TestResolveAssignName_ResolvesByEmailCaseInsensitive(t *testing.T) {
	h := membersHandler(t, []memberRow{
		{UserID: "u1", UserName: "Dave", UserEmail: "dave@example.com"},
	})
	d := &HTTPHandlerDispatcher{Handler: h, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}

	out, err := d.resolveAssignName(context.Background(), &models.User{ID: "caller"}, map[string]any{
		"workspace": "docapp", "assign": "DAVE@example.com",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["assigned_user_id"] != "u1" {
		t.Errorf("expected u1 from case-insensitive email match, got %v", out["assigned_user_id"])
	}
}

func TestResolveAssignName_ErrorsWhenNoMatch(t *testing.T) {
	h := membersHandler(t, []memberRow{{UserID: "u1", UserName: "Alice", UserEmail: "alice@example.com"}})
	d := &HTTPHandlerDispatcher{Handler: h, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}

	_, err := d.resolveAssignName(context.Background(), &models.User{ID: "caller"}, map[string]any{
		"workspace": "docapp", "assign": "Bob",
	})
	if err == nil {
		t.Fatalf("expected error for unmatched assignee")
	}
	if !strings.Contains(err.Error(), `"Bob"`) {
		t.Errorf("error should mention the unmatched name; got %v", err)
	}
}

func TestResolveAssignName_ExplicitIDWins(t *testing.T) {
	// If the caller passes both --assign Dave and an explicit
	// assigned_user_id, the explicit ID wins and we skip the
	// members-lookup entirely.
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not call members when assigned_user_id is explicit"),
		UserResolver: fixedUserResolver(&models.User{ID: "caller"}),
	}
	out, err := d.resolveAssignName(context.Background(), &models.User{ID: "caller"}, map[string]any{
		"workspace": "docapp", "assign": "Dave", "assigned_user_id": "explicit-uuid",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["assigned_user_id"] != "explicit-uuid" {
		t.Errorf("expected explicit ID to win; got %v", out)
	}
	if _, present := out["assign"]; present {
		t.Errorf("assign key should be cleared even when ID-only path runs: %v", out)
	}
}

func TestResolveAssignName_ErrorsWhenMembersEndpointFails(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"forbidden"}`))
	})
	d := &HTTPHandlerDispatcher{Handler: h, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}

	_, err := d.resolveAssignName(context.Background(), &models.User{ID: "caller"}, map[string]any{
		"workspace": "docapp", "assign": "Dave",
	})
	if err == nil {
		t.Fatalf("expected error when members endpoint returns 403")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error should propagate status: %v", err)
	}
}

// --- Dispatch preprocess for assign ---

func TestDispatch_PreprocessesAssignForItemCreate(t *testing.T) {
	// End-to-end through Dispatch: input has --assign Dave; Dispatch
	// resolves via members endpoint, mapItemCreate sees the resolved
	// assigned_user_id, and the create POST carries the UUID.
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/members", membersHandler(t, []memberRow{
		{UserID: "u-dave", UserName: "Dave", UserEmail: "dave@example.com"},
	}).ServeHTTP)
	mux.Handle("/api/v1/workspaces/docapp/collections/tasks/items", captured)

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "Fix oauth",
		"assign":     "Dave",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if captured.requestCount != 1 {
		t.Fatalf("expected exactly 1 captured create request, got %d", captured.requestCount)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, captured.lastBody)
	}
	if body["assigned_user_id"] != "u-dave" {
		t.Errorf("create body did not carry resolved user id: %v", body)
	}
}

func TestDispatch_PreprocessAssignFailureSurfacesAsToolError(t *testing.T) {
	// When resolution fails (no matching member), Dispatch must
	// return an IsError result rather than dispatching the create
	// with an empty assignee — silently posting without the
	// assignment would be the worst outcome.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/members", membersHandler(t, []memberRow{
		{UserID: "u-alice", UserName: "Alice", UserEmail: "alice@example.com"},
	}).ServeHTTP)
	createCount := 0
	mux.HandleFunc("/api/v1/workspaces/docapp/collections/tasks/items", func(_ http.ResponseWriter, _ *http.Request) {
		createCount++
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "collection": "tasks", "title": "x",
		"assign": "Bob",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError when assignee resolution fails; got %#v", res)
	}
	if createCount != 0 {
		t.Errorf("create handler must not run after preprocess failure; ran %d times", createCount)
	}
}

func TestDispatch_PreprocessSkippedForCommandsNotInAllowlist(t *testing.T) {
	// `item show` doesn't take --assign — Dispatch must not call
	// the members endpoint just because input happens to carry an
	// `assign` key (e.g. from a stale schema cache).
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/members", func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("members endpoint must not be called for item.show")
	})
	mux.Handle("/api/v1/workspaces/docapp/items/TASK-5", captured)

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
		"assign": "Dave", // ignored
	})
	res, err := d.Dispatch(ctx, []string{"item", "show"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
}

// --- dispatchItemUpdate ---

func TestDispatchItemUpdate_MergesFieldsWithExisting(t *testing.T) {
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			// Existing item with two fields set.
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"ref":"TASK-5",
				"fields":"{\"status\":\"open\",\"priority\":\"medium\",\"category\":\"infra\"}"
			}`))
		case http.MethodPatch:
			captured.ServeHTTP(w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5","status":"updated"}`))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	})

	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp",
		"ref":       "TASK-5",
		"status":    "in-progress",     // overrides existing "open"
		"comment":   "Started work",    // top-level update
		"field":     []any{"effort=l"}, // adds a new key
	})
	res, err := d.Dispatch(ctx, []string{"item", "update"}, nil)
	if err != nil || res.IsError {
		t.Fatalf("Dispatch err=%v IsError=%v: %#v", err, res != nil && res.IsError, res)
	}
	if captured.requestCount != 1 {
		t.Fatalf("expected 1 PATCH, got %d", captured.requestCount)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v\n%s", err, captured.lastBody)
	}
	if body["comment"] != "Started work" {
		t.Errorf("comment lost: %v", body)
	}
	fields := map[string]any{}
	if s, ok := body["fields"].(string); ok {
		_ = json.Unmarshal([]byte(s), &fields)
	} else {
		t.Fatalf("fields not a string in body: %v", body)
	}
	want := map[string]string{
		"status":   "in-progress",
		"priority": "medium", // existing, preserved
		"category": "infra",  // existing, preserved
		"effort":   "l",      // newly added via --field
	}
	for k, v := range want {
		if got := fields[k]; got != v {
			t.Errorf("merged fields[%q] = %v, want %v", k, got, v)
		}
	}
}

func TestDispatchItemUpdate_NoFieldChangesSkipsFieldsMerge(t *testing.T) {
	// Updating only top-level keys (title / content / comment)
	// without any field-level changes must not include `fields` in
	// the PATCH body. Sending a fields object would still go through
	// the handler's schema validator (cheap but unnecessary), and a
	// no-op update of fields would churn the audit log.
	captured := newRequestCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5","fields":"{\"status\":\"open\"}"}`))
		case http.MethodPatch:
			captured.ServeHTTP(w, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ref":"TASK-5"}`))
		}
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
		"comment": "noted",
	})
	if _, err := d.Dispatch(ctx, []string{"item", "update"}, nil); err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured.lastBody), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := body["fields"]; present {
		t.Errorf("fields should be omitted when no field changes; got %v", body)
	}
}

func TestDispatchItemUpdate_SurfacesPrefetch404(t *testing.T) {
	// If the GET prefetch fails (item not found), the PATCH must
	// not run. The 404 surfaces to the agent as a tool error.
	patchCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/workspaces/docapp/items/TASK-5", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCount++
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"item not found"}`))
	})
	d := &HTTPHandlerDispatcher{Handler: mux, UserResolver: fixedUserResolver(&models.User{ID: "caller"})}
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "ref": "TASK-5", "status": "done",
	})
	res, err := d.Dispatch(ctx, []string{"item", "update"}, nil)
	if err != nil {
		t.Fatalf("Dispatch err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result on 404 prefetch; got %#v", res)
	}
	if patchCount != 0 {
		t.Errorf("PATCH must not run after 404 prefetch; ran %d times", patchCount)
	}
}

func TestDispatchItemUpdate_RequiresWorkspaceAndRef(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "must not reach handler when workspace/ref missing"),
		UserResolver: fixedUserResolver(&models.User{ID: "caller"}),
	}
	for _, missing := range []string{"workspace", "ref"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			input := map[string]any{"workspace": "ws", "ref": "TASK-1", "status": "done"}
			delete(input, missing)
			ctx := WithDispatchInput(context.Background(), input)
			res, err := d.Dispatch(ctx, []string{"item", "update"}, nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError when %s missing; got %#v", missing, res)
			}
		})
	}
}

// --- Integration smoke against the real server ---

// TestHTTPHandlerDispatcher_Integration_ItemUpdateAndAssignResolution
// drives item.create → item.update through the real *server.Server,
// asserting that:
//
//   - --assign Dave gets resolved via workspace.members → user UUID,
//     and the item lands assigned to Dave.
//   - item.update preserves existing fields while updating status —
//     i.e. the read-modify-write merge actually keeps the priority +
//     category set at create time.
//
// This is the behavioural integration the unit tests can't cover —
// they stub out the handler. Here we run against the real chi
// router + SQLite store so a regression in handler shape, store
// schema, or middleware ordering would fail this test.
func TestHTTPHandlerDispatcher_Integration_ItemUpdateAndAssignResolution(t *testing.T) {
	srv, st := newPadServer(t)

	// Bootstrap workspace + two users + memberships.
	wsRec := doJSONReq(t, srv, http.MethodPost, "/api/v1/workspaces",
		map[string]any{"name": "DocApp"})
	if wsRec.Code != http.StatusCreated {
		t.Fatalf("create workspace: %d %s", wsRec.Code, wsRec.Body.String())
	}
	var ws models.Workspace
	if err := json.Unmarshal(wsRec.Body.Bytes(), &ws); err != nil {
		t.Fatalf("decode workspace: %v", err)
	}

	owner, err := st.CreateUser(models.UserCreate{Email: "dave@example.com", Name: "Dave", Password: "x"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	assignee, err := st.CreateUser(models.UserCreate{Email: "alice@example.com", Name: "Alice", Password: "x"})
	if err != nil {
		t.Fatalf("create assignee: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}
	if err := st.AddWorkspaceMember(ws.ID, assignee.ID, "editor"); err != nil {
		t.Fatalf("add editor: %v", err)
	}

	d := &HTTPHandlerDispatcher{
		Handler:      srv,
		UserResolver: fixedUserResolver(owner),
	}

	// Create an item with --assign Alice — exercises Dispatch's
	// preprocess + the members-lookup against the real handler.
	createCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace":  ws.Slug,
		"collection": "tasks",
		"title":      "Smoke",
		"priority":   "high",
		"category":   "infra",
		"assign":     "Alice",
	})
	createRes, err := d.Dispatch(createCtx, []string{"item", "create"}, nil)
	if err != nil || createRes.IsError {
		t.Fatalf("item create: err=%v IsError=%v: %#v", err, createRes != nil && createRes.IsError, createRes)
	}
	created, ok := createRes.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("create result not structured: %#v", createRes.StructuredContent)
	}
	ref, _ := created["ref"].(string)
	if ref == "" {
		t.Fatalf("created item missing ref: %#v", created)
	}
	if got, _ := created["assigned_user_id"].(string); got != assignee.ID {
		t.Errorf("created item not assigned to Alice; got %q want %q", got, assignee.ID)
	}

	// Update only the status. The priority + category set at create
	// time MUST survive — that's what the read-modify-write merge
	// guarantees vs. the handler treating Fields as a complete
	// replacement.
	updateCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug,
		"ref":       ref,
		"status":    "in-progress",
		"comment":   "Picked up.",
	})
	updateRes, err := d.Dispatch(updateCtx, []string{"item", "update"}, nil)
	if err != nil || updateRes.IsError {
		t.Fatalf("item update: err=%v IsError=%v: %#v", err, updateRes != nil && updateRes.IsError, updateRes)
	}
	updated, ok := updateRes.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("update result not structured: %#v", updateRes.StructuredContent)
	}
	fields, _ := updated["fields"].(string)
	var fieldMap map[string]any
	if err := json.Unmarshal([]byte(fields), &fieldMap); err != nil {
		t.Fatalf("decode fields: %v\n%s", err, fields)
	}
	want := map[string]string{
		"status":   "in-progress",
		"priority": "high",  // pre-existing, must survive RMW
		"category": "infra", // pre-existing, must survive RMW
	}
	for k, v := range want {
		if got, _ := fieldMap[k].(string); got != v {
			t.Errorf("fields[%q] = %q, want %q (full fields: %v)", k, got, v, fieldMap)
		}
	}
}

func TestRouteTable_ContainsWorkspaceMembers(t *testing.T) {
	if _, ok := routeTable["workspace members"]; !ok {
		t.Errorf("routeTable should include `workspace members` after TASK-967")
	}
	m, p, _, err := routeTable["workspace members"](map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/members" {
		t.Errorf("path = %q", p)
	}
}

// --- Test fixtures ---

// memberRow is the test-side mirror of the (subset of) fields the
// resolveAssignName lookup reads from the workspace-members response.
type memberRow struct {
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	UserEmail string `json:"user_email"`
}

// membersHandler returns an http.Handler that responds to any path
// with the standard `{members:[...], invitations:[]}` shape using
// the supplied rows.
func membersHandler(t *testing.T, rows []memberRow) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"members":     rows,
			"invitations": []any{},
		}
		_ = json.NewEncoder(w).Encode(body)
	})
}

// errorHandler fails the test if invoked. Used to assert that a
// code path doesn't hit the handler at all.
func errorHandler(t *testing.T, msg string) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Errorf("unexpected handler call: %s", msg)
	})
}

// requestCapture is a tiny helper that records the last request +
// counts how many times it was called. Compatible with http.Handler
// so it slots into mux.Handle / mux.HandleFunc directly.
type requestCapture struct {
	requestCount int
	lastMethod   string
	lastPath     string
	lastBody     string
}

func newRequestCapture() *requestCapture { return &requestCapture{} }

func (c *requestCapture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.requestCount++
	c.lastMethod = r.Method
	c.lastPath = r.URL.Path
	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		c.lastBody = string(body)
	}
	if w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodPost {
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	_, _ = w.Write([]byte(`{}`))
}
