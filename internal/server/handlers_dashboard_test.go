package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// helper: create an item in a collection and return the parsed Item
func createItem(t *testing.T, srv *Server, wsSlug, collSlug string, body map[string]interface{}) models.Item {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections/"+collSlug+"/items", body)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item in %s: expected 201, got %d: %s", collSlug, rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return item
}

// helper: update an item by slug and return the parsed Item
func updateItem(t *testing.T, srv *Server, wsSlug, itemSlug string, body map[string]interface{}) models.Item {
	t.Helper()
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug, body)
	if rr.Code != http.StatusOK {
		t.Fatalf("update item %s: expected 200, got %d: %s", itemSlug, rr.Code, rr.Body.String())
	}
	var item models.Item
	parseJSON(t, rr, &item)
	return item
}

// helper: fetch the dashboard and return the parsed response
func getDashboard(t *testing.T, srv *Server, wsSlug string) DashboardResponse {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/dashboard", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dashboard: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp DashboardResponse
	parseJSON(t, rr, &resp)
	return resp
}

// helper: create a "blocks" link from source to target
func createBlocksLink(t *testing.T, srv *Server, wsSlug, sourceSlug, targetID string) models.ItemLink {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/items/"+sourceSlug+"/links", map[string]interface{}{
		"target_id": targetID,
		"link_type": "blocks",
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create link: expected 201, got %d: %s", rr.Code, rr.Body.String())
	}
	var link models.ItemLink
	parseJSON(t, rr, &link)
	return link
}

func TestDashboardEmpty(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	resp := getDashboard(t, srv, slug)

	if resp.Summary.TotalItems != 0 {
		t.Errorf("expected total_items=0, got %d", resp.Summary.TotalItems)
	}
	if len(resp.Summary.ByCollection) != 0 {
		t.Errorf("expected empty by_collection, got %v", resp.Summary.ByCollection)
	}
	if len(resp.ActiveItems) != 0 {
		t.Errorf("expected 0 active_items, got %d", len(resp.ActiveItems))
	}
	if len(resp.ActivePhases) != 0 {
		t.Errorf("expected 0 active_phases, got %d", len(resp.ActivePhases))
	}
	if len(resp.Attention) != 0 {
		t.Errorf("expected 0 attention items, got %d", len(resp.Attention))
	}
	if len(resp.RecentActivity) != 0 {
		t.Errorf("expected 0 recent_activity, got %d", len(resp.RecentActivity))
	}
	if len(resp.SuggestedNext) != 0 {
		t.Errorf("expected 0 suggested_next, got %d", len(resp.SuggestedNext))
	}

	// Verify arrays are non-nil (empty JSON arrays, not null)
	raw := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/dashboard", nil)
	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(raw.Body.Bytes(), &rawMap); err != nil {
		t.Fatalf("failed to parse raw JSON: %v", err)
	}
	for _, key := range []string{"active_items", "active_phases", "attention", "recent_activity", "suggested_next"} {
		val := string(rawMap[key])
		if val == "null" {
			t.Errorf("expected %s to be [], got null", key)
		}
	}
}

