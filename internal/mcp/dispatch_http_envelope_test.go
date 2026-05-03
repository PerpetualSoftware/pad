package mcp

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// =====================================================================
// HTTP error envelope tests (TASK-977)
//
// Pin the contract that classifyHTTPStatus + packageHTTPResponse
// produce closed-set ErrorCode envelopes for every documented HTTP
// status. The tests double as the test plan from the task spec:
//
//   | HTTP status | ErrorCode           |
//   | 401         | auth_required       |
//   | 403         | permission_denied   |
//   | 404 (item)  | item_not_found      |
//   | 404 (ws)    | unknown_workspace   |
//   | 409         | conflict            |
//   | 422         | validation_failed   |
//   | 5xx         | server_error        |
//
// Plus the privacy-filter property: unknown_workspace's
// available_workspaces is filtered by the OAuth token allow-list.
// =====================================================================

// fakeStore implements oauthWorkspaceListerStore for unit tests.
// One method, no DB; tests can wire deterministic membership lists
// without spinning up SQLite.
type fakeStore struct {
	workspaces []WorkspaceHint
	err        error
}

func (f *fakeStore) GetUserWorkspaces(_ string) ([]WorkspaceHint, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]WorkspaceHint, len(f.workspaces))
	copy(out, f.workspaces)
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────
// 401 / 403 / 404 / 409 / 422 / 5xx envelope round-trips
// ─────────────────────────────────────────────────────────────────────

func TestClassifyHTTPStatus_AuthRequiredOn401(t *testing.T) {
	res := classifyHTTPStatus(context.Background(), "item create", 401, []byte(`{"error":{"message":"token expired"}}`), nil)
	if !res.IsError {
		t.Fatal("IsError must be true on 401")
	}
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("structured content: got %T, want ErrorEnvelope", res.StructuredContent)
	}
	if env.Error.Code != ErrAuthRequired {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrAuthRequired)
	}
}

