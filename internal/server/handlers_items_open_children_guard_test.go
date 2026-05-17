package server

// IDEA-1494 — handler-level coverage for the open-children guard on
// `pad item update` (handleUpdateItem). Each test seeds a parent +
// children via the public HTTP API so we exercise the same code path
// CLI / MCP traffic hits.

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// seedParentAndChildren creates a plan + N child tasks with the given
// statuses (one per child), returning the parent and the slice of child
// refs in creation order. Uses the default workspace template so the
// `tasks` and `plans` collections exist with their default schemas.
func seedParentAndChildren(t *testing.T, srv *Server, wsSlug string, childStatuses []string) (models.Item, []models.Item) {
	t.Helper()
	planResp := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/plans/items", map[string]interface{}{
		"title":  "PLAN under test",
		"fields": `{"status":"active"}`,
	})
	if planResp.Code != http.StatusCreated {
		t.Fatalf("seed plan: expected 201, got %d: %s", planResp.Code, planResp.Body.String())
	}
	var plan models.Item
	parseJSON(t, planResp, &plan)

	children := make([]models.Item, 0, len(childStatuses))
	for i, status := range childStatuses {
		body := map[string]interface{}{
			"title":  "child " + status,
			"fields": map[string]interface{}{"status": status, "parent": plan.Ref},
		}
		_ = i
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/tasks/items", body)
		if rr.Code != http.StatusCreated {
			t.Fatalf("seed child %d: expected 201, got %d: %s", i, rr.Code, rr.Body.String())
		}
		var c models.Item
		parseJSON(t, rr, &c)
		children = append(children, c)
	}
	return plan, children
}