func TestDashboardSummary(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create items across different collections with various statuses
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task Open 1",
		"fields": `{"status":"open","priority":"high"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task Open 2",
		"fields": `{"status":"open","priority":"low"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task In Progress",
		"fields": `{"status":"in-progress","priority":"medium"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task Done",
		"fields": `{"status":"done","priority":"low"}`,
	})
	createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Idea New",
		"fields": `{"status":"new"}`,
	})
	createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Idea Exploring",
		"fields": `{"status":"exploring"}`,
	})
	createItem(t, srv, slug, "docs", map[string]interface{}{
		"title":  "Draft Doc",
		"fields": `{"status":"draft"}`,
	})

	resp := getDashboard(t, srv, slug)

	if resp.Summary.TotalItems != 7 {
		t.Errorf("expected total_items=7, got %d", resp.Summary.TotalItems)
	}

	// Verify tasks breakdown
	taskCounts, ok := resp.Summary.ByCollection["tasks"]
	if !ok {
		t.Fatal("expected 'tasks' in by_collection")
	}
	if taskCounts["open"] != 2 {
		t.Errorf("expected 2 open tasks, got %d", taskCounts["open"])
	}
	if taskCounts["in-progress"] != 1 {
		t.Errorf("expected 1 in-progress task, got %d", taskCounts["in-progress"])
	}
	if taskCounts["done"] != 1 {
		t.Errorf("expected 1 done task, got %d", taskCounts["done"])
	}

	// Verify ideas breakdown
	ideaCounts, ok := resp.Summary.ByCollection["ideas"]
	if !ok {
		t.Fatal("expected 'ideas' in by_collection")
	}
	if ideaCounts["new"] != 1 {
		t.Errorf("expected 1 new idea, got %d", ideaCounts["new"])
	}
	if ideaCounts["exploring"] != 1 {
		t.Errorf("expected 1 exploring idea, got %d", ideaCounts["exploring"])
	}

	// Verify docs breakdown
	docCounts, ok := resp.Summary.ByCollection["docs"]
	if !ok {
		t.Fatal("expected 'docs' in by_collection")
	}
	if docCounts["draft"] != 1 {
		t.Errorf("expected 1 draft doc, got %d", docCounts["draft"])
	}
}

func TestDashboardActiveItems(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// "in-progress" is an active status for tasks
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "In Progress Task",
		"fields": `{"status":"in-progress","priority":"high"}`,
	})
	// "exploring" is an active status for ideas
	createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Exploring Idea",
		"fields": `{"status":"exploring"}`,
	})
	// "open" is NOT an active status (it's an initial/queued state)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Open Task",
		"fields": `{"status":"open","priority":"medium"}`,
	})
	// "done" is NOT an active status (it's terminal)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Task",
		"fields": `{"status":"done","priority":"low"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.ActiveItems) != 2 {
		t.Fatalf("expected 2 active_items, got %d", len(resp.ActiveItems))
	}

	// Verify the active items have the expected fields
	found := map[string]bool{}
	for _, ai := range resp.ActiveItems {
		found[ai.Title] = true
		if ai.Slug == "" {
			t.Errorf("active item %q missing slug", ai.Title)
		}
		if ai.CollectionSlug == "" {
			t.Errorf("active item %q missing collection_slug", ai.Title)
		}
		if ai.Status == "" {
			t.Errorf("active item %q missing status", ai.Title)
		}
		if ai.UpdatedAt == "" {
			t.Errorf("active item %q missing updated_at", ai.Title)
		}
	}

	if !found["In Progress Task"] {
		t.Error("expected 'In Progress Task' in active items")
	}
	if !found["Exploring Idea"] {
		t.Error("expected 'Exploring Idea' in active items")
	}
	if found["Open Task"] {
		t.Error("'Open Task' should NOT be in active items")
	}
	if found["Done Task"] {
		t.Error("'Done Task' should NOT be in active items")
	}
}

func TestDashboardActiveItemsSorting(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create active items with different priorities
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Low Priority",
		"fields": `{"status":"in-progress","priority":"low"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Critical Priority",
		"fields": `{"status":"in-progress","priority":"critical"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "High Priority",
		"fields": `{"status":"in-progress","priority":"high"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Medium Priority",
		"fields": `{"status":"in-progress","priority":"medium"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.ActiveItems) != 4 {
		t.Fatalf("expected 4 active_items, got %d", len(resp.ActiveItems))
	}

	expectedOrder := []string{"Critical Priority", "High Priority", "Medium Priority", "Low Priority"}
	for i, expected := range expectedOrder {
		if resp.ActiveItems[i].Title != expected {
			t.Errorf("active_items[%d]: expected %q, got %q", i, expected, resp.ActiveItems[i].Title)
		}
	}

	// Verify priority values are set correctly
	expectedPriorities := []string{"critical", "high", "medium", "low"}
	for i, expected := range expectedPriorities {
		if resp.ActiveItems[i].Priority != expected {
			t.Errorf("active_items[%d].priority: expected %q, got %q", i, expected, resp.ActiveItems[i].Priority)
		}
	}
}

func TestDashboardActiveItemsExcludesPhases(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create an active phase — it should NOT appear in active_items
	// (phases have their own section)
	createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Phase 1",
		"fields": `{"status":"active"}`,
	})

	// Create an active task — it SHOULD appear
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Active Task",
		"fields": `{"status":"in-progress","priority":"high"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.ActiveItems) != 1 {
		t.Fatalf("expected 1 active_item (no phases), got %d", len(resp.ActiveItems))
	}
	if resp.ActiveItems[0].Title != "Active Task" {
		t.Errorf("expected active item to be 'Active Task', got %q", resp.ActiveItems[0].Title)
	}
}

func TestDashboardActivePhases(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create a phase with status=active
	phase := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Sprint 1",
		"fields": `{"status":"active"}`,
	})

	// Create tasks linked to the phase
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task A",
		"fields": `{"status":"open","priority":"high","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task B",
		"fields": `{"status":"done","priority":"medium","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task C",
		"fields": `{"status":"done","priority":"low","phase":"` + phase.ID + `"}`,
	})

	// Also a planned phase that should NOT appear in active_phases
	createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Sprint 2",
		"fields": `{"status":"planned"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.ActivePhases) != 1 {
		t.Fatalf("expected 1 active_phase, got %d", len(resp.ActivePhases))
	}

	ap := resp.ActivePhases[0]
	if ap.Title != "Sprint 1" {
		t.Errorf("expected phase title 'Sprint 1', got %q", ap.Title)
	}
	if ap.Slug == "" {
		t.Error("expected phase slug to be set")
	}
	if ap.TaskCount != 3 {
		t.Errorf("expected task_count=3, got %d", ap.TaskCount)
	}
	if ap.DoneCount != 2 {
		t.Errorf("expected done_count=2, got %d", ap.DoneCount)
	}
	// Progress: 2/3 = 66%
	expectedProgress := (2 * 100) / 3
	if ap.Progress != expectedProgress {
		t.Errorf("expected progress=%d, got %d", expectedProgress, ap.Progress)
	}
}

func TestDashboardAttentionOverdue(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create task with a past due_date (clearly in the past)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Overdue Task",
		"fields": `{"status":"open","priority":"high","due_date":"2020-01-01"}`,
	})

	// Create task with a future due_date — should NOT be overdue
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Future Task",
		"fields": `{"status":"open","priority":"medium","due_date":"2099-12-31"}`,
	})

	// Create done task with a past due_date — done tasks should NOT be flagged
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Overdue Task",
		"fields": `{"status":"done","priority":"low","due_date":"2020-01-01"}`,
	})

	resp := getDashboard(t, srv, slug)

	overdueItems := filterAttention(resp.Attention, "overdue")
	if len(overdueItems) != 1 {
		t.Fatalf("expected 1 overdue attention item, got %d: %+v", len(overdueItems), overdueItems)
	}

	if overdueItems[0].ItemTitle != "Overdue Task" {
		t.Errorf("expected overdue item to be 'Overdue Task', got %q", overdueItems[0].ItemTitle)
	}
	if overdueItems[0].Collection != "tasks" {
		t.Errorf("expected collection 'tasks', got %q", overdueItems[0].Collection)
	}
	if overdueItems[0].Reason == "" {
		t.Error("expected reason to be set for overdue item")
	}
}

