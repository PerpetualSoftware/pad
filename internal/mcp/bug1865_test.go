package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// BUG-1865: the cloud /mcp transport used to resolve the target
// workspace from a SINGLE process-global *mcp.WorkspaceState shared
// across every OAuth user and every MCP session (constructed once at
// cmd/pad/main.go). pad_set_workspace mutated that shared object, and on
// any later tool call that omitted an explicit `workspace`, env.Dispatch
// injected the shared value:
//
//	mergeDispatchInput(input, env.Workspace.ResolveDefault(), ...)   // catalog.go:169
//	    -> read back at dispatch_http.go:280
//
// so one session's selection bled into a DIFFERENT session's call.
//
// IMPACT (verified — not overstated): it was a workspace-RESOLUTION
// leak, not a write into a stranger's tenant. Per BUG-1616
// (middleware_auth.go:571-594) an admin/PAT/OAuth *bearer* caller — the
// shape MCP uses — who is NOT a member of the leaked workspace is
// rejected 403, so the misrouted write is blocked there. The real harm
// was (a) leaking another user's workspace slug into your routing/errors
// and (b) confusing "you are not a member of <someone else's ws>" errors
// for a workspace you never named, plus wrong-destination reads/writes
// among workspaces you DO have access to (your own workspaces across
// concurrent sessions — the same-user case below needs no second user
// and no admin).
//
// FIX: the cloud mount uses NewSharedWorkspaceState(), whose
// ResolveDefault() returns "" — so the shared value is never injected as
// a per-call default. Resolution falls back to the explicit `workspace`
// arg (or the per-user maybeInjectWorkspace default), never to cross-user
// shared memory. These tests are regression GUARDS: they assert the leak
// no longer happens on a shared state, and that a single-user (local
// stdio) state still injects its session workspace.

// newSharedLeakEnv models the multi-user cloud transport: one SHARED
// WorkspaceState behind a real fan-out dispatch env, with a recording
// handler capturing the routed workspace URL + user. Lister is nil so the
// only possible source of a workspace on a no-explicit call is the shared
// session state — the vector under test, isolated from the per-user
// single-workspace auto-default.
func newSharedLeakEnv(t *testing.T) (ActionEnv, *WorkspaceState, *recordingHandler) {
	t.Helper()
	shared := NewSharedWorkspaceState() // mirrors cmd/pad/main.go cloud mount
	rec := &recordingHandler{t: t}
	dispatcher := &HTTPHandlerDispatcher{
		Handler: rec,
		UserResolver: func(ctx context.Context) *models.User {
			u, _ := server.CurrentUserFromContext(ctx)
			return u
		},
	}
	return ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: shared, Dispatcher: dispatcher}, shared, rec
}

// TestBUG1865_SharedStateDoesNotLeakAcrossUsers guards the cross-user
// case: Alice's pad_set_workspace must NOT influence Bob's later call.
func TestBUG1865_SharedStateDoesNotLeakAcrossUsers(t *testing.T) {
	env, shared, rec := newSharedLeakEnv(t)

	// Alice's session "sets" her workspace. On a shared state this does
	// not persist (status=not_persisted) — the whole point.
	_, setWS := SetWorkspaceTool(shared, nil)
	alice := &models.User{ID: "alice", Name: "Alice"}
	aliceCtx := server.WithCurrentUser(context.Background(), alice)
	if res, err := setWS(aliceCtx, callToolRequest(map[string]any{"workspace": "alice-ws"})); err != nil {
		t.Fatalf("pad_set_workspace(alice-ws): %v", err)
	} else if res.IsError {
		t.Fatalf("pad_set_workspace(alice-ws) IsError: %s", textOf(res))
	}

	// Bob creates an item with NO explicit workspace.
	bob := &models.User{ID: "bob", Name: "Bob"}
	bobCtx := server.WithCurrentUser(context.Background(), bob)
	itemHandler := makeFanOutHandler(padItemTool, env)
	res, err := itemHandler(bobCtx, callToolRequest(map[string]any{
		"action":     "create",
		"collection": "tasks",
		"title":      "Bob's task",
	}))
	if err != nil {
		t.Fatalf("Bob's create errored at transport: %v", err)
	}

	// The request must never have reached Alice's workspace...
	if strings.Contains(rec.gotPath, "alice-ws") {
		t.Fatalf("LEAK: Bob's request routed to %q (Alice's workspace)", rec.gotPath)
	}
	// ...and with no workspace resolvable, the call must fail cleanly
	// (workspace required) rather than silently routing somewhere.
	if !res.IsError {
		t.Fatalf("expected a workspace-required error for Bob's no-workspace call; got success path=%q", rec.gotPath)
	}
	if rec.requestCount != 0 {
		t.Fatalf("no HTTP request should have been synthesized; got %d (path=%q)", rec.requestCount, rec.gotPath)
	}
}

// TestBUG1865_SharedSetWorkspaceDoesNotPersist locks in the honest
// shared-state contract: pad_set_workspace on a shared state must NOT
// persist the value (ResolveDefault stays "") and must report
// not_persisted so the agent passes workspace explicitly.
func TestBUG1865_SharedSetWorkspaceDoesNotPersist(t *testing.T) {
	shared := NewSharedWorkspaceState()
	_, setWS := SetWorkspaceTool(shared, nil)
	res, err := setWS(context.Background(), callToolRequest(map[string]any{"workspace": "some-ws"}))
	if err != nil {
		t.Fatalf("set-workspace: %v", err)
	}
	if res.IsError {
		t.Fatalf("set-workspace on shared state should not be an error: %s", textOf(res))
	}
	if got := shared.ResolveDefault(); got != "" {
		t.Fatalf("shared ResolveDefault() = %q, want \"\" (must not persist)", got)
	}
	body := textOf(res)
	if !strings.Contains(body, "not_persisted") {
		t.Fatalf("shared set-workspace response should report not_persisted; got %q", body)
	}
}