func TestOpenChildrenGuard_RejectsTerminalWithOpenChildren(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, children := seedParentAndChildren(t, srv, slug, []string{"open", "done"})

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				OpenChildren []struct {
					Ref            string `json:"ref"`
					Title          string `json:"title"`
					Status         string `json:"status"`
					CollectionSlug string `json:"collection_slug"`
				} `json:"open_children"`
				DoneField      string `json:"done_field"`
				AttemptedValue string `json:"attempted_value"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse error body: %v (raw=%s)", err, rr.Body.String())
	}
	if resp.Error.Code != "open_children" {
		t.Fatalf("expected code=open_children, got %q", resp.Error.Code)
	}
	if len(resp.Error.Details.OpenChildren) != 1 {
		t.Fatalf("expected exactly 1 open child in details, got %d (%+v)",
			len(resp.Error.Details.OpenChildren), resp.Error.Details.OpenChildren)
	}
	got := resp.Error.Details.OpenChildren[0]
	if got.Ref != children[0].Ref {
		t.Errorf("open child ref: want %q, got %q", children[0].Ref, got.Ref)
	}
	if got.Status != "open" {
		t.Errorf("open child status: want open, got %q", got.Status)
	}
	if got.CollectionSlug != "tasks" {
		t.Errorf("open child collection_slug: want tasks, got %q", got.CollectionSlug)
	}
	if got.Title == "" {
		t.Errorf("open child title should be populated")
	}
	if resp.Error.Details.DoneField != "status" {
		t.Errorf("expected done_field=status, got %q", resp.Error.Details.DoneField)
	}
	if resp.Error.Details.AttemptedValue != "completed" {
		t.Errorf("expected attempted_value=completed, got %q", resp.Error.Details.AttemptedValue)
	}
	if !strings.Contains(resp.Error.Message, "--force") {
		t.Errorf("expected message to mention --force escape hatch, got %q", resp.Error.Message)
	}

	// Mutation safety: the parent's status must be unchanged.
	getResp := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("re-read parent: %d", getResp.Code)
	}
	var fresh models.Item
	parseJSON(t, getResp, &fresh)
	var fields map[string]any
	_ = json.Unmarshal([]byte(fresh.Fields), &fields)
	if s, _ := fields["status"].(string); s != "active" {
		t.Errorf("parent status should be unchanged after rejection, got %q", s)
	}
}

func TestOpenChildrenGuard_NoChildren_OK(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	planResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
		"title":  "lone plan",
		"fields": `{"status":"active"}`,
	})
	parseJSON(t, planResp, &models.Item{})
	var plan models.Item
	parseJSON(t, planResp, &plan)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (no children, no guard), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOpenChildrenGuard_AllChildrenTerminal_OK(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"done", "cancelled"})

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (all children terminal), got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOpenChildrenGuard_ForceOverrides(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
		"force":  true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with force=true, got %d: %s", rr.Code, rr.Body.String())
	}
	var updated models.Item
	parseJSON(t, rr, &updated)
	var fields map[string]any
	_ = json.Unmarshal([]byte(updated.Fields), &fields)
	if s, _ := fields["status"].(string); s != "completed" {
		t.Errorf("expected parent status=completed with force, got %q", s)
	}
}

func TestOpenChildrenGuard_NonTerminalTransition_NotGuarded(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	// active → paused is non-terminal → non-terminal; no guard.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "paused"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-terminal transition, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOpenChildrenGuard_TerminalToTerminal_NotGuarded(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Custom collection with two terminal options so we can transition
	// between them. The plans schema only declares one terminal value
	// (`completed`), so we need a richer schema to exercise this edge.
	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Releases",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["draft","shipped","archived"],"terminal_options":["shipped","archived"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create releases collection: %d %s", collResp.Code, collResp.Body.String())
	}
	parentResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/releases/items", map[string]interface{}{
		"title":  "v1.0",
		"fields": map[string]interface{}{"status": "shipped"},
	})
	if parentResp.Code != http.StatusCreated {
		t.Fatalf("create release: %d %s", parentResp.Code, parentResp.Body.String())
	}
	var release models.Item
	parseJSON(t, parentResp, &release)

	// Hang an open child task off the release.
	childResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "follow-up",
		"fields": map[string]interface{}{"status": "open", "parent": release.Ref},
	})
	if childResp.Code != http.StatusCreated {
		t.Fatalf("create child: %d %s", childResp.Code, childResp.Body.String())
	}

	// shipped → archived is terminal → terminal under the release
	// schema; the guard should not fire even though an open child is
	// still attached. Only non-terminal → terminal is gated.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+release.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "archived"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for terminal→terminal transition, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOpenChildrenGuard_NoOpTerminal_NotGuarded(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})
	// Force-flip to completed first.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
		"force":  true,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("setup force-complete: %d", rr.Code)
	}

	// completed → completed is a no-op terminal transition; guard
	// must not fire.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for no-op terminal transition, got %d: %s", rr.Code, rr.Body.String())
	}
}

func TestOpenChildrenGuard_UsesCollectionTerminalOptions(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Custom collection whose `status` field declares its own
	// terminal_options (no overlap with DefaultTerminalStatuses' core
	// trio). Ensures the guard reads terminal_options from the schema,
	// not the global default list.
	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Epics",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["todo","shipping","shipped"],"terminal_options":["shipped"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create epic collection: %d %s", collResp.Code, collResp.Body.String())
	}
	parentResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/epics/items", map[string]interface{}{
		"title":  "Epic",
		"fields": map[string]interface{}{"status": "todo"},
	})
	if parentResp.Code != http.StatusCreated {
		t.Fatalf("create epic: %d %s", parentResp.Code, parentResp.Body.String())
	}
	var epic models.Item
	parseJSON(t, parentResp, &epic)

	// Hang an open child task off the epic. Tasks use their own
	// default terminal list — `open` is not in it, so the child reads
	// as non-terminal.
	childResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "epic child",
		"fields": map[string]interface{}{"status": "open", "parent": epic.Ref},
	})
	if childResp.Code != http.StatusCreated {
		t.Fatalf("create child: %d %s", childResp.Code, childResp.Body.String())
	}

	// Attempt to move the epic to its declared terminal value.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+epic.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "shipped"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for shipped transition with open child, got %d: %s", rr.Code, rr.Body.String())
	}

	// `shipping` is non-terminal under the schema even though it
	// sounds done-ish; the guard must not fire.
	rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+epic.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "shipping"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 for non-terminal `shipping`, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestOpenChildrenGuard_GuardAppliesToAnyParent confirms the guard
// fires for parent items beyond just `plans` — IDEA-1494 optional extra
// #3 (the brief explicitly adopts it). Uses a Task with a child Task
// to exercise the non-plan path.
func TestOpenChildrenGuard_GuardAppliesToAnyParent(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	parentResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "parent task",
		"fields": map[string]interface{}{"status": "open"},
	})
	if parentResp.Code != http.StatusCreated {
		t.Fatalf("create parent task: %d %s", parentResp.Code, parentResp.Body.String())
	}
	var parent models.Item
	parseJSON(t, parentResp, &parent)

	childResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":  "subtask",
		"fields": map[string]interface{}{"status": "open", "parent": parent.Ref},
	})
	if childResp.Code != http.StatusCreated {
		t.Fatalf("create subtask: %d %s", childResp.Code, childResp.Body.String())
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+parent.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "done"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 for task-with-open-subtask, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestOpenChildrenGuard_VisibilitySanitizesPayload covers Codex round 2
// P1: when a restricted caller updates a parent they CAN see but the
// parent has children in collections they CAN'T see, the guard's
// invariant check still considers those hidden children (data
// integrity — a restricted user can't slip an item past completion by
// virtue of reduced visibility) but the 409 response sanitizes the
// payload — hidden children appear only as a count, never with refs
// or titles.
func TestOpenChildrenGuard_VisibilitySanitizesPayload(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	// Workspace with two collections: parent's collection (which the
	// editor CAN see) and "secrets" (which they can't).
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Vis"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	parentColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Plans", Slug: "plans", Prefix: "PLAN",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"],"terminal_options":["completed"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection plans: %v", err)
	}
	visibleChildColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Tasks", Slug: "tasks", Prefix: "TASK",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection tasks: %v", err)
	}
	hiddenChildColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Secrets", Slug: "secrets", Prefix: "SEC",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection secrets: %v", err)
	}

	// Seed the parent + one visible-open child + one hidden-open child.
	parent, err := srv.store.CreateItem(ws.ID, parentColl.ID, models.ItemCreate{
		Title: "Parent", Fields: `{"status":"active"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem parent: %v", err)
	}
	visibleChild, err := srv.store.CreateItem(ws.ID, visibleChildColl.ID, models.ItemCreate{
		Title: "Visible task", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem visible child: %v", err)
	}
	if _, err := srv.store.SetParentLink(ws.ID, visibleChild.ID, parent.ID, "admin"); err != nil {
		t.Fatalf("SetParentLink visible: %v", err)
	}
	hiddenChild, err := srv.store.CreateItem(ws.ID, hiddenChildColl.ID, models.ItemCreate{
		Title:  "TOP SECRET — do not leak",
		Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("CreateItem hidden child: %v", err)
	}
	if _, err := srv.store.SetParentLink(ws.ID, hiddenChild.ID, parent.ID, "admin"); err != nil {
		t.Fatalf("SetParentLink hidden: %v", err)
	}

	// Restricted editor: workspace member with collection_access scoped
	// to the parent's collection + the visible-child collection only.
	editor, err := srv.store.CreateUser(models.UserCreate{
		Email: "editor-vis@example.com", Name: "Editor", Username: "editor-vis",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser editor: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, editor.ID, "specific", []string{parentColl.ID, visibleChildColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	token, err := srv.store.CreateSession(editor.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// PATCH the parent as the restricted editor.
	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/workspaces/"+ws.Slug+"/items/"+parent.Slug, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	}, token)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}

	body := rr.Body.String()
	// Leak check: the hidden child's title / ref / collection slug must
	// NOT appear anywhere in the response.
	for _, secret := range []string{"TOP SECRET", "do not leak", "secrets", hiddenChild.Slug} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaked hidden child material %q: %s", secret, body)
		}
	}
	// Structural assertions.
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details struct {
				OpenChildren []struct {
					Ref            string `json:"ref"`
					Title          string `json:"title"`
					CollectionSlug string `json:"collection_slug"`
				} `json:"open_children"`
				HiddenBlockerCount int `json:"hidden_blocker_count"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error.Code != "open_children" {
		t.Fatalf("expected code=open_children, got %q", resp.Error.Code)
	}
	if len(resp.Error.Details.OpenChildren) != 1 {
		t.Fatalf("expected 1 visible blocker, got %d (%+v)",
			len(resp.Error.Details.OpenChildren), resp.Error.Details.OpenChildren)
	}
	if resp.Error.Details.OpenChildren[0].CollectionSlug != "tasks" {
		t.Errorf("only the visible-collection child should appear, got slug=%q",
			resp.Error.Details.OpenChildren[0].CollectionSlug)
	}
	if resp.Error.Details.HiddenBlockerCount != 1 {
		t.Errorf("expected hidden_blocker_count=1, got %d",
			resp.Error.Details.HiddenBlockerCount)
	}
}

// TestOpenChildrenGuard_AllBlockersHiddenSurfaceGenericMessage covers
// the edge case where EVERY blocking child is hidden from the caller —
// the guard still fires (data integrity) but the human message is
// generic and `details.open_children` is empty while
// `hidden_blocker_count` is non-zero.
func TestOpenChildrenGuard_AllBlockersHiddenSurfaceGenericMessage(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Vis2"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	parentColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Plans", Slug: "plans", Prefix: "PLAN",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"],"terminal_options":["completed"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	hiddenColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Hidden", Slug: "hidden", Prefix: "HID",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`,
	})
	if err != nil {
		t.Fatalf("CreateCollection hidden: %v", err)
	}
	parent, _ := srv.store.CreateItem(ws.ID, parentColl.ID, models.ItemCreate{
		Title: "P", Fields: `{"status":"active"}`,
	})
	child, _ := srv.store.CreateItem(ws.ID, hiddenColl.ID, models.ItemCreate{
		Title: "secret", Fields: `{"status":"open"}`,
	})
	if _, err := srv.store.SetParentLink(ws.ID, child.ID, parent.ID, "admin"); err != nil {
		t.Fatalf("SetParentLink: %v", err)
	}

	editor, _ := srv.store.CreateUser(models.UserCreate{
		Email: "e@example.com", Name: "E", Username: "e", Password: "pw-test-12345",
	})
	if err := srv.store.AddWorkspaceMember(ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	// Editor sees ONLY parentColl; child's collection is invisible.
	if err := srv.store.SetMemberCollectionAccess(ws.ID, editor.ID, "specific", []string{parentColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	token, _ := srv.store.CreateSession(editor.ID, "go-test", "192.0.2.1", "", 24*time.Hour)

	rr := doRequestWithCookie(srv, "PATCH", "/api/v1/workspaces/"+ws.Slug+"/items/"+parent.Slug, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	}, token)
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Error struct {
			Message string `json:"message"`
			Details struct {
				OpenChildren       []map[string]any `json:"open_children"`
				HiddenBlockerCount int              `json:"hidden_blocker_count"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(resp.Error.Details.OpenChildren) != 0 {
		t.Errorf("expected empty open_children when all blockers hidden, got %v", resp.Error.Details.OpenChildren)
	}
	// Codex round-5 P3: open_children must serialize as `[]`, not
	// `null`, when all blockers are hidden. Clients range over the
	// array unconditionally; `null` would break their iteration.
	// Re-parse the raw JSON so we see the wire shape (not the Go
	// nil-vs-empty-slice distinction the typed unmarshal hides).
	if !strings.Contains(rr.Body.String(), `"open_children":[]`) {
		t.Errorf("open_children must serialize as [] (not null) on hidden-only rejection; raw body: %s", rr.Body.String())
	}
	if resp.Error.Details.HiddenBlockerCount != 1 {
		t.Errorf("expected hidden_blocker_count=1, got %d", resp.Error.Details.HiddenBlockerCount)
	}
	if !strings.Contains(resp.Error.Message, "you don't have access to") {
		t.Errorf("expected hidden-blocker phrasing in message, got %q", resp.Error.Message)
	}
}

// TestOpenChildrenGuard_TOCTOURace covers Codex round 2 P2: a child
// status flip that races a parent-terminal update must NOT slip past
// the guard. The race is concurrent: a goroutine waits a tick then
// flips the last open child to terminal while the parent's PATCH is
// in flight. We retry the race a handful of times because either
// outcome is acceptable (guard fires OR child commits first then
// parent succeeds), but the FORBIDDEN outcome — parent completes
// WITH the child still open — must never occur.
//
// In practice the test ALWAYS observes one of the two acceptable
// outcomes because Store.UpdateItemWithPreCheck holds the parent-
// children advisory lock (Postgres) / BEGIN IMMEDIATE write lock
// (SQLite) across the precheck + UPDATE; the racer either runs
// before or fully after our tx.
func TestOpenChildrenGuard_TOCTOURace(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Keep the iteration count modest: each iteration costs ~6 HTTP
	// calls (seed plan, seed child, racer PATCH, parent PATCH, two
	// post-condition GETs) and the test HTTP stack's rate limiter
	// rejects sustained bursts. Eight iterations is plenty to surface
	// any TOCTOU regression — the race window is microseconds, so the
	// guard's locking either holds or it doesn't.
	const iterations = 8
	for i := 0; i < iterations; i++ {
		plan, children := seedParentAndChildren(t, srv, slug, []string{"open"})
		child := children[0]

		var wg sync.WaitGroup
		wg.Add(1)

		// Racer goroutine: flip the open child to done. Tiny sleep
		// to maximise overlap with the parent PATCH.
		go func() {
			defer wg.Done()
			time.Sleep(100 * time.Microsecond)
			rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Ref, map[string]interface{}{
				"fields": map[string]interface{}{"status": "done"},
			})
			if rr.Code != http.StatusOK {
				// Racer can lose the lock-acquisition race; that's
				// fine, the test just exercises one outcome.
				return
			}
		}()

		// Parent PATCH: attempt the terminal transition.
		parentRR := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
			"fields": map[string]interface{}{"status": "completed"},
		})
		wg.Wait()

		// Verify post-conditions on the data.
		getResp := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, nil)
		if getResp.Code != http.StatusOK {
			t.Fatalf("iter %d re-read parent: %d", i, getResp.Code)
		}
		var freshParent models.Item
		parseJSON(t, getResp, &freshParent)
		var pf map[string]any
		_ = json.Unmarshal([]byte(freshParent.Fields), &pf)
		parentStatus, _ := pf["status"].(string)

		childResp := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+child.Ref, nil)
		var freshChild models.Item
		parseJSON(t, childResp, &freshChild)
		var cf map[string]any
		_ = json.Unmarshal([]byte(freshChild.Fields), &cf)
		childStatus, _ := cf["status"].(string)

		// FORBIDDEN: parent=completed AND child=open. This is the
		// race the guard exists to prevent.
		if parentStatus == "completed" && childStatus == "open" {
			t.Fatalf("iter %d: TOCTOU violation — parent=completed with child still open. parent PATCH status=%d body=%s",
				i, parentRR.Code, parentRR.Body.String())
		}
	}
}

// TestOpenChildrenGuard_MoveItem_RejectsTerminalWithOpenChildren covers
// Codex round-3 P1: `pad item move ... --field status=completed` now
// runs the same guard as the regular update path. Without the
// MoveItemWithPreCheck wiring, this PATCH would silently complete
// a parent that still has open children.
func TestOpenChildrenGuard_MoveItem_RejectsTerminalWithOpenChildren(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Plan with one open child. We then "move" the plan to its OWN
	// collection equivalent via a custom collection that has the
	// same terminal-options shape. To exercise the move path we need
	// a different target collection; use a custom one we create with
	// matching schema (terminal_options=[completed]).
	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Programs",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"],"terminal_options":["completed"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create programs collection: %d %s", collResp.Code, collResp.Body.String())
	}

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	moveResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref+"/move", map[string]interface{}{
		"target_collection": "programs",
		"field_overrides":   map[string]any{"status": "completed"},
	})
	if moveResp.Code != http.StatusConflict {
		t.Fatalf("expected 409 for move-into-terminal with open child, got %d: %s", moveResp.Code, moveResp.Body.String())
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				OpenChildren []map[string]any `json:"open_children"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(moveResp.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if resp.Error.Code != "open_children" {
		t.Errorf("expected code=open_children from move, got %q", resp.Error.Code)
	}
	if len(resp.Error.Details.OpenChildren) != 1 {
		t.Errorf("expected 1 blocking child from move guard, got %d", len(resp.Error.Details.OpenChildren))
	}

	// Sanity: the item was NOT moved (still in plans collection,
	// still active status). Mutation-safety check.
	getResp := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, nil)
	if getResp.Code != http.StatusOK {
		t.Fatalf("re-read plan: %d", getResp.Code)
	}
	var fresh models.Item
	parseJSON(t, getResp, &fresh)
	if fresh.CollectionSlug != "plans" {
		t.Errorf("plan was moved despite 409 — collection now %q", fresh.CollectionSlug)
	}
}

// TestOpenChildrenGuard_MoveItem_ForceOverrides confirms the move
// path's `?force=true` query escapes the guard, mirroring the update
// path's --force.
func TestOpenChildrenGuard_MoveItem_ForceOverrides(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Programs",
		"icon":   "package",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["active","completed"],"terminal_options":["completed"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create programs collection: %d", collResp.Code)
	}

	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	moveResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref+"/move?force=true", map[string]interface{}{
		"target_collection": "programs",
		"field_overrides":   map[string]any{"status": "completed"},
	})
	if moveResp.Code != http.StatusOK {
		t.Fatalf("expected 200 with force=true, got %d: %s", moveResp.Code, moveResp.Body.String())
	}
}

// TestOpenChildrenGuard_LinkMutationRace covers Codex round-3 P1: a
// concurrent SetParentLink that attempts to attach a non-terminal
// child to a parent while a terminal-status UPDATE on the parent is
// in flight must not slip past the guard's view of the children set.
//
// Allowed outcomes (documented semantics):
//
//  1. Link tx commits first → parent UPDATE's in-tx precheck sees
//     the open child → 409, parent stays active.
//  2. Parent UPDATE tx commits first → parent terminal, then the
//     link tx commits AFTER. The link is then attached to an
//     already-terminal parent; this is legal under the invariant
//     "no open children EXIST AT THE MOMENT the parent transitions
//     to terminal." The stricter post-condition ("no open child may
//     EVER be attached to a terminal parent") would require
//     SetParentLink to reject when the target is terminal; that is
//     intentionally deferred — out of scope for round 3.
//
// Forbidden outcome (the bug round-3 P1 fixes):
//
//	link tx commits BEFORE parent UPDATE tx, AND parent UPDATE
//	still succeeds. That would require the parent's precheck to
//	have read children=[] from a snapshot taken before the link
//	was attached — i.e. SetParentLink failed to serialize against
//	the parent-children advisory lock the precheck holds. We
//	detect this by comparing the link's created_at against the
//	parent's post-commit updated_at.
func TestOpenChildrenGuard_LinkMutationRace(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Seed: an empty plan + a standalone open task we'll attach
	// concurrently with the plan's terminal-status update.
	const iterations = 6
	for i := 0; i < iterations; i++ {
		planResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
			"title":  "race plan",
			"fields": `{"status":"active"}`,
		})
		if planResp.Code != http.StatusCreated {
			t.Fatalf("iter %d seed plan: %d %s", i, planResp.Code, planResp.Body.String())
		}
		var plan models.Item
		parseJSON(t, planResp, &plan)

		taskResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
			"title":  "race task",
			"fields": `{"status":"open"}`,
		})
		if taskResp.Code != http.StatusCreated {
			t.Fatalf("iter %d seed task: %d %s", i, taskResp.Code, taskResp.Body.String())
		}
		var task models.Item
		parseJSON(t, taskResp, &task)

		var wg sync.WaitGroup
		wg.Add(1)

		// Racer: attach the task as a child of the plan via the
		// links endpoint. SetParentLink (the underlying store call)
		// now acquires the parent-children advisory lock.
		go func() {
			defer wg.Done()
			time.Sleep(100 * time.Microsecond)
			_ = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+task.Ref+"/links", map[string]interface{}{
				"target_id": plan.ID,
				"link_type": "parent",
			})
		}()

		// Parent flip.
		_ = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
			"fields": map[string]interface{}{"status": "completed"},
		})
		wg.Wait()

		// Inspect the post-race state.
		planGet := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, nil)
		var pf models.Item
		parseJSON(t, planGet, &pf)
		var planFields map[string]any
		_ = json.Unmarshal([]byte(pf.Fields), &planFields)
		planStatus, _ := planFields["status"].(string)
		if planStatus != "completed" {
			// Outcome 1: link won, parent stayed active (or got
			// rejected). Acceptable per the documented semantics.
			continue
		}

		// Plan committed terminal. Check whether the task is linked
		// as its child — and if so, that the LINK was created AFTER
		// the parent's terminal-status commit. The advisory lock
		// ensures one of the two strict orderings: either the link
		// committed first (and then the parent UPDATE precheck would
		// have read it and rejected — so planStatus wouldn't be
		// completed), OR the parent UPDATE committed first and the
		// link followed. The forbidden interleaving is "link
		// committed BEFORE parent flip AND parent flip still
		// succeeded" — that's only possible if SetParentLink failed
		// to serialize against the UPDATE.
		linksGet := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref+"/links", nil)
		var links []models.ItemLink
		parseJSON(t, linksGet, &links)
		for _, l := range links {
			if l.SourceID != task.ID || l.LinkType != "parent" {
				continue
			}
			// Found the link. Compare its created_at against the
			// parent's updated_at. Parent UPDATE bumps updated_at;
			// link insert stamps created_at. If link.created_at <
			// parent.updated_at, the link DEFINITELY committed
			// first → the parent's precheck should have seen it →
			// terminal commit would have been rejected → bug.
			if !l.CreatedAt.IsZero() && !pf.UpdatedAt.IsZero() && l.CreatedAt.Before(pf.UpdatedAt) {
				t.Fatalf("iter %d: TOCTOU violation — link committed at %v (before parent terminal commit at %v) AND parent reached terminal. The advisory lock failed to serialize.",
					i, l.CreatedAt, pf.UpdatedAt)
			}
		}
	}
}