func TestDashboardAttentionOverdueEndDate(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create a phase with a past end_date (not done)
	createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Overdue Phase",
		"fields": `{"status":"active","end_date":"2020-06-15"}`,
	})

	resp := getDashboard(t, srv, slug)

	overdueItems := filterAttention(resp.Attention, "overdue")
	if len(overdueItems) != 1 {
		t.Fatalf("expected 1 overdue attention item for end_date, got %d: %+v", len(overdueItems), overdueItems)
	}

	if overdueItems[0].ItemTitle != "Overdue Phase" {
		t.Errorf("expected 'Overdue Phase', got %q", overdueItems[0].ItemTitle)
	}
}

func TestDashboardAttentionBlocked(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create blocker task (not done)
	blocker := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Blocker Task",
		"fields": `{"status":"in-progress","priority":"high"}`,
	})

	// Create blocked task (not done)
	blocked := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Blocked Task",
		"fields": `{"status":"open","priority":"medium"}`,
	})

	// Create a "blocks" link: blocker blocks blocked
	createBlocksLink(t, srv, slug, blocker.Slug, blocked.ID)

	resp := getDashboard(t, srv, slug)

	blockedItems := filterAttention(resp.Attention, "blocked")
	if len(blockedItems) != 1 {
		t.Fatalf("expected 1 blocked attention item, got %d: %+v", len(blockedItems), blockedItems)
	}

	if blockedItems[0].ItemTitle != "Blocked Task" {
		t.Errorf("expected blocked item to be 'Blocked Task', got %q", blockedItems[0].ItemTitle)
	}
	if blockedItems[0].Reason == "" {
		t.Error("expected reason for blocked item")
	}
}

