package server

// IDEA-1494 — handler-level coverage for the open-children guard on
// `pad item update` (handleUpdateItem). Each test seeds a parent +
// children via the public HTTP API so we exercise the same code path
// CLI / MCP traffic hits.

import (
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
