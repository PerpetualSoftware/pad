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

	// GET / DELETE requests synthesized by buildHTTPRequest have nil
	// r.Body (no payload to send). Guard so the test handler is
	// usable for both the create-with-body and the list-without-body
	// dispatcher paths.
	if r.Body != nil {
		body, _ := io.ReadAll(r.Body)
		h.gotBody = string(body)
		_ = r.Body.Close()
	}

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
		"status":     "in-progress",
		"category":   "infrastructure",
		"parent":     "PLAN-3",
		"field":      []any{"due_date=2026-06-01"},
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
	// status/priority/category/parent must NOT live at the top level
	// (the handler ignores them there); they belong in fields. This
	// guards against the regression Codex caught in PR review round 1.
	for _, leaked := range []string{"status", "priority", "category", "parent"} {
		if _, present := body[leaked]; present {
			t.Errorf("body.%s leaked to top level; should be inside body.fields", leaked)
		}
	}
	fieldsRaw, ok := body["fields"].(string)
	if !ok {
		t.Fatalf("body.fields missing or wrong type; body=%v", body)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsRaw), &fields); err != nil {
		t.Fatalf("fields JSON parse: %v", err)
	}
	wantFields := map[string]any{
		"status":   "in-progress",
		"priority": "high",
		"category": "infrastructure",
		"parent":   "PLAN-3",
		"due_date": "2026-06-01",
	}
	for k, want := range wantFields {
		if got := fields[k]; got != want {
			t.Errorf("fields.%s = %v, want %v", k, got, want)
		}
	}

	if rec.gotUser == nil || rec.gotUser.ID != user.ID {
		t.Errorf("handler saw user=%v, want %v", rec.gotUser, user)
	}
	if !rec.gotIsAPITok {
		t.Errorf("isAPIToken not set; auth context missing")
	}
}

// TestHTTPHandlerDispatcher_ScopeEnforcement_RejectsReadScopeOnWrites
// pins the security fix from Codex review #369 round 1: a PAT with
// scopes `["read"]` must not drive write tools through the in-process
// dispatcher.
//
// Background: MCPBearerAuth used to skip tokenScopeAllows entirely.
// The dispatcher's synthesized request bypasses TokenAuth's chain-
// level check (because WithCurrentUser is already set), so without
// the executeRequest scope re-check, a read-scoped token could POST
// item create / PATCH item update / DELETE item — silently. The
// fix stashes scopes in MCPBearerAuth via server.WithTokenScopes
// and re-checks per synthesized request here.
func TestHTTPHandlerDispatcher_ScopeEnforcement_RejectsReadScopeOnWrites(t *testing.T) {
	user := &models.User{ID: "user-1", Name: "Dave", Email: "dave@example.com"}
	rec := &recordingHandler{t: t}

	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: fixedUserResolver(user),
	}

	// Read-only scope in context — simulates what MCPBearerAuth
	// stashes for a `["read"]` PAT.
	ctx := server.WithTokenScopes(context.Background(), `["read"]`)
	ctx = WithDispatchInput(ctx, map[string]any{
		"workspace":  "docapp",
		"collection": "tasks",
		"title":      "Should Be Rejected",
	})

	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected IsError result for read-scoped POST; got success: %#v", res)
	}
	if rec.requestCount != 0 {
		t.Errorf("handler must not be invoked when scope check fails (defense in depth); got %d calls", rec.requestCount)
	}
	// The error envelope must mention permission_denied so agents can
	// branch on the code rather than parse free-text.
	var msg string
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcp.TextContent); ok {
			msg = tc.Text
		}
	}
	if !strings.Contains(msg, "permission_denied") {
		t.Errorf("error envelope missing permission_denied marker: %q", msg)
	}
}