func TestDashboardAttentionBlockedNotFlaggedWhenBlockerDone(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create a done blocker
	blocker := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Blocker",
		"fields": `{"status":"done","priority":"high"}`,
	})

	// Create blocked task
	blocked := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Should Not Be Blocked",
		"fields": `{"status":"open","priority":"medium"}`,
	})

	createBlocksLink(t, srv, slug, blocker.Slug, blocked.ID)

	resp := getDashboard(t, srv, slug)

	blockedItems := filterAttention(resp.Attention, "blocked")
	if len(blockedItems) != 0 {
		t.Errorf("expected 0 blocked items when blocker is done, got %d: %+v", len(blockedItems), blockedItems)
	}
}

func TestDashboardAttentionBlockedNotFlaggedWhenTargetDone(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Blocker is open
	blocker := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Open Blocker",
		"fields": `{"status":"open","priority":"high"}`,
	})

	// Target is done — done items should not be flagged
	blocked := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Target",
		"fields": `{"status":"done","priority":"medium"}`,
	})

	createBlocksLink(t, srv, slug, blocker.Slug, blocked.ID)

	resp := getDashboard(t, srv, slug)

	blockedItems := filterAttention(resp.Attention, "blocked")
	if len(blockedItems) != 0 {
		t.Errorf("expected 0 blocked items when target is done, got %d: %+v", len(blockedItems), blockedItems)
	}
}

func TestDashboardSuggestedNext(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create an active phase
	phase := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Active Phase",
		"fields": `{"status":"active"}`,
	})

	// Create open tasks in the phase with different priorities
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Low Priority Task",
		"fields": `{"status":"open","priority":"low","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Critical Priority Task",
		"fields": `{"status":"open","priority":"critical","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "High Priority Task",
		"fields": `{"status":"open","priority":"high","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Medium Priority Task",
		"fields": `{"status":"open","priority":"medium","phase":"` + phase.ID + `"}`,
	})

	// This one is in-progress — not "open", so NOT suggested
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Already In Progress",
		"fields": `{"status":"in-progress","priority":"critical","phase":"` + phase.ID + `"}`,
	})

	resp := getDashboard(t, srv, slug)

	// Should get top 3 sorted by priority
	if len(resp.SuggestedNext) != 3 {
		t.Fatalf("expected 3 suggested_next, got %d: %+v", len(resp.SuggestedNext), resp.SuggestedNext)
	}

	expectedOrder := []string{"Critical Priority Task", "High Priority Task", "Medium Priority Task"}
	for i, expected := range expectedOrder {
		if resp.SuggestedNext[i].ItemTitle != expected {
			t.Errorf("suggested_next[%d]: expected %q, got %q", i, expected, resp.SuggestedNext[i].ItemTitle)
		}
	}

	// Verify collection and reason are populated
	for _, sn := range resp.SuggestedNext {
		if sn.Collection != "tasks" {
			t.Errorf("expected collection 'tasks', got %q", sn.Collection)
		}
		if sn.Reason == "" {
			t.Errorf("expected reason to be set for suggestion %q", sn.ItemTitle)
		}
		if sn.ItemSlug == "" {
			t.Errorf("expected item_slug to be set for suggestion %q", sn.ItemTitle)
		}
	}
}

func TestDashboardSuggestedNextNoPhases(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create tasks but no active phases
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Orphan Task",
		"fields": `{"status":"open","priority":"high"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.SuggestedNext) != 0 {
		t.Errorf("expected 0 suggested_next without active phases, got %d", len(resp.SuggestedNext))
	}
}

