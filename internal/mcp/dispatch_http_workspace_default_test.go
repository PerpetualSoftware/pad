package mcp

// Tests for TASK-1076's workspace auto-default. Pins the four-case
// matrix the task spec called out:
//
//   1. Single resolved workspace, no caller param → inject
//   2. Single resolved workspace, caller passed workspace=other →
//      caller wins (no silent override)
//   3. Multiple resolved workspaces, no caller param → leave alone
//   4. Zero resolved workspaces, no caller param → leave alone
//
// Plus two operational cases that aren't in the formal matrix but
// would silently break behavior if regressed:
//
//   - No Lister wired → leave alone (tests / non-OAuth paths)
//   - Lister returns error → leave alone (don't poison input on
//     transient store hiccup)

import (
	"context"
	"errors"
	"testing"
)

// fakeLister satisfies WorkspaceLister with a fixed return value.
// Used to exercise each matrix case without spinning up a real store.
type fakeLister struct {
	workspaces []WorkspaceHint
	err        error
}

func (f *fakeLister) ListWorkspaces(ctx context.Context) ([]WorkspaceHint, error) {
	return f.workspaces, f.err
}

func TestMaybeInjectWorkspace_SingleWorkspace_Injects(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Lister: &fakeLister{workspaces: []WorkspaceHint{{Slug: "only", Name: "Only Workspace"}}},
	}
	got := d.maybeInjectWorkspace(context.Background(), map[string]any{})
	if got["workspace"] != "only" {
		t.Errorf("expected workspace='only' injected, got %v", got["workspace"])
	}
}

func TestMaybeInjectWorkspace_CallerWins_OverSingleWorkspace(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Lister: &fakeLister{workspaces: []WorkspaceHint{{Slug: "default-one"}}},
	}
	got := d.maybeInjectWorkspace(context.Background(), map[string]any{"workspace": "explicit"})
	if got["workspace"] != "explicit" {
		t.Errorf("explicit workspace= must win over default; got %v", got["workspace"])
	}
}

func TestMaybeInjectWorkspace_MultipleWorkspaces_NoInject(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Lister: &fakeLister{workspaces: []WorkspaceHint{{Slug: "a"}, {Slug: "b"}}},
	}
	got := d.maybeInjectWorkspace(context.Background(), map[string]any{})
	if _, present := got["workspace"]; present {
		t.Errorf("multiple workspaces must NOT auto-inject (ambiguous); got %v", got["workspace"])
	}
}

func TestMaybeInjectWorkspace_ZeroWorkspaces_NoInject(t *testing.T) {
	d := &HTTPHandlerDispatcher{
		Lister: &fakeLister{workspaces: nil},
	}
	got := d.maybeInjectWorkspace(context.Background(), map[string]any{})
	if _, present := got["workspace"]; present {
		t.Errorf("zero workspaces must NOT inject; got %v", got["workspace"])
	}
}

func TestMaybeInjectWorkspace_NilLister_NoInject(t *testing.T) {
	// Tests + non-OAuth transports leave Lister nil. The injection
	// must be a no-op so existing behavior is preserved.
	d := &HTTPHandlerDispatcher{Lister: nil}
	in := map[string]any{"foo": "bar"}
	got := d.maybeInjectWorkspace(context.Background(), in)
	if _, present := got["workspace"]; present {
		t.Errorf("nil Lister must NOT inject; got %v", got["workspace"])
	}
	if got["foo"] != "bar" {
		t.Errorf("nil Lister must preserve other input keys; got %v", got)
	}
}

func TestMaybeInjectWorkspace_ListerError_NoInject(t *testing.T) {
	// Transient store error from the lister must fall through to
	// "no inject" — don't fail the whole tool call on a hiccup, let
	// the route mapper's eventual error speak for itself if the
	// route needs workspace.
	d := &HTTPHandlerDispatcher{
		Lister: &fakeLister{err: errors.New("simulated store failure")},
	}
	got := d.maybeInjectWorkspace(context.Background(), map[string]any{})
	if _, present := got["workspace"]; present {
		t.Errorf("lister error must NOT inject; got %v", got["workspace"])
	}
}

func TestMaybeInjectWorkspace_CopyOnWrite(t *testing.T) {
	// The helper must NOT mutate the caller's input map in place —
	// callers (the dispatch flow) hold the original reference and
	// surface it in error logs / tool results.
	d := &HTTPHandlerDispatcher{
		Lister: &fakeLister{workspaces: []WorkspaceHint{{Slug: "only"}}},
	}
	in := map[string]any{}
	out := d.maybeInjectWorkspace(context.Background(), in)
	if _, present := in["workspace"]; present {
		t.Error("input map must not be mutated in place")
	}
	if out["workspace"] != "only" {
		t.Errorf("returned map must carry the injected value; got %v", out["workspace"])
	}
}