// TestHTTPHandlerDispatcher_ScopeEnforcement_AllowsReadScopeOnReads
// is the positive complement: a `["read"]` PAT can still call
// read-only tools (project dashboard, item show, etc.) because their
// synthesized HTTP method is GET.
func TestHTTPHandlerDispatcher_ScopeEnforcement_AllowsReadScopeOnReads(t *testing.T) {
	user := &models.User{ID: "user-1", Name: "Dave", Email: "dave@example.com"}
	rec := &recordingHandler{t: t, wantStatus: http.StatusOK, respBody: `{"items":[]}`}

	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: fixedUserResolver(user),
	}

	ctx := server.WithTokenScopes(context.Background(), `["read"]`)
	ctx = WithDispatchInput(ctx, map[string]any{"workspace": "docapp"})

	// item list resolves to GET — read scope must allow it.
	res, err := d.Dispatch(ctx, []string{"item", "list"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("read-scoped GET should succeed; got error: %#v", res)
	}
	if rec.gotMethod != http.MethodGet {
		t.Errorf("expected GET (precondition for the scope-allow path), got %q", rec.gotMethod)
	}
}

// TestHTTPHandlerDispatcher_ScopeEnforcement_BulkUpdateBlockedOnReadScope
// pins the round-2 fix Codex caught: bulk-update used to call
// buildAuthedRequest + d.Handler.ServeHTTP directly for the per-item
// PATCH, bypassing the executeRequest scope check that round 1
// added. Centralizing the check inside buildAuthedRequest closes the
// gap — every synthesized request, no matter the caller, is gated.
//
// The expected behavior with `["read"]` scope: each ref's GET
// prefetch succeeds (read scope allows GET), but the subsequent
// PATCH fails at request-build time with permission_denied. The
// per-item error envelope carries the message; the bulk operation
// returns successfully with all-errors recorded (the "no abort on
// per-item failure" contract is unchanged).
func TestHTTPHandlerDispatcher_ScopeEnforcement_BulkUpdateBlockedOnReadScope(t *testing.T) {
	user := &models.User{ID: "user-1"}

	// Test handler: respond 200 with a minimal item JSON to GET (so
	// the prefetch parses), 200 to anything else. We only care that
	// PATCH never reaches the handler — the scope gate must reject
	// before ServeHTTP runs.
	patchSeen := false
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchSeen = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"fields":"{\"status\":\"open\"}"}`))
	})

	d := &HTTPHandlerDispatcher{
		Handler:      h,
		UserResolver: fixedUserResolver(user),
	}

	ctx := server.WithTokenScopes(context.Background(), `["read"]`)

	res, err := d.dispatchItemBulkUpdate(ctx, map[string]any{
		"workspace": "docapp",
		"ref":       []any{"TASK-1", "TASK-2"},
		"status":    "done",
	}, user)
	if err != nil {
		t.Fatalf("dispatchItemBulkUpdate: %v", err)
	}
	if patchSeen {
		t.Fatal("PATCH must not reach the handler when scope check fails (regression: round-2 fix removed the gap)")
	}
	if res.IsError {
		t.Fatalf("bulk-update returns success with per-item errors recorded; got top-level IsError: %#v", res)
	}

	// Pull the structured result out and confirm both refs carry a
	// permission_denied error in their per-item entry. We accept any
	// reasonable wrapping of "permission_denied" in the message string.
	var content string
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcp.TextContent); ok {
			content = tc.Text
		}
	}
	for _, ref := range []string{"TASK-1", "TASK-2"} {
		if !strings.Contains(content, ref) {
			t.Errorf("expected per-item entry for %s in result; got %q", ref, content)
		}
	}
	if !strings.Contains(content, "permission_denied") {
		t.Errorf("expected permission_denied in per-item errors; got %q", content)
	}
}

// TestHTTPHandlerDispatcher_ScopeEnforcement_NoScopeContextAllows
// pins the legacy/empty-context behaviour: when no scope is stashed
// (e.g. the dispatcher is used outside the MCP middleware path, or
// for tests that don't simulate a token), the scope check defers to
// tokenScopeAllows's "" → allow-all branch. Without this, every
// existing dispatcher test would break.
func TestHTTPHandlerDispatcher_ScopeEnforcement_NoScopeContextAllows(t *testing.T) {
	user := &models.User{ID: "user-1", Name: "Dave", Email: "dave@example.com"}
	rec := &recordingHandler{t: t}

	d := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: fixedUserResolver(user),
	}

	// No WithTokenScopes call — empty scope falls through.
	ctx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": "docapp", "collection": "tasks", "title": "OK",
	})
	res, err := d.Dispatch(ctx, []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Fatalf("empty scope must allow (legacy); got error: %#v", res)
	}
}

func TestMapItemCreate_ExplicitFieldOverridesNamedFlag(t *testing.T) {
	// Last-write-wins: an explicit --field status=blocked should
	// override a separate --status=in-progress on the same call.
	// This matches the CLI's behaviour and lets agents reach custom
	// schema-defined fields without us teaching the mapper about each.
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"status": "in-progress",
		"field":  []any{"status=blocked"},
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload["fields"].(string)), &fields); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	if fields["status"] != "blocked" {
		t.Errorf("fields.status = %v, want %q (--field overrides --status)", fields["status"], "blocked")
	}
}

func TestMapItemCreate_NormalizesCollectionAliases(t *testing.T) {
	// MCP callers may mirror documented CLI shapes ("item create task X")
	// using singular/short collection names. The CLI normalizes via
	// collections.NormalizeSlug; the dispatcher does the same so the
	// HTTP transport doesn't 404 on a call that works through
	// ExecDispatcher. Locked into a test so the parity doesn't drift.
	cases := map[string]string{
		"task":       "tasks",
		"idea":       "ideas",
		"doc":        "docs",
		"plan":       "plans",
		"bug":        "bugs",
		"convention": "conventions",
		"tasks":      "tasks", // already-canonical: no change
		"my-custom":  "my-custom",
	}
	for in, wantSlug := range cases {
		t.Run(in, func(t *testing.T) {
			_, path, _, err := mapItemCreate(map[string]any{
				"workspace": "ws", "collection": in, "title": "x",
			})
			if err != nil {
				t.Fatalf("mapItemCreate: %v", err)
			}
			wantPath := "/api/v1/workspaces/ws/collections/" + wantSlug + "/items"
			if path != wantPath {
				t.Errorf("path = %q, want %q", path, wantPath)
			}
		})
	}
}

func TestMapItemCreate_PassesThroughResolvedAgentRoleID(t *testing.T) {
	// Slug → ID resolution moved up to Dispatch in TASK-968 (the
	// preprocess step rewrites `role: <slug>` to `agent_role_id:
	// <uuid>` via the agent-roles endpoint). The mapper itself just
	// trusts the resolved input. This test asserts that the
	// pass-through for `agent_role_id` keeps working — the Dispatch-
	// level preprocess can't actually run without a Handler, so the
	// route table contract is "agent_role_id flows through to the
	// payload."
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"agent_role_id": "role-uuid-101",
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["agent_role_id"] != "role-uuid-101" {
		t.Errorf("agent_role_id should pass through to payload; got %v", payload)
	}
}