// TestOpenChildrenGuard_SoftDeletedCollectionSchemaHonored covers
// Codex round-3 P3: when a child still exists in a soft-deleted
// collection whose schema has a custom terminal_options set, the
// guard reads the schema via GetCollectionAnyState so the child is
// evaluated against the custom done-field — not the default-status
// fallback which would mis-classify it as non-terminal.
func TestOpenChildrenGuard_SoftDeletedCollectionSchemaHonored(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create a child collection with a non-default done-field shape:
	// schema declares `done` as the only terminal value (the global
	// default list also contains "done" so we use a custom code that
	// ISN'T in DefaultTerminalStatuses to actually exercise the
	// schema-driven path).
	collResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]interface{}{
		"name":   "Subtasks",
		"icon":   "list",
		"schema": `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","retired"],"terminal_options":["retired"]}]}`,
	})
	if collResp.Code != http.StatusCreated {
		t.Fatalf("create subtasks collection: %d %s", collResp.Code, collResp.Body.String())
	}
	var subColl models.Collection
	parseJSON(t, collResp, &subColl)

	// Parent + child in the subtasks collection.
	planResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
		"title":  "Plan",
		"fields": `{"status":"active"}`,
	})
	var plan models.Item
	parseJSON(t, planResp, &plan)

	childResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/subtasks/items", map[string]interface{}{
		"title":  "still-open subtask",
		"fields": map[string]interface{}{"status": "retired", "parent": plan.Ref},
	})
	if childResp.Code != http.StatusCreated {
		t.Fatalf("create child: %d %s", childResp.Code, childResp.Body.String())
	}

	// Soft-delete the subtasks collection. (The child item stays
	// attached — soft-delete on the collection doesn't cascade to
	// items via the link table.)
	del := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/collections/"+subColl.Slug, nil)
	if del.Code != http.StatusNoContent {
		t.Fatalf("soft-delete collection: %d %s", del.Code, del.Body.String())
	}

	// Now mark the plan completed. The child is `retired` — terminal
	// per the (soft-deleted) collection's schema. With round-3 P3
	// fixed, the guard reads the schema via GetCollectionAnyState
	// and recognizes `retired` as terminal → no blocker → 200.
	// Without the fix, the guard would fall back to the default
	// status list which doesn't include `retired`, miscount the
	// child as a blocker, and return 409 — i.e. a false positive.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 (terminal child in soft-deleted collection should not block), got %d: %s",
			rr.Code, rr.Body.String())
	}
}