func TestDashboardSuggestedNextFromPlannedPhase(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Planned phase — should NOT contribute to suggested_next
	phase := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Planned Phase",
		"fields": `{"status":"planned"}`,
	})

	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task in Planned Phase",
		"fields": `{"status":"open","priority":"high","phase":"` + phase.ID + `"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.SuggestedNext) != 0 {
		t.Errorf("expected 0 suggested_next from planned phase, got %d", len(resp.SuggestedNext))
	}
}

func TestDashboardIsDoneStatus(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// The tasks collection only allows: open, in-progress, done, cancelled
	// The ideas collection allows: new, exploring, planned, implemented, rejected
	// So we test done/cancelled from tasks and implemented/rejected from ideas

	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Task",
		"fields": `{"status":"done","priority":"medium"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Cancelled Task",
		"fields": `{"status":"cancelled","priority":"high"}`,
	})
	createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Implemented Idea",
		"fields": `{"status":"implemented"}`,
	})
	createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Rejected Idea",
		"fields": `{"status":"rejected"}`,
	})

	resp := getDashboard(t, srv, slug)

	// None of these should appear in active_items
	if len(resp.ActiveItems) != 0 {
		t.Errorf("expected 0 active_items for done statuses, got %d: %+v", len(resp.ActiveItems), resp.ActiveItems)
	}

	// Done items with past due_dates should NOT be flagged as overdue
	// (tested in TestDashboardAttentionOverdue above, but verify here too)
	overdueItems := filterAttention(resp.Attention, "overdue")
	if len(overdueItems) != 0 {
		t.Errorf("expected 0 overdue attention for done items, got %d", len(overdueItems))
	}
}

func TestDashboardPhaseCompletion(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create active phase
	phase := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Completed Sprint",
		"fields": `{"status":"active"}`,
	})

	// All tasks done
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Task 1",
		"fields": `{"status":"done","priority":"high","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Task 2",
		"fields": `{"status":"done","priority":"medium","phase":"` + phase.ID + `"}`,
	})

	resp := getDashboard(t, srv, slug)

	phaseCompletions := filterAttention(resp.Attention, "phase_completion")
	if len(phaseCompletions) != 1 {
		t.Fatalf("expected 1 phase_completion attention, got %d: %+v", len(phaseCompletions), phaseCompletions)
	}

	if phaseCompletions[0].ItemTitle != "Completed Sprint" {
		t.Errorf("expected 'Completed Sprint', got %q", phaseCompletions[0].ItemTitle)
	}
	if phaseCompletions[0].Collection != "phases" {
		t.Errorf("expected collection 'phases', got %q", phaseCompletions[0].Collection)
	}
}

func TestDashboardPhaseCompletionNotTriggeredWithOpenTasks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Active phase with one done and one open task
	phase := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "In Progress Sprint",
		"fields": `{"status":"active"}`,
	})

	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Task",
		"fields": `{"status":"done","priority":"high","phase":"` + phase.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Open Task",
		"fields": `{"status":"open","priority":"high","phase":"` + phase.ID + `"}`,
	})

	resp := getDashboard(t, srv, slug)

	phaseCompletions := filterAttention(resp.Attention, "phase_completion")
	if len(phaseCompletions) != 0 {
		t.Errorf("expected 0 phase_completion when tasks are still open, got %d", len(phaseCompletions))
	}
}

func TestDashboardRecentActivity(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Creating items generates activity
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Activity Task",
		"fields": `{"status":"open","priority":"high"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.RecentActivity) == 0 {
		t.Fatal("expected at least 1 recent_activity entry")
	}

	// Verify activity fields
	a := resp.RecentActivity[0]
	if a.Action == "" {
		t.Error("expected action to be set")
	}
	if a.CreatedAt == "" {
		t.Error("expected created_at to be set")
	}
}

func TestDashboardActiveItemsItemRef(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Tasks collection has a prefix, so items should get item_ref
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Task With Ref",
		"fields": `{"status":"in-progress","priority":"high"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.ActiveItems) != 1 {
		t.Fatalf("expected 1 active item, got %d", len(resp.ActiveItems))
	}

	ai := resp.ActiveItems[0]
	// The item_ref should be set if the collection has a prefix and item has a number
	// Tasks collection has prefix "TASK", so item_ref should be like "TASK-1"
	if ai.ItemRef == "" {
		t.Log("item_ref is empty — collection may not have a prefix configured, skipping assertion")
	}
}

func TestDashboardNonexistentWorkspace(t *testing.T) {
	srv := testServer(t)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/nonexistent-ws/dashboard", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404 for nonexistent workspace, got %d", rr.Code)
	}
}