func TestMapItemCreate_LiftsAgentRoleIDFromFieldKVPToColumn(t *testing.T) {
	// Codex review #345 round 3: the MCP tool schema only exposes
	// `--role` (and the `--field` escape hatch), not a top-level
	// `agent_role_id`. The error message tells agents to use
	// `--field agent_role_id=<uuid>`; the lift logic below makes
	// that workaround reachable by recognizing column keys in the
	// fields blob and moving them to the top-level payload before
	// PATCH/POST.
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"field": []any{"agent_role_id=role-uuid-789", "effort=l"},
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["agent_role_id"] != "role-uuid-789" {
		t.Errorf("agent_role_id not lifted to top level; got %v", payload)
	}
	// And not in the fields blob.
	fields := map[string]any{}
	if s, _ := payload["fields"].(string); s != "" {
		_ = json.Unmarshal([]byte(s), &fields)
	}
	if _, present := fields["agent_role_id"]; present {
		t.Errorf("agent_role_id should be lifted out of fields blob: %v", fields)
	}
	// Other --field entries (effort=l) stay in the blob.
	if fields["effort"] != "l" {
		t.Errorf("non-column --field key should remain in fields blob: %v", fields)
	}
}

func TestMapItemCreate_LiftsAssignedUserIDFromFieldKVP(t *testing.T) {
	// Companion to the agent_role_id lift — assigned_user_id is the
	// other column key columnFieldKeys recognizes.
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"field": []any{"assigned_user_id=user-uuid-12"},
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["assigned_user_id"] != "user-uuid-12" {
		t.Errorf("assigned_user_id not lifted: %v", payload)
	}
}