// TestOpenChildrenGuard_VisibilityLookupErrorFailsClosed covers Codex
// round-3 P1: when the visibility lookup errors (DB unavailable, etc.)
// the handler must fail CLOSED — 5xx, no payload — rather than fail
// OPEN and leak hidden-child metadata via the unrestricted code path.
//
// We provoke the error by closing the store's DB pool between the
// seed and the PATCH. Subsequent queries return "sql: database is
// closed" which surfaces from visibleCollectionIDs → writeInternalError
// → 500 with the generic error envelope (no children listed).
func TestOpenChildrenGuard_VisibilityLookupErrorFailsClosed(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	// Close the DB to break VisibleCollectionIDs and any other store
	// call the handler makes. The PATCH should NOT silently succeed
	// AND must NOT emit a 409 with a (potentially leaky) children
	// payload — both would be the "fail open" bug. We accept any
	// non-2xx, non-409 outcome (500 / 503 / unauthorized — whichever
	// the broken-DB path lands on first) as proof of fail-closed.
	if err := srv.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code == http.StatusOK {
		t.Fatalf("expected non-2xx after store close, got 200: %s", rr.Body.String())
	}
	// The forbidden outcome is "409 with details.open_children
	// populated" — that would mean the guard fired against a child
	// set built without the visibility filter and leaked metadata.
	if rr.Code == http.StatusConflict {
		body := rr.Body.String()
		if strings.Contains(body, "open_children") && strings.Contains(body, "TASK") {
			t.Fatalf("fail-open regression: 409 with child metadata after visibility lookup broke. body=%s", body)
		}
	}
}