// TestBUG1865_SharedSetWorkspaceDescriptionWarns guards that the
// pad_set_workspace tool description is honest per-deployment: a shared
// (cloud) state must warn that the default is not persisted, while a
// single-user state keeps the "sets the default" wording.
func TestBUG1865_SharedSetWorkspaceDescriptionWarns(t *testing.T) {
	sharedTool, _ := SetWorkspaceTool(NewSharedWorkspaceState(), nil)
	if !strings.Contains(sharedTool.Description, "NOT persist") {
		t.Errorf("shared pad_set_workspace description should warn it does NOT persist; got %q", sharedTool.Description)
	}
	localTool, _ := SetWorkspaceTool(NewWorkspaceState(""), nil)
	if !strings.Contains(localTool.Description, "subsequent tool calls in this MCP session") {
		t.Errorf("local pad_set_workspace description should describe session defaulting; got %q", localTool.Description)
	}
}

// TestBUG1865_SharedStateNoSameUserSessionBleed guards the same-user
// case: two concurrent sessions of one user must not clobber each other's
// workspace. Before the fix, session B's set-workspace bled into session
// A's next call.
func TestBUG1865_SharedStateNoSameUserSessionBleed(t *testing.T) {
	env, shared, rec := newSharedLeakEnv(t)
	_, setWS := SetWorkspaceTool(shared, nil)

	dave := &models.User{ID: "dave", Name: "Dave"}
	daveCtx := server.WithCurrentUser(context.Background(), dave)

	if _, err := setWS(daveCtx, callToolRequest(map[string]any{"workspace": "project-a"})); err != nil {
		t.Fatalf("session A set-workspace: %v", err)
	}
	if _, err := setWS(daveCtx, callToolRequest(map[string]any{"workspace": "project-b"})); err != nil {
		t.Fatalf("session B set-workspace: %v", err)
	}

	itemHandler := makeFanOutHandler(padItemTool, env)
	res, err := itemHandler(daveCtx, callToolRequest(map[string]any{
		"action":     "create",
		"collection": "tasks",
		"title":      "meant for project-a",
	}))
	if err != nil {
		t.Fatalf("create errored at transport: %v", err)
	}
	if strings.Contains(rec.gotPath, "project-b") || strings.Contains(rec.gotPath, "project-a") {
		t.Fatalf("LEAK: session A's write routed to %q via shared state", rec.gotPath)
	}
	if !res.IsError {
		t.Fatalf("expected workspace-required error; got success path=%q", rec.gotPath)
	}
}

// TestBUG1865_ExplicitWorkspaceStillWins confirms an explicit workspace
// on the call is honored on the shared transport (the supported way to
// target a workspace on cloud).
func TestBUG1865_ExplicitWorkspaceStillWins(t *testing.T) {
	env, shared, rec := newSharedLeakEnv(t)
	_, setWS := SetWorkspaceTool(shared, nil)
	if _, err := setWS(context.Background(), callToolRequest(map[string]any{"workspace": "alice-ws"})); err != nil {
		t.Fatalf("set-workspace: %v", err)
	}

	bob := &models.User{ID: "bob", Name: "Bob"}
	bobCtx := server.WithCurrentUser(context.Background(), bob)
	itemHandler := makeFanOutHandler(padItemTool, env)
	res, err := itemHandler(bobCtx, callToolRequest(map[string]any{
		"action":     "create",
		"collection": "tasks",
		"title":      "Bob's task",
		"workspace":  "bob-ws",
	}))
	if err != nil || res.IsError {
		t.Fatalf("Bob's explicit create failed: err=%v res=%s", err, textOf(res))
	}
	if !strings.Contains(rec.gotPath, "/workspaces/bob-ws/") {
		t.Fatalf("explicit workspace not honored: path = %q, want bob-ws", rec.gotPath)
	}
}

// TestBUG1865_LocalStdioSessionStillInjected locks in that the fix does
// NOT regress the single-user local stdio transport, where a
// (non-shared) session workspace is legitimately injected as the default.
func TestBUG1865_LocalStdioSessionStillInjected(t *testing.T) {
	// Non-shared state == local `pad mcp serve` (single user per process).
	local := NewWorkspaceState("my-local-ws")
	rec := &recordingHandler{t: t}
	dispatcher := &HTTPHandlerDispatcher{
		Handler:      rec,
		UserResolver: func(ctx context.Context) *models.User { u, _ := server.CurrentUserFromContext(ctx); return u },
	}
	env := ActionEnv{Doc: liveCmdhelpDoc(t), Workspace: local, Dispatcher: dispatcher}

	dave := &models.User{ID: "dave", Name: "Dave"}
	ctx := server.WithCurrentUser(context.Background(), dave)
	itemHandler := makeFanOutHandler(padItemTool, env)
	res, err := itemHandler(ctx, callToolRequest(map[string]any{
		"action":     "create",
		"collection": "tasks",
		"title":      "Dave's task",
	}))
	if err != nil || res.IsError {
		t.Fatalf("local create failed: err=%v res=%s", err, textOf(res))
	}
	if !strings.Contains(rec.gotPath, "/workspaces/my-local-ws/") {
		t.Fatalf("local session workspace not injected: path = %q, want my-local-ws", rec.gotPath)
	}
}