func TestMapItemCreate_TopLevelAgentRoleIDWinsOverFieldKVP(t *testing.T) {
	// Belt-and-braces: if both a top-level agent_role_id and a
	// --field agent_role_id are set, the top-level wins. Avoids
	// surprising callers who mix paths.
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"agent_role_id": "explicit-uuid",
		"field":         []any{"agent_role_id=lift-uuid"},
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["agent_role_id"] != "explicit-uuid" {
		t.Errorf("explicit top-level agent_role_id should win; got %v", payload["agent_role_id"])
	}
	// Lift still removes the duplicate from fields so the value
	// doesn't appear in two places.
	fields := map[string]any{}
	if s, _ := payload["fields"].(string); s != "" {
		_ = json.Unmarshal([]byte(s), &fields)
	}
	if _, present := fields["agent_role_id"]; present {
		t.Errorf("agent_role_id should be removed from fields blob even when ignored: %v", fields)
	}
}

func TestMapItemCreate_PassesThroughAgentRoleID(t *testing.T) {
	// agent_role_id (UUID) is the ItemCreate column the handler
	// writes to. Agents that know the UUID (e.g. from a prior
	// `role list` call) can set it without --role slug resolution.
	// Codex review #345 round 2 caught the misleading error message
	// pointing at `--field agent_role_id=<uuid>` — that goes into
	// the fields JSON blob, NOT the column. The fix is to pass the
	// top-level `agent_role_id` through directly; this test pins
	// that path so the workaround the error message points at
	// actually works.
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"agent_role_id": "role-uuid-789",
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["agent_role_id"] != "role-uuid-789" {
		t.Errorf("agent_role_id not passed through: %v", payload)
	}
	// And specifically not in the fields blob (where the misleading
	// `--field agent_role_id=...` workaround would have put it).
	if fields, _ := payload["fields"].(string); fields != "" {
		t.Errorf("agent_role_id leaked into fields blob: %v", fields)
	}
}

func TestMapItemCreate_PassesThroughResolvedAssignedUserID(t *testing.T) {
	// TASK-967: --assign is preprocessed at the dispatcher level
	// (resolveAssignName) before the mapper runs. By the time
	// mapItemCreate sees the input, only `assigned_user_id` should
	// be present; the mapper passes it through to the body.
	_, _, body, err := mapItemCreate(map[string]any{
		"workspace": "ws", "collection": "tasks", "title": "x",
		"assigned_user_id": "user-uuid-123",
	})
	if err != nil {
		t.Fatalf("mapItemCreate: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if payload["assigned_user_id"] != "user-uuid-123" {
		t.Errorf("assigned_user_id not passed through: %v", payload)
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
	// Pre-TASK-1077 the handler's raw response body ("Title is
	// required") was passed through into the error envelope's hint.
	// Post-fix the body is a non-envelope JSON ({"error":"Title is
	// required"} — note: error is a string, not an object) so
	// extractUpstreamMessage's safe-fallback drops it. The envelope
	// still surfaces the validation_failed code + the route + a
	// generic recovery hint, so agents know what to do; only the raw
	// body content is dropped.
	//
	// If the upstream handler emits the structured shape
	// {"error":{"message":"Title is required"}}, the inner message
	// IS lifted into the hint — see TestExtractUpstreamMessage's
	// envelope happy path. The pad codebase's writeError emits that
	// shape; the bare-string shape this test uses is a deliberate
	// anti-pattern repro.
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("expected ErrorEnvelope, got %T", res.StructuredContent)
	}
	if env.Error.Code != ErrValidationFailed {
		t.Errorf("expected validation_failed code, got %q", env.Error.Code)
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
	res, err := packageHTTPResponse(context.Background(), "item create", resp, nil)
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
	res, err := packageHTTPResponse(context.Background(), "item create", resp, nil)
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

	// Source attribution: HTTPHandlerDispatcher must persist source="cli"
	// (matching ExecDispatcher) so dashboard/standup/audit views
	// attribute the change correctly. Codex review caught this one in
	// round 2 — the synthesized request has no Authorization header,
	// so actorFromRequest now honors the ctxIsAPIToken context flag
	// to derive source.
	if found.Source != "cli" {
		t.Errorf("Source = %q, want %q (HTTPHandlerDispatcher should mirror CLI attribution)",
			found.Source, "cli")
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