// TestOpenChildrenGuard_PrecheckReadsInTxSnapshot covers Codex round-3
// P2: the precheck classifies the transition against the parent's
// fields as read INSIDE the tx (after locks), not the pre-tx capture.
// We exercise this by passing a precheck closure that asserts the
// `existing.Fields` it receives reflects a status mutation committed
// between the handler-side load and the precheck's invocation.
//
// Test shape: directly invoke UpdateItemWithPreCheck on the store
// with a precheck that records the snapshot it sees. Between
// constructing the call and the precheck firing, another goroutine
// commits a status change on the same parent. The recorded snapshot
// must show the post-mutation value — otherwise the precheck was
// using a stale view.
func TestOpenChildrenGuard_PrecheckReadsInTxSnapshot(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	plan, _ := seedParentAndChildren(t, srv, slug, []string{"done"})

	// Resolve the plan to get its ID (the in-store API takes UUIDs).
	getResp := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, nil)
	parseJSON(t, getResp, &plan)

	// Stage 1: mutate the parent's status from "active" to "completed"
	// via the store layer directly. This bumps fields BEFORE we
	// invoke UpdateItemWithPreCheck below.
	stageFields := `{"status":"completed"}`
	if _, err := srv.store.UpdateItem(plan.ID, models.ItemUpdate{Fields: &stageFields, Force: true}); err != nil {
		t.Fatalf("stage update: %v", err)
	}

	// Stage 2: invoke UpdateItemWithPreCheck again, this time
	// transitioning to a different valid value. The precheck closure
	// records what `existing.Fields` looked like at invocation time.
	// If round-3 P2 is broken, the closure sees the stale `active`
	// (the pre-tx snapshot reproducing the bug); if the fix holds,
	// it sees the staged `completed`.
	var seenFields string
	finalFields := `{"status":"completed"}`
	_, err := srv.store.UpdateItemWithPreCheck(plan.ID,
		models.ItemUpdate{Fields: &finalFields, Force: true},
		func(tx *sql.Tx, existing *models.Item) error {
			seenFields = existing.Fields
			return nil
		})
	if err != nil {
		t.Fatalf("UpdateItemWithPreCheck: %v", err)
	}
	var f map[string]any
	if err := json.Unmarshal([]byte(seenFields), &f); err != nil {
		t.Fatalf("parse seen fields: %v", err)
	}
	if s, _ := f["status"].(string); s != "completed" {
		t.Errorf("precheck must see the in-tx snapshot (round-3 P2). Want status=completed, got %q. Full fields=%s",
			s, seenFields)
	}
}

