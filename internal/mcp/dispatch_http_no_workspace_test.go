package mcp

// TASK-2314: every remote route that needs a workspace and lacks one
// answers the structured no_workspace envelope, with
// available_workspaces, which is what stdio has always given. Before it
// the remote door answered a bare validation_failed "workspace is
// required" on every route class, so instructions.md's promise of the
// list held on one transport only.

import (
	"context"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// twoWorkspaces is ambiguous on purpose: one would be auto-injected
// (TASK-1076) and the route would never see a missing workspace.
var twoWorkspaces = []WorkspaceHint{{Slug: "alpha", Name: "Alpha"}, {Slug: "beta", Name: "Beta"}}

func noWorkspaceDispatcher(t *testing.T, h http.Handler) *HTTPHandlerDispatcher {
	t.Helper()
	if h == nil {
		h = errorHandler(t, "a missing workspace must be refused before any backend call")
	}
	return &HTTPHandlerDispatcher{
		Handler:      h,
		UserResolver: fixedUserResolver(&models.User{ID: "u-1", Name: "Tester"}),
		Lister:       &fakeLister{workspaces: twoWorkspaces},
	}
}

func assertNoWorkspace(t *testing.T, d *HTTPHandlerDispatcher, cmd []string, input map[string]any) {
	t.Helper()
	res, err := d.Dispatch(WithDispatchInput(context.Background(), input), cmd, nil)
	if err != nil {
		t.Fatalf("Dispatch protocol error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result, got %+v", res)
	}
	env := unwrapErrorEnvelope(t, res)
	if env.Error.Code != ErrNoWorkspace {
		t.Fatalf("code: got %q, want %q (envelope %+v)", env.Error.Code, ErrNoWorkspace, env.Error)
	}
	if !reflect.DeepEqual(env.Error.AvailableWorkspaces, twoWorkspaces) {
		t.Errorf("available_workspaces: got %+v, want %+v", env.Error.AvailableWorkspaces, twoWorkspaces)
	}
	if env.Error.Hint == "" {
		t.Error("envelope missing Hint")
	}
}

// One case per route class. Each was a bare validation_failed on main.
func TestRemoteNoWorkspace_PerRouteClass(t *testing.T) {
	cases := []struct {
		class string
		cmd   []string
		input map[string]any
	}{
		// A pure mapper in routeTable.
		{"route-table mapper", []string{"item", "comment"}, map[string]any{"ref": "TASK-1", "message": "hi"}},
		// A dispatcher method in specialRoutes.
		{"special route", []string{"project", "ready"}, map[string]any{}},
		// An itemLinkSpecs create and delete.
		{"link create", []string{"item", "block"}, map[string]any{"source": "TASK-1", "target": "TASK-2"}},
		{"link delete", []string{"item", "unblock"}, map[string]any{"source": "TASK-1", "target": "TASK-2"}},
		// The --assign / --role preprocessors, which resolve against the
		// workspace before any route runs.
		{"assign resolver", []string{"item", "create"}, map[string]any{"collection": "tasks", "title": "x", "assign": "bob"}},
		{"role resolver", []string{"item", "create"}, map[string]any{"collection": "tasks", "title": "x", "role": "implementer"}},
	}
	for _, tc := range cases {
		t.Run(tc.class, func(t *testing.T) {
			assertNoWorkspace(t, noWorkspaceDispatcher(t, nil), tc.cmd, tc.input)
		})
	}
}

// The census: no remote route answers a missing workspace in the old
// prose. A route added later that refuses with a hand-written
// "workspace is required" fails here, whichever class it joins.
func TestRemoteNoWorkspace_NoRouteAnswersTheOldProse(t *testing.T) {
	// Routes that need no workspace reach the backend; answer them
	// with a 404 rather than failing the test.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"not_found","message":"nope"}}`, http.StatusNotFound)
	})
	d := noWorkspaceDispatcher(t, h)

	seen := map[string]bool{}
	for k := range routeTable {
		seen[k] = true
	}
	for k := range d.specialRoutes() {
		seen[k] = true
	}
	for k := range itemLinkSpecs {
		seen[k] = true
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var mu sync.Mutex
	noWorkspace := 0
	for _, k := range keys {
		res, err := d.Dispatch(WithDispatchInput(context.Background(), map[string]any{}), strings.Fields(k), nil)
		if err != nil || res == nil || !res.IsError {
			continue
		}
		env := unwrapErrorEnvelope(t, res)
		if strings.Contains(strings.ToLower(env.Error.Message), "workspace is required") {
			t.Errorf("%s: answers a missing workspace with %q %q, want %q", k, env.Error.Code, env.Error.Message, ErrNoWorkspace)
		}
		if env.Error.Code == ErrNoWorkspace {
			mu.Lock()
			noWorkspace++
			mu.Unlock()
		}
	}
	// Control: the census must actually reach the workspace check on a
	// substantial share of routes, or it proves nothing.
	if noWorkspace < 20 {
		t.Errorf("only %d of %d routes answered no_workspace on empty input; the census is not reaching the check", noWorkspace, len(keys))
	}
}

// The injection now runs before the resolvers, so a caller with ONE
// workspace who omits it still gets --assign resolved in that workspace
// instead of a refusal.
func TestRemoteNoWorkspace_SingleWorkspaceAssignIsInjectedFirst(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		http.Error(w, `{"error":{"code":"not_found","message":"nope"}}`, http.StatusNotFound)
	})
	d := &HTTPHandlerDispatcher{
		Handler:      h,
		UserResolver: fixedUserResolver(&models.User{ID: "u-1", Name: "Tester"}),
		Lister:       &fakeLister{workspaces: []WorkspaceHint{{Slug: "only"}}},
	}
	in := map[string]any{"collection": "tasks", "title": "x", "assign": "bob"}
	res, err := d.Dispatch(WithDispatchInput(context.Background(), in), []string{"item", "create"}, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res != nil && res.IsError {
		if env := unwrapErrorEnvelope(t, res); env.Error.Code == ErrNoWorkspace ||
			strings.Contains(env.Error.Message, "workspace is required") {
			t.Fatalf("single-workspace caller refused for a missing workspace: %+v", env.Error)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || !strings.Contains(paths[0], "/workspaces/only/") {
		t.Fatalf("--assign was not resolved in the injected workspace; backend saw %v", paths)
	}
}