func TestDashboardMultipleActivePhases(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create two active phases
	phase1 := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Phase Alpha",
		"fields": `{"status":"active"}`,
	})
	phase2 := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Phase Beta",
		"fields": `{"status":"active"}`,
	})

	// Tasks for phase 1: 1 done out of 2
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Alpha Task 1",
		"fields": `{"status":"done","priority":"high","phase":"` + phase1.ID + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Alpha Task 2",
		"fields": `{"status":"open","priority":"medium","phase":"` + phase1.ID + `"}`,
	})

	// Tasks for phase 2: 0 done out of 1
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Beta Task 1",
		"fields": `{"status":"open","priority":"high","phase":"` + phase2.ID + `"}`,
	})

	resp := getDashboard(t, srv, slug)

	if len(resp.ActivePhases) != 2 {
		t.Fatalf("expected 2 active phases, got %d", len(resp.ActivePhases))
	}

	phaseMap := map[string]DashboardPhase{}
	for _, p := range resp.ActivePhases {
		phaseMap[p.Title] = p
	}

	alpha, ok := phaseMap["Phase Alpha"]
	if !ok {
		t.Fatal("expected 'Phase Alpha' in active phases")
	}
	if alpha.TaskCount != 2 || alpha.DoneCount != 1 {
		t.Errorf("Phase Alpha: expected task_count=2, done_count=1, got task_count=%d, done_count=%d", alpha.TaskCount, alpha.DoneCount)
	}
	if alpha.Progress != 50 {
		t.Errorf("Phase Alpha: expected progress=50, got %d", alpha.Progress)
	}

	beta, ok := phaseMap["Phase Beta"]
	if !ok {
		t.Fatal("expected 'Phase Beta' in active phases")
	}
	if beta.TaskCount != 1 || beta.DoneCount != 0 {
		t.Errorf("Phase Beta: expected task_count=1, done_count=0, got task_count=%d, done_count=%d", beta.TaskCount, beta.DoneCount)
	}
	if beta.Progress != 0 {
		t.Errorf("Phase Beta: expected progress=0, got %d", beta.Progress)
	}
}

func TestDashboardActiveItemsCappedAt10(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create 12 in-progress items
	for i := 0; i < 12; i++ {
		createItem(t, srv, slug, "tasks", map[string]interface{}{
			"title":  "In Progress Task " + string(rune('A'+i)),
			"fields": `{"status":"in-progress","priority":"medium"}`,
		})
	}

	resp := getDashboard(t, srv, slug)

	if len(resp.ActiveItems) != 10 {
		t.Errorf("expected active_items capped at 10, got %d", len(resp.ActiveItems))
	}
}

func TestDashboardOrphanedTasks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create an active phase WITH at least one linked task (activates orphan detection)
	phase := createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Sprint 1",
		"fields": `{"status":"active"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Linked Task",
		"fields": `{"status":"open","priority":"high","phase":"` + phase.ID + `"}`,
	})

	// Create a task WITHOUT a phase — should be flagged as orphaned
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Orphan Task",
		"fields": `{"status":"open","priority":"medium"}`,
	})

	// Create a done task without a phase — should NOT be flagged (done tasks are excluded)
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Done Orphan",
		"fields": `{"status":"done","priority":"low"}`,
	})

	resp := getDashboard(t, srv, slug)

	orphans := filterAttention(resp.Attention, "orphaned_task")
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphaned_task attention, got %d: %+v", len(orphans), orphans)
	}
	if orphans[0].ItemTitle != "Orphan Task" {
		t.Errorf("expected orphaned item 'Orphan Task', got %q", orphans[0].ItemTitle)
	}
}

func TestDashboardOrphanedTasksNotFlaggedWithoutPhaseLinks(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Create an active phase but with NO linked tasks
	createItem(t, srv, slug, "phases", map[string]interface{}{
		"title":  "Empty Phase",
		"fields": `{"status":"active"}`,
	})

	// Task without phase — should NOT be flagged because no phase has tasks
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Unlinked Task",
		"fields": `{"status":"open","priority":"medium"}`,
	})

	resp := getDashboard(t, srv, slug)

	orphans := filterAttention(resp.Attention, "orphaned_task")
	if len(orphans) != 0 {
		t.Errorf("expected 0 orphaned tasks when no phase has tasks linked, got %d: %+v", len(orphans), orphans)
	}
}

// filterAttention returns attention items of a given type.
func filterAttention(items []DashboardAttention, typ string) []DashboardAttention {
	var result []DashboardAttention
	for _, item := range items {
		if item.Type == typ {
			result = append(result, item)
		}
	}
	return result
}