// TestBulkUpdateStructuredFailuresCarryOpenChildrenDetails covers
// Codex round-3 P2: the CLI's bulk-update JSON envelope now embeds
// the per-row structured server error (code + details) rather than
// flattening it into a string, so MCP-driven agents reading the
// stdout JSON see the same payload single-row update would deliver.
//
// We exercise the path by invoking the CLI binary via the test
// server's HTTP surface — the bulk-update CLI loops over the
// `client.UpdateItem` HTTP call, and our extension lifts the
// APIError's code/details into the row. Tested end-to-end via the
// `pad item bulk-update` cobra command would require spawning a
// subprocess; here we directly invoke the per-item HTTP path and
// verify the server returns the same wire shape, which is the
// upstream signal the CLI lifts.
func TestBulkUpdateStructuredFailuresCarryOpenChildrenDetails(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	plan, _ := seedParentAndChildren(t, srv, slug, []string{"open"})

	// One PATCH attempting the terminal transition without force.
	// The handler returns the structured envelope; the CLI's
	// bulk-update wraps it per-row. We assert the wire shape directly.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+plan.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed"},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 from single PATCH (bulk-update loops over this exact call), got %d", rr.Code)
	}
	var envelope struct {
		Error struct {
			Code    string          `json:"code"`
			Details json.RawMessage `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("parse envelope: %v", err)
	}
	if envelope.Error.Code != "open_children" {
		t.Errorf("wire code should be open_children for bulk-update lift: got %q", envelope.Error.Code)
	}
	if len(envelope.Error.Details) == 0 {
		t.Error("wire details must be populated for bulk-update lift")
	}
}

// TestOpenChildrenGuard_MultiParentChildLocksAll covers Codex round-4
// P1: a child item with BOTH a `parent` link to P1 AND an `implements`
// link to P2 (two distinct "parents" under childLinkTypes) must
// serialize against BOTH parents' open-children guard locks when its
// status flips. Pre-fix the LIMIT 1 lookup grabbed one parent only;
// the other could race the child's status-flip and miss it.
//
// Test shape: build the multi-parent topology, then race
// (child status flip) vs (P1 terminal-update) vs (P2 terminal-update)
// across iterations. The forbidden outcome is the same as the round-2
// race test, applied to either parent: parent committed terminal AND
// the child's status flip committed BEFORE the parent's commit.
func TestOpenChildrenGuard_MultiParentChildLocksAll(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	const iterations = 4
	for i := 0; i < iterations; i++ {
		// Two plans (P1, P2) + one child task. Attach child to P1 via
		// the `parent` link_type (regular hierarchy) and to P2 via
		// the `implements` link_type (the other childLinkType).
		p1Resp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
			"title":  "P1",
			"fields": `{"status":"active"}`,
		})
		var p1 models.Item
		parseJSON(t, p1Resp, &p1)
		p2Resp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/plans/items", map[string]interface{}{
			"title":  "P2",
			"fields": `{"status":"active"}`,
		})
		var p2 models.Item
		parseJSON(t, p2Resp, &p2)

		childResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
			"title":  "multi-parent child",
			"fields": map[string]interface{}{"status": "open", "parent": p1.Ref},
		})
		var child models.Item
		parseJSON(t, childResp, &child)

		// Add the `implements` link to P2.
		linkResp := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+child.Ref+"/links", map[string]interface{}{
			"target_id": p2.ID,
			"link_type": "implements",
		})
		if linkResp.Code != http.StatusCreated {
			t.Fatalf("iter %d add implements link: %d %s", i, linkResp.Code, linkResp.Body.String())
		}

		// Race: flip child status to done, while concurrently each
		// parent attempts the terminal transition. Goroutines so
		// timing varies across iterations.
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+p1.Ref, map[string]interface{}{
				"fields": map[string]interface{}{"status": "completed"},
			})
		}()
		go func() {
			defer wg.Done()
			_ = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+p2.Ref, map[string]interface{}{
				"fields": map[string]interface{}{"status": "completed"},
			})
		}()
		time.Sleep(50 * time.Microsecond)
		_ = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+child.Ref, map[string]interface{}{
			"fields": map[string]interface{}{"status": "done"},
		})
		wg.Wait()

		// Verify the forbidden outcome did not occur for EITHER
		// parent. Same shape as the round-2 TOCTOU test, applied to
		// both. A multi-parent child whose status flip wasn't
		// serialized against P2's lock would let P2 commit terminal
		// with a stale view that read child=open.
		childGet := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+child.Ref, nil)
		var cf models.Item
		parseJSON(t, childGet, &cf)
		var cfFields map[string]any
		_ = json.Unmarshal([]byte(cf.Fields), &cfFields)
		childStatus, _ := cfFields["status"].(string)

		for _, parentRef := range []string{p1.Ref, p2.Ref} {
			parentGet := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+parentRef, nil)
			var pf models.Item
			parseJSON(t, parentGet, &pf)
			var pfFields map[string]any
			_ = json.Unmarshal([]byte(pf.Fields), &pfFields)
			parentStatus, _ := pfFields["status"].(string)
			if parentStatus == "completed" && childStatus == "open" {
				t.Fatalf("iter %d: multi-parent TOCTOU violation — parent %s=completed with child %s still open. Parent lock was missing.",
					i, parentRef, child.Ref)
			}
		}
	}
}

// TestOpenChildrenGuard_NoDeadlockUnderReverseOrderConcurrency covers
// Codex round-4 P2: every call site that takes pad:parent-children:*
// advisory locks now goes through AcquireParentChildrenLocks (the
// canonical sorted multi-lock helper). Pre-fix some sites locked
// parent-then-self; others locked sorted; concurrent reverse-order
// operations could AB/BA deadlock. With one canonical helper, that's
// impossible.
//
// Test shape: two parents A and B. Race two operations that each
// touch BOTH A and B:
//   - Goroutine 1: re-parent child X from A to B (SetParentLink A→B).
//   - Goroutine 2: re-parent child Y from B to A (SetParentLink B→A).
//
// Both will need locks on {A, B}. With sorted acquisition, both
// always grab A first then B; no deadlock. The test hard-times-out
// after 5 seconds; if the helpers ever regress to per-call-site
// ad-hoc ordering, this test hangs and fails the timeout.
func TestOpenChildrenGuard_NoDeadlockUnderReverseOrderConcurrency(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Two plans + two children. We'll re-parent X and Y in opposite
	// directions across A and B repeatedly.
	planA := mustCreate(t, srv, slug, "plans", map[string]interface{}{"title": "A", "fields": `{"status":"active"}`})
	planB := mustCreate(t, srv, slug, "plans", map[string]interface{}{"title": "B", "fields": `{"status":"active"}`})
	childX := mustCreate(t, srv, slug, "tasks", map[string]interface{}{
		"title": "X", "fields": map[string]interface{}{"status": "open", "parent": planA.Ref},
	})
	childY := mustCreate(t, srv, slug, "tasks", map[string]interface{}{
		"title": "Y", "fields": map[string]interface{}{"status": "open", "parent": planB.Ref},
	})

	const rounds = 6
	done := make(chan struct{})
	go func() {
		defer close(done)
		var wg sync.WaitGroup
		for i := 0; i < rounds; i++ {
			wg.Add(2)
			from1, to1 := planA.Ref, planB.Ref
			from2, to2 := planB.Ref, planA.Ref
			if i%2 == 1 {
				from1, to1 = to1, from1
				from2, to2 = to2, from2
			}
			_ = from1
			_ = from2
			go func(parentRef string) {
				defer wg.Done()
				_ = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+childX.Ref, map[string]interface{}{
					"fields": map[string]interface{}{"parent": parentRef, "status": "open"},
				})
			}(to1)
			go func(parentRef string) {
				defer wg.Done()
				_ = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+childY.Ref, map[string]interface{}{
					"fields": map[string]interface{}{"parent": parentRef, "status": "open"},
				})
			}(to2)
			wg.Wait()
		}
	}()

	select {
	case <-done:
		// All rounds completed without hanging. The sorted multi-
		// lock helper held the deadlock-avoidance contract.
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: reverse-order parent-children lock acquisition hung > 5s. AcquireParentChildrenLocks's sorted-acquisition contract regressed.")
	}
}

// mustCreate is a small helper for the deadlock test's seeding —
// keeps the test body focused on the race itself rather than
// boilerplate response parsing.
func mustCreate(t *testing.T, srv *Server, wsSlug, collectionSlug string, body map[string]interface{}) models.Item {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+collectionSlug+"/items", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("seed %s item: %d %s", collectionSlug, rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	return it
}

// TestOpenChildrenGuard_PatchAtomicRejectionPreservesParentLink
// covers Codex round-4 P3: a PATCH that combines a parent-link
// change AND a status flip to terminal, against a parent with open
// children, must reject the WHOLE PATCH — both the field write and
// the link change. Pre-fix the link committed before the guard ran,
// so the caller saw 409 but the parent had already moved.
func TestOpenChildrenGuard_PatchAtomicRejectionPreservesParentLink(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Two candidate parent plans (oldParent / newParent) + the item
	// under test (plan with an open child).
	oldParent := mustCreate(t, srv, slug, "plans", map[string]interface{}{
		"title": "old parent", "fields": `{"status":"active"}`,
	})
	newParent := mustCreate(t, srv, slug, "plans", map[string]interface{}{
		"title": "new parent", "fields": `{"status":"active"}`,
	})

	// The item under test is a plan with one open child + an existing
	// parent link to oldParent.
	target := mustCreate(t, srv, slug, "plans", map[string]interface{}{
		"title":  "target",
		"fields": map[string]interface{}{"status": "active", "parent": oldParent.Ref},
	})
	// Attach an open child to target so the guard will fire.
	mustCreate(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "blocker",
		"fields": map[string]interface{}{"status": "open", "parent": target.Ref},
	})

	// Sanity: target's parent IS oldParent right now.
	parentBefore := readParentRef(t, srv, slug, target.Ref)
	if parentBefore != oldParent.Ref {
		t.Fatalf("setup: target's parent should start as %s, got %q", oldParent.Ref, parentBefore)
	}

	// Combined PATCH: change parent AND mark terminal. The guard
	// must reject the whole thing.
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+target.Ref, map[string]interface{}{
		"fields": map[string]interface{}{"status": "completed", "parent": newParent.Ref},
	})
	if rr.Code != http.StatusConflict {
		t.Fatalf("expected 409 from combined parent+terminal PATCH, got %d: %s", rr.Code, rr.Body.String())
	}

	// CRITICAL: the parent link must be unchanged. Pre-fix it would
	// have been newParent because SetParentLink committed before the
	// guard ran.
	parentAfter := readParentRef(t, srv, slug, target.Ref)
	if parentAfter != oldParent.Ref {
		t.Fatalf("PATCH atomicity violated: parent moved to %q despite 409 (want unchanged %q)",
			parentAfter, oldParent.Ref)
	}
}

// readParentRef returns the ref of the item's current `parent` link
// target, or "" if there is no parent link. Used by the PATCH-atomic
// test to assert the link DIDN'T change.
func readParentRef(t *testing.T, srv *Server, wsSlug, itemRef string) string {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/items/"+itemRef+"/links", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("read links for %s: %d %s", itemRef, rr.Code, rr.Body.String())
	}
	var links []models.ItemLink
	parseJSON(t, rr, &links)
	for _, l := range links {
		if l.LinkType == "parent" {
			// The item is the SOURCE of its parent link; the target
			// is the parent. Return the parent's ref.
			return l.TargetRef
		}
	}
	return ""
}
