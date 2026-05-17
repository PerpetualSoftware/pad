package server

// IDEA-1494 — handler-level coverage for the open-children guard on
// `pad item update` (handleUpdateItem). Each test seeds a parent +
// children via the public HTTP API so we exercise the same code path
// CLI / MCP traffic hits.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