func TestClassifyHTTPStatus_PermissionDeniedOn403(t *testing.T) {
	res := classifyHTTPStatus(context.Background(), "item update", 403,
		[]byte(`{"error":{"message":"insufficient role"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrPermissionDenied {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrPermissionDenied)
	}
}

func TestClassifyHTTPStatus_ItemNotFoundOn404Generic(t *testing.T) {
	// Body doesn't mention "workspace" → defaults to item_not_found.
	res := classifyHTTPStatus(context.Background(), "item show", 404,
		[]byte(`{"error":{"message":"item not found"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrItemNotFound {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrItemNotFound)
	}
}

func TestClassifyHTTPStatus_UnknownWorkspaceOn404WorkspaceBody(t *testing.T) {
	res := classifyHTTPStatus(context.Background(), "item list", 404,
		[]byte(`{"error":{"message":"workspace 'foo' not visible"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrUnknownWorkspace {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrUnknownWorkspace)
	}
	// The slug should be extracted from the body.
	if !strings.Contains(env.Error.Message, "foo") {
		t.Errorf("expected slug 'foo' in message; got %q", env.Error.Message)
	}
}

func TestClassifyHTTPStatus_ConflictOn409(t *testing.T) {
	res := classifyHTTPStatus(context.Background(), "item update", 409,
		[]byte(`{"error":{"message":"version mismatch"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrConflict {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrConflict)
	}
}

func TestClassifyHTTPStatus_ValidationOn422(t *testing.T) {
	res := classifyHTTPStatus(context.Background(), "item create", 422,
		[]byte(`{"error":{"message":"title is required"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrValidationFailed {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrValidationFailed)
	}
}

func TestClassifyHTTPStatus_ValidationOn400(t *testing.T) {
	// 400 also maps to validation_failed (the handler's StatusBadRequest
	// path is semantically equivalent for input rejection).
	res := classifyHTTPStatus(context.Background(), "item create", 400,
		[]byte(`{"error":{"message":"invalid json"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrValidationFailed {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrValidationFailed)
	}
}

func TestClassifyHTTPStatus_ServerErrorOn500(t *testing.T) {
	// TASK-1078: 5xx now classifies as upstream_error (transient
	// backend failure), distinct from server_error (catch-all for
	// dispatcher internal failures and un-mapped 4xx). The split lets
	// agents tell "retry the upstream" from "fix the request" /
	// "file a bug."
	res := classifyHTTPStatus(context.Background(), "item create", 500,
		[]byte(`{"error":{"message":"db connection failed"}}`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrUpstreamError {
		t.Errorf("code: got %q, want %q", env.Error.Code, ErrUpstreamError)
	}
}

func TestClassifyHTTPStatus_OtherClientStatusFallsToServerError(t *testing.T) {
	// 418 has no taxonomy slot — fall through to server_error rather
	// than silently promote to validation_failed (which would mislead
	// callers that the input was at fault).
	res := classifyHTTPStatus(context.Background(), "item create", 418,
		[]byte(`teapot`), nil)
	env := res.StructuredContent.(ErrorEnvelope)
	if env.Error.Code != ErrServerError {
		t.Errorf("418: got code %q, want %q (other 4xx → server_error)",
			env.Error.Code, ErrServerError)
	}
}

// ─────────────────────────────────────────────────────────────────────
// Privacy filter: available_workspaces must be filtered by the
// OAuth token's allow-list (TASK-977 core property).
// ─────────────────────────────────────────────────────────────────────

// TestUnknownWorkspace_AvailableWorkspaces_FilteredByAllowList is the
// core privacy invariant: when a token's allow-list is [alpha, beta]
// but the user is also a member of gamma + delta, the
// unknown_workspace envelope's available_workspaces hint MUST list
// only [alpha, beta].
//
// Without this filter, an attacker controlling an OAuth client could
// hit any workspace slug the user is a member of, get the unknown_
// workspace envelope, and read OFF the user's full workspace list
// from available_workspaces — defeating the consent UI's "only
// these workspaces" choice.
func TestUnknownWorkspace_AvailableWorkspaces_FilteredByAllowList(t *testing.T) {
	store := &fakeStore{
		workspaces: []WorkspaceHint{
			{Slug: "alpha", Name: "Alpha"},
			{Slug: "beta", Name: "Beta"},
			{Slug: "gamma", Name: "Gamma"}, // user is a member, NOT in allow-list
			{Slug: "delta", Name: "Delta"}, // same
		},
	}
	lister := &oauthWorkspaceLister{store: store}

	// User is on context (MCPBearerAuth's WithCurrentUser) AND token
	// allow-list = [alpha, beta] (sub-PR E's WithTokenAllowedWorkspaces).
	ctx := server.WithCurrentUser(context.Background(), fakeUser("user-1"))
	ctx = server.WithTokenAllowedWorkspaces(ctx, []string{"alpha", "beta"})

	res := classifyHTTPStatus(ctx, "item show", 404,
		[]byte(`{"error":{"message":"workspace 'epsilon' not visible"}}`), lister)
	env := res.StructuredContent.(ErrorEnvelope)

	if env.Error.Code != ErrUnknownWorkspace {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrUnknownWorkspace)
	}
	got := slugSet(env.Error.AvailableWorkspaces)
	want := map[string]bool{"alpha": true, "beta": true}
	if len(got) != len(want) {
		t.Fatalf("available_workspaces leaked: got %v, want exactly [alpha beta]",
			env.Error.AvailableWorkspaces)
	}
	for slug := range want {
		if !got[slug] {
			t.Errorf("missing expected slug %q from filtered allow-list", slug)
		}
	}
	for slug := range got {
		if !want[slug] {
			t.Errorf("PRIVACY LEAK: slug %q in available_workspaces but NOT in token allow-list", slug)
		}
	}
}

// TestUnknownWorkspace_Wildcard_NoFilterApplied verifies that a
// wildcard allow-list (`["*"]`) returns ALL the user's workspaces
// in available_workspaces. The user explicitly granted "any" at
// consent time, so no per-slug filter applies.
func TestUnknownWorkspace_Wildcard_NoFilterApplied(t *testing.T) {
	store := &fakeStore{
		workspaces: []WorkspaceHint{
			{Slug: "alpha"}, {Slug: "beta"}, {Slug: "gamma"},
		},
	}
	lister := &oauthWorkspaceLister{store: store}

	ctx := server.WithCurrentUser(context.Background(), fakeUser("user-1"))
	ctx = server.WithTokenAllowedWorkspaces(ctx, []string{"*"})

	res := classifyHTTPStatus(ctx, "item show", 404,
		[]byte(`workspace 'unknown' not visible`), lister)
	env := res.StructuredContent.(ErrorEnvelope)

	got := slugSet(env.Error.AvailableWorkspaces)
	want := map[string]bool{"alpha": true, "beta": true, "gamma": true}
	if len(got) != 3 {
		t.Fatalf("wildcard allow-list: got %d entries, want 3 (all user workspaces); entries=%v",
			len(got), env.Error.AvailableWorkspaces)
	}
	for slug := range want {
		if !got[slug] {
			t.Errorf("missing %q under wildcard allow-list", slug)
		}
	}
}

// TestUnknownWorkspace_NoTokenAllowList_NoFilterApplied covers the
// PAT path (or pre-TASK-952 OAuth tokens): no allow-list set →
// fall back to all the user's workspaces. Behavioural parity with
// the local CLI's `pad workspace list` output.
func TestUnknownWorkspace_NoTokenAllowList_NoFilterApplied(t *testing.T) {
	store := &fakeStore{
		workspaces: []WorkspaceHint{{Slug: "alpha"}, {Slug: "beta"}},
	}
	lister := &oauthWorkspaceLister{store: store}

	// No WithTokenAllowedWorkspaces — simulating PAT auth.
	ctx := server.WithCurrentUser(context.Background(), fakeUser("user-1"))

	res := classifyHTTPStatus(ctx, "item show", 404,
		[]byte(`workspace 'unknown' not visible`), lister)
	env := res.StructuredContent.(ErrorEnvelope)

	if len(env.Error.AvailableWorkspaces) != 2 {
		t.Fatalf("PAT path: got %d entries, want 2 (no token-level filter)",
			len(env.Error.AvailableWorkspaces))
	}
}

// TestUnknownWorkspace_NoUser_EmptyHints covers anonymous probes:
// without a user on context the lister returns nil and the envelope
// ships with empty available_workspaces — the rest of the error
// taxonomy (code + message) is still useful.
func TestUnknownWorkspace_NoUser_EmptyHints(t *testing.T) {
	store := &fakeStore{
		workspaces: []WorkspaceHint{{Slug: "alpha"}},
	}
	lister := &oauthWorkspaceLister{store: store}

	res := classifyHTTPStatus(context.Background(), "item show", 404,
		[]byte(`workspace 'foo' not visible`), lister)
	env := res.StructuredContent.(ErrorEnvelope)

	if env.Error.Code != ErrUnknownWorkspace {
		t.Errorf("code should still classify; got %q", env.Error.Code)
	}
	if len(env.Error.AvailableWorkspaces) != 0 {
		t.Errorf("anonymous probe must have empty available_workspaces; got %v",
			env.Error.AvailableWorkspaces)
	}
}

// TestUnknownWorkspace_StoreError_EmptyHints covers the "best-effort"
// contract: if the store lookup fails, the envelope still ships with
// the right code + message, just with empty available_workspaces.
func TestUnknownWorkspace_StoreError_EmptyHints(t *testing.T) {
	lister := &oauthWorkspaceLister{store: &fakeStore{err: errors.New("db down")}}

	ctx := server.WithCurrentUser(context.Background(), fakeUser("user-1"))
	ctx = server.WithTokenAllowedWorkspaces(ctx, []string{"alpha"})

	res := classifyHTTPStatus(ctx, "item show", 404,
		[]byte(`workspace 'foo' not visible`), lister)
	env := res.StructuredContent.(ErrorEnvelope)

	if env.Error.Code != ErrUnknownWorkspace {
		t.Errorf("code should still classify even when store fails; got %q", env.Error.Code)
	}
	if len(env.Error.AvailableWorkspaces) != 0 {
		t.Errorf("store failure must yield empty available_workspaces; got %v",
			env.Error.AvailableWorkspaces)
	}
}

// ─────────────────────────────────────────────────────────────────────
// buildAllowSet unit tests — the shared helper between lister + gate.
// ─────────────────────────────────────────────────────────────────────

func TestBuildAllowSet_NilReturnsNilNoFilter(t *testing.T) {
	if got := buildAllowSet(nil); got != nil {
		t.Errorf("nil input should yield nil (no filter); got %v", got)
	}
}

func TestBuildAllowSet_WildcardReturnsNilNoFilter(t *testing.T) {
	if got := buildAllowSet([]string{"*"}); got != nil {
		t.Errorf("wildcard should yield nil (no filter); got %v", got)
	}
}

func TestBuildAllowSet_WildcardWinsOverSpecific(t *testing.T) {
	// Defense-in-depth: a tampered allow-list with both a slug AND
	// the wildcard collapses to "no filter" — the safer
	// interpretation, matching what RequireWorkspaceAccess does.
	if got := buildAllowSet([]string{"alpha", "*"}); got != nil {
		t.Errorf("wildcard + specific should yield nil (no filter); got %v", got)
	}
}

func TestBuildAllowSet_SpecificListBuildsSet(t *testing.T) {
	got := buildAllowSet([]string{"alpha", "beta"})
	if got == nil {
		t.Fatal("specific list should yield non-nil set")
	}
	if _, ok := got["alpha"]; !ok {
		t.Error("expected alpha in set")
	}
	if _, ok := got["beta"]; !ok {
		t.Error("expected beta in set")
	}
	if _, ok := got["gamma"]; ok {
		t.Error("gamma must not be in set")
	}
}

// ─────────────────────────────────────────────────────────────────────
// End-to-end through packageHTTPResponse (the dispatcher's actual
// call site). Validates that wiring d.Lister into packageHTTPResponse
// produces a privacy-filtered envelope, exercising the full code
// path the production /mcp transport runs.
// ─────────────────────────────────────────────────────────────────────

// TestExecuteRequest_UsesRequestContext_NotOuterContext pins Codex
// review #379 round 1. The dispatcher's `executeRequest` MUST pass
// `req.Context()` (not the outer `ctx`) into `packageHTTPResponse`
// so the lister sees everything `buildHTTPRequest` + `d.Apply`
// attached: WithCurrentUser, WithAPITokenAuth, and any
// TokenAllowedWorkspaces the Apply hook layered on.
//
// Strategy: drive executeRequest with context.Background() (no
// values on the outer ctx) + a UserResolver that returns a user
// + a Handler that 404s with a workspace body. The lister must
// still see the user via req.Context() and produce
// available_workspaces. If the buggy version (using outer ctx)
// runs, the lister sees no user and returns empty hints —
// asserted against here as the fail mode.
func TestExecuteRequest_UsesRequestContext_NotOuterContext(t *testing.T) {
	store := &fakeStore{
		workspaces: []WorkspaceHint{{Slug: "alpha"}, {Slug: "beta"}},
	}
	user := fakeUser("user-1")

	d := &HTTPHandlerDispatcher{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"workspace 'foo' not visible"}}`))
		}),
		// UserResolver runs from the dispatcher's outer context;
		// the user is set on req.Context() inside buildHTTPRequest.
		// The outer ctx (passed to executeRequest) has NO user.
		UserResolver: func(_ context.Context) *models.User { return user },
		Lister:       &oauthWorkspaceLister{store: store},
	}

	res, err := d.executeRequest(context.Background(), "item show", user, "GET", "/api/v1/workspaces/foo/items", nil)
	if err != nil {
		t.Fatalf("executeRequest: %v", err)
	}
	env, ok := res.StructuredContent.(ErrorEnvelope)
	if !ok {
		t.Fatalf("structured content: got %T, want ErrorEnvelope", res.StructuredContent)
	}
	if env.Error.Code != ErrUnknownWorkspace {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrUnknownWorkspace)
	}
	if len(env.Error.AvailableWorkspaces) != 2 {
		t.Errorf("expected 2 hints (user has alpha+beta, no token allow-list); got %d (entries=%v)",
			len(env.Error.AvailableWorkspaces), env.Error.AvailableWorkspaces)
	}
}

func TestPackageHTTPResponse_404Workspace_FiltersAvailableWorkspaces(t *testing.T) {
	store := &fakeStore{
		workspaces: []WorkspaceHint{
			{Slug: "alpha"}, {Slug: "beta"}, {Slug: "gamma"},
		},
	}
	lister := &oauthWorkspaceLister{store: store}

	ctx := server.WithCurrentUser(context.Background(), fakeUser("user-1"))
	ctx = server.WithTokenAllowedWorkspaces(ctx, []string{"alpha"})

	resp := &http.Response{
		StatusCode: 404,
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"workspace 'gamma' not visible"}}`)),
	}
	res, err := packageHTTPResponse(ctx, "item show", resp, lister)
	if err != nil {
		t.Fatalf("packageHTTPResponse: %v", err)
	}
	env := res.StructuredContent.(ErrorEnvelope)

	if env.Error.Code != ErrUnknownWorkspace {
		t.Fatalf("code: got %q, want %q", env.Error.Code, ErrUnknownWorkspace)
	}
	if len(env.Error.AvailableWorkspaces) != 1 || env.Error.AvailableWorkspaces[0].Slug != "alpha" {
		t.Errorf("end-to-end privacy filter: got %v, want exactly [{Slug:alpha}]",
			env.Error.AvailableWorkspaces)
	}
}

// =====================================================================
// Helpers
// =====================================================================

// fakeUser returns a minimal *models.User the lister's auth check is
// happy with — only ID is read by the lister itself, everything
// else stays zero-value.
func fakeUser(id string) *models.User {
	return &models.User{ID: id}
}

func slugSet(hints []WorkspaceHint) map[string]bool {
	out := map[string]bool{}
	for _, h := range hints {
		out[h.Slug] = true
	}
	return out
}
