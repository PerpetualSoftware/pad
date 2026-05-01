package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// --- Stars ---

func TestRouteTable_ItemStarPostsToStarEndpoint(t *testing.T) {
	m, p, body, err := routeTable["item star"](map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
	})
	if err != nil {
		t.Fatalf("routeTable[item star]: %v", err)
	}
	if m != http.MethodPost {
		t.Errorf("method = %q, want POST", m)
	}
	if p != "/api/v1/workspaces/docapp/items/TASK-5/star" {
		t.Errorf("path = %q", p)
	}
	if len(body) != 0 {
		t.Errorf("expected empty body for star POST; got %q", string(body))
	}
}

func TestRouteTable_ItemUnstarDeletesStarEndpoint(t *testing.T) {
	m, p, _, err := routeTable["item unstar"](map[string]any{
		"workspace": "docapp", "ref": "TASK-5",
	})
	if err != nil {
		t.Fatalf("routeTable[item unstar]: %v", err)
	}
	if m != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", m)
	}
	if p != "/api/v1/workspaces/docapp/items/TASK-5/star" {
		t.Errorf("path = %q", p)
	}
}

func TestMapItemStarred_DefaultsHidesTerminalItems(t *testing.T) {
	// Without --all the dispatcher omits include_terminal so the
	// handler defaults to hiding done/completed items. Mirrors
	// `pad item starred` (default) on the CLI side.
	m, p, _, err := mapItemStarred(map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("mapItemStarred: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/starred" {
		t.Errorf("path = %q (expected no include_terminal query)", p)
	}
}

func TestMapItemStarred_AllSetsIncludeTerminal(t *testing.T) {
	_, p, _, err := mapItemStarred(map[string]any{
		"workspace": "docapp",
		"all":       true,
	})
	if err != nil {
		t.Fatalf("mapItemStarred: %v", err)
	}
	if !strings.Contains(p, "include_terminal=true") {
		t.Errorf("path missing include_terminal=true: %q", p)
	}
}

func TestMapItemStarred_RequiresWorkspace(t *testing.T) {
	_, _, _, err := mapItemStarred(map[string]any{})
	if err == nil {
		t.Errorf("expected error when workspace missing")
	}
}

// --- Roles ---

func TestMapRoleCreate_BuildsCanonicalBody(t *testing.T) {
	m, p, body, err := mapRoleCreate(map[string]any{
		"workspace":   "docapp",
		"name":        "Designer",
		"description": "UX work",
		"icon":        "🎨",
		"tools":       "figma,sketch",
	})
	if err != nil {
		t.Fatalf("mapRoleCreate: %v", err)
	}
	if m != http.MethodPost {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/agent-roles" {
		t.Errorf("path = %q", p)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	want := map[string]string{
		"name":        "Designer",
		"description": "UX work",
		"icon":        "🎨",
		"tools":       "figma,sketch",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("body[%q] = %v, want %v", k, got[k], v)
		}
	}
}

func TestMapRoleCreate_OmitsEmptyOptionalFields(t *testing.T) {
	_, _, body, err := mapRoleCreate(map[string]any{
		"workspace": "docapp",
		"name":      "Reviewer",
	})
	if err != nil {
		t.Fatalf("mapRoleCreate: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["name"] != "Reviewer" {
		t.Errorf("name not preserved: %v", got)
	}
	for _, key := range []string{"description", "icon", "tools"} {
		if _, present := got[key]; present {
			t.Errorf("empty %q should be omitted from body; got %v", key, got)
		}
	}
}

func TestMapRoleCreate_RequiresName(t *testing.T) {
	_, _, _, err := mapRoleCreate(map[string]any{"workspace": "ws"})
	if err == nil {
		t.Errorf("expected error when name missing")
	}
}

func TestRouteTable_RoleDelete(t *testing.T) {
	m, p, _, err := routeTable["role delete"](map[string]any{
		"workspace": "docapp",
		"slug":      "designer",
	})
	if err != nil {
		t.Fatalf("routeTable[role delete]: %v", err)
	}
	if m != http.MethodDelete {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/agent-roles/designer" {
		t.Errorf("path = %q", p)
	}
}

// --- Webhooks ---

func TestRouteTable_WebhookListAndDelete(t *testing.T) {
	if m, p, _, err := routeTable["webhook list"](map[string]any{"workspace": "docapp"}); err != nil {
		t.Errorf("webhook list: %v", err)
	} else {
		if m != http.MethodGet || p != "/api/v1/workspaces/docapp/webhooks" {
			t.Errorf("webhook list mapping = %q %q", m, p)
		}
	}
	if m, p, _, err := routeTable["webhook delete"](map[string]any{
		"workspace": "docapp", "id": "abc-123",
	}); err != nil {
		t.Errorf("webhook delete: %v", err)
	} else {
		if m != http.MethodDelete || p != "/api/v1/workspaces/docapp/webhooks/abc-123" {
			t.Errorf("webhook delete mapping = %q %q", m, p)
		}
	}
	if m, p, _, err := routeTable["webhook test"](map[string]any{
		"workspace": "docapp", "id": "abc-123",
	}); err != nil {
		t.Errorf("webhook test: %v", err)
	} else {
		if m != http.MethodPost || p != "/api/v1/workspaces/docapp/webhooks/abc-123/test" {
			t.Errorf("webhook test mapping = %q %q", m, p)
		}
	}
}

func TestMapWebhookCreate_BuildsCanonicalBody(t *testing.T) {
	_, p, body, err := mapWebhookCreate(map[string]any{
		"workspace": "docapp",
		"url":       "https://example.com/hook",
		"events":    "item.created,item.updated",
		"secret":    "shhh",
	})
	if err != nil {
		t.Fatalf("mapWebhookCreate: %v", err)
	}
	if p != "/api/v1/workspaces/docapp/webhooks" {
		t.Errorf("path = %q", p)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["url"] != "https://example.com/hook" {
		t.Errorf("url not preserved: %v", got)
	}
	if got["events"] != "item.created,item.updated" {
		t.Errorf("events not preserved: %v", got)
	}
	if got["secret"] != "shhh" {
		t.Errorf("secret not preserved: %v", got)
	}
}

func TestMapWebhookCreate_RequiresURL(t *testing.T) {
	_, _, _, err := mapWebhookCreate(map[string]any{"workspace": "ws"})
	if err == nil {
		t.Errorf("expected error when url missing")
	}
}

// --- Auth + workspace surfaces ---

func TestRouteTable_AuthWhoamiNoWorkspaceRequired(t *testing.T) {
	// `auth whoami` is global — input map need not carry workspace,
	// and the URL must NOT include /workspaces/ prefix.
	m, p, _, err := routeTable["auth whoami"](map[string]any{})
	if err != nil {
		t.Fatalf("auth whoami: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/auth/me" {
		t.Errorf("path = %q (expected /api/v1/auth/me with no workspace scoping)", p)
	}
}

func TestRouteTable_WorkspaceListNoWorkspaceRequired(t *testing.T) {
	m, p, _, err := routeTable["workspace list"](map[string]any{})
	if err != nil {
		t.Fatalf("workspace list: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces" {
		t.Errorf("path = %q", p)
	}
}

func TestRouteTable_WorkspaceStorage(t *testing.T) {
	m, p, _, err := routeTable["workspace storage"](map[string]any{"workspace": "docapp"})
	if err != nil {
		t.Fatalf("workspace storage: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/workspaces/docapp/storage/usage" {
		t.Errorf("path = %q", p)
	}
}

func TestMapWorkspaceAuditLog_OmitsEmptyFilters(t *testing.T) {
	// No filters → bare /api/v1/audit-log path. Mirrors the CLI's
	// "only set ?<key>=<val> if the flag has a value" behaviour.
	m, p, _, err := mapWorkspaceAuditLog(map[string]any{})
	if err != nil {
		t.Fatalf("mapWorkspaceAuditLog: %v", err)
	}
	if m != http.MethodGet {
		t.Errorf("method = %q", m)
	}
	if p != "/api/v1/audit-log" {
		t.Errorf("path = %q (expected no query string)", p)
	}
}

func TestMapWorkspaceAuditLog_PassesFiltersThrough(t *testing.T) {
	_, p, _, err := mapWorkspaceAuditLog(map[string]any{
		"action":    "item.created",
		"actor":     "user-uuid-1",
		"workspace": "docapp",
		"days":      float64(7),
		"limit":     float64(25),
	})
	if err != nil {
		t.Fatalf("mapWorkspaceAuditLog: %v", err)
	}
	idx := strings.Index(p, "?")
	if idx < 0 {
		t.Fatalf("path missing query string: %q", p)
	}
	values, parseErr := url.ParseQuery(p[idx+1:])
	if parseErr != nil {
		t.Fatalf("parse query: %v", parseErr)
	}
	want := map[string]string{
		"action":    "item.created",
		"actor":     "user-uuid-1",
		"workspace": "docapp",
		"days":      "7",
		"limit":     "25",
	}
	for k, v := range want {
		if got := values.Get(k); got != v {
			t.Errorf("query[%q] = %q, want %q", k, got, v)
		}
	}
}

func TestMapWorkspaceInvite_BuildsCanonicalBody(t *testing.T) {
	_, p, body, err := mapWorkspaceInvite(map[string]any{
		"workspace": "docapp",
		"email":     "alice@example.com",
		"role":      "viewer",
	})
	if err != nil {
		t.Fatalf("mapWorkspaceInvite: %v", err)
	}
	if p != "/api/v1/workspaces/docapp/members/invite" {
		t.Errorf("path = %q", p)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got["email"] != "alice@example.com" {
		t.Errorf("email not preserved: %v", got)
	}
	if got["role"] != "viewer" {
		t.Errorf("role not preserved: %v", got)
	}
}

func TestMapWorkspaceInvite_RoleOmittedWhenMissing(t *testing.T) {
	// Handler defaults to "editor" when role is unset, so the
	// dispatcher should NOT forward an empty role string — that
	// would override the default with "" and surprise the agent.
	_, _, body, err := mapWorkspaceInvite(map[string]any{
		"workspace": "docapp", "email": "alice@example.com",
	})
	if err != nil {
		t.Fatalf("mapWorkspaceInvite: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if _, present := got["role"]; present {
		t.Errorf("role should be omitted when not set; got %v", got)
	}
}

func TestMapWorkspaceInvite_RequiresEmail(t *testing.T) {
	_, _, _, err := mapWorkspaceInvite(map[string]any{"workspace": "ws"})
	if err == nil {
		t.Errorf("expected error when email missing")
	}
}

// --- noRemoteEquivalent extension ---

func TestDispatch_GithubCommandsRejectedAsCLIOnly(t *testing.T) {
	// `github link/status/unlink` chain `git rev-parse` + `gh` CLI for
	// the local branch + PR data they write — the agent's local
	// checkout has no representation on pad-cloud, so these commands
	// can never have a remote equivalent. The dispatcher should give
	// the same stable "no remote equivalent" message it gives for
	// other CLI-only commands.
	d := &HTTPHandlerDispatcher{
		Handler:      errorHandler(t, "github commands must not reach handler"),
		UserResolver: fixedUserResolver(&models.User{ID: "u"}),
	}
	for _, cmd := range []string{"github link", "github status", "github unlink"} {
		t.Run(cmd, func(t *testing.T) {
			ctx := WithDispatchInput(context.Background(), map[string]any{
				"workspace": "docapp",
			})
			res, err := d.Dispatch(ctx, strings.Split(cmd, " "), nil)
			if err != nil {
				t.Fatalf("Dispatch err: %v", err)
			}
			if !res.IsError {
				t.Errorf("expected IsError; got %#v", res)
			}
			if !containsToolText(res, "no remote equivalent") {
				t.Errorf("error should call out CLI-only nature; got %#v", res)
			}
		})
	}
}

// --- Integration smoke ---

// TestHTTPHandlerDispatcher_Integration_StarsRolesWebhooksWhoami exercises
// the new route-table additions end-to-end against a real
// *server.Server. Catches regressions in handler shape, route
// registration, and authentication for this slice's commands.
func TestHTTPHandlerDispatcher_Integration_StarsRolesWebhooksWhoami(t *testing.T) {
	srv, st := newPadServer(t)

	// Bootstrap workspace + owner.
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
	if err := st.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("add owner: %v", err)
	}

	d := &HTTPHandlerDispatcher{Handler: srv, UserResolver: fixedUserResolver(owner)}

	// auth whoami — global, no workspace.
	whoamiRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"auth", "whoami"},
		nil,
	)
	if err != nil || whoamiRes.IsError {
		t.Fatalf("auth whoami: err=%v IsError=%v: %#v", err, whoamiRes != nil && whoamiRes.IsError, whoamiRes)
	}
	me, ok := whoamiRes.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("whoami result not structured: %#v", whoamiRes.StructuredContent)
	}
	if me["email"] != "dave@example.com" {
		t.Errorf("whoami did not return current user: %v", me)
	}

	// workspace list — should include the bootstrapped workspace.
	wsListRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{}),
		[]string{"workspace", "list"},
		nil,
	)
	if err != nil || wsListRes.IsError {
		t.Fatalf("workspace list: err=%v IsError=%v: %#v", err, wsListRes != nil && wsListRes.IsError, wsListRes)
	}
	wsList, ok := wsListRes.StructuredContent.([]any)
	if !ok || len(wsList) == 0 {
		t.Fatalf("workspace list result not array or empty: %#v", wsListRes.StructuredContent)
	}

	// Create an item, then star it.
	createCtx := WithDispatchInput(context.Background(), map[string]any{
		"workspace": ws.Slug, "collection": "tasks", "title": "To star",
	})
	createRes, err := d.Dispatch(createCtx, []string{"item", "create"}, nil)
	if err != nil || createRes.IsError {
		t.Fatalf("item create: err=%v IsError=%v: %#v", err, createRes != nil && createRes.IsError, createRes)
	}
	created := createRes.StructuredContent.(map[string]any)
	ref, _ := created["ref"].(string)
	if ref == "" {
		t.Fatalf("item create missing ref: %#v", created)
	}

	starRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "ref": ref,
		}),
		[]string{"item", "star"},
		nil,
	)
	if err != nil || starRes.IsError {
		t.Fatalf("item star: err=%v IsError=%v: %#v", err, starRes != nil && starRes.IsError, starRes)
	}

	starredRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
		[]string{"item", "starred"},
		nil,
	)
	if err != nil || starredRes.IsError {
		t.Fatalf("item starred: err=%v IsError=%v: %#v", err, starredRes != nil && starredRes.IsError, starredRes)
	}
	starred, ok := starredRes.StructuredContent.([]any)
	if !ok || len(starred) != 1 {
		t.Fatalf("expected 1 starred item; got %#v", starredRes.StructuredContent)
	}

	// role create (owner-only) → role delete.
	roleRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "name": "Designer", "icon": "🎨",
		}),
		[]string{"role", "create"},
		nil,
	)
	if err != nil || roleRes.IsError {
		t.Fatalf("role create: err=%v IsError=%v: %#v", err, roleRes != nil && roleRes.IsError, roleRes)
	}
	role, _ := roleRes.StructuredContent.(map[string]any)
	roleSlug, _ := role["slug"].(string)
	if roleSlug == "" {
		t.Fatalf("role create did not return slug: %#v", role)
	}

	delRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "slug": roleSlug,
		}),
		[]string{"role", "delete"},
		nil,
	)
	if err != nil || delRes.IsError {
		t.Fatalf("role delete: err=%v IsError=%v: %#v", err, delRes != nil && delRes.IsError, delRes)
	}

	// Webhook list (empty) → create → list (one) → delete → list (empty).
	listRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{"workspace": ws.Slug}),
		[]string{"webhook", "list"},
		nil,
	)
	if err != nil || listRes.IsError {
		t.Fatalf("webhook list (empty): err=%v IsError=%v: %#v", err, listRes != nil && listRes.IsError, listRes)
	}
	if hooks, _ := listRes.StructuredContent.([]any); len(hooks) != 0 {
		t.Errorf("expected no webhooks initially; got %v", hooks)
	}

	createHookRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug,
			"url":       "https://example.com/hook",
			"events":    "item.created",
		}),
		[]string{"webhook", "create"},
		nil,
	)
	if err != nil || createHookRes.IsError {
		t.Fatalf("webhook create: err=%v IsError=%v: %#v", err, createHookRes != nil && createHookRes.IsError, createHookRes)
	}
	hook, _ := createHookRes.StructuredContent.(map[string]any)
	hookID, _ := hook["id"].(string)
	if hookID == "" {
		t.Fatalf("webhook create did not return id: %#v", hook)
	}

	delHookRes, err := d.Dispatch(
		WithDispatchInput(context.Background(), map[string]any{
			"workspace": ws.Slug, "id": hookID,
		}),
		[]string{"webhook", "delete"},
		nil,
	)
	if err != nil || delHookRes.IsError {
		t.Fatalf("webhook delete: err=%v IsError=%v: %#v", err, delHookRes != nil && delHookRes.IsError, delHookRes)
	}
}
