package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const bug3707StageSchema = `{"fields":[` +
	`{"key":"status","type":"select","options":["open","in-progress","done"],"default":"open"},` +
	`{"key":"stage","type":"select","options":["planning","building","shipped"],"terminal_options":["shipped"],"default":"planning"},` +
	`{"key":"priority","type":"select","options":["low","medium","high","critical"],"default":"medium"}]}`

func createStageCollection(t *testing.T, srv *Server, wsSlug string) {
	t.Helper()
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/collections", map[string]any{
		"name":     "Releases",
		"slug":     "releases",
		"prefix":   "REL",
		"schema":   bug3707StageSchema,
		"settings": `{"board_group_by":"stage"}`,
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create releases collection: %d %s", rr.Code, rr.Body.String())
	}
}

func linkParent(t *testing.T, srv *Server, wsSlug, childSlug, parentID string) {
	t.Helper()
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+wsSlug+"/items/"+childSlug+"/links", map[string]any{
		"target_id": parentID,
		"link_type": "parent",
	}); rr.Code != http.StatusCreated {
		t.Fatalf("link parent: %d %s", rr.Code, rr.Body.String())
	}
}

func getProjectNext(t *testing.T, srv *Server, wsSlug string) []DashboardSuggestion {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/next", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("next: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out []DashboardSuggestion
	parseJSON(t, rr, &out)
	return out
}

func getProjectStandup(t *testing.T, srv *Server, wsSlug string) StandupResponse {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/standup", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var out StandupResponse
	parseJSON(t, rr, &out)
	return out
}

func containsSuggestionTitle(list []DashboardSuggestion, title string) bool {
	for _, s := range list {
		if s.ItemTitle == title {
			return true
		}
	}
	return false
}

func containsStandupTitle(list []StandupItem, title string) bool {
	for _, s := range list {
		if s.Title == title {
			return true
		}
	}
	return false
}

// TestBUG3707_ActivePlanBranchSkipsDoneByBoardGroupBy pins Problem 2 on the
// active-plan branch: a child whose done field (board_group_by="stage") is
// terminal ("shipped") must not be suggested even when its literal `status`
// field is still "in-progress".
func TestBUG3707_ActivePlanBranchSkipsDoneByBoardGroupBy(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createStageCollection(t, srv, slug)

	plan := createItem(t, srv, slug, "plans", map[string]any{
		"title":  "Launch Plan",
		"fields": `{"status":"active"}`,
	})
	shippedChild := createItem(t, srv, slug, "releases", map[string]any{
		"title":  "Shipped child still marked in-progress",
		"fields": `{"status":"in-progress","stage":"shipped","priority":"high"}`,
	})
	linkParent(t, srv, slug, shippedChild.Slug, plan.ID)

	openChild := createItem(t, srv, slug, "releases", map[string]any{
		"title":  "Building child",
		"fields": `{"status":"open","stage":"building","priority":"medium"}`,
	})
	linkParent(t, srv, slug, openChild.Slug, plan.ID)

	dash := getDashboard(t, srv, slug)
	next := getProjectNext(t, srv, slug)
	standup := getProjectStandup(t, srv, slug)

	if containsSuggestionTitle(dash.SuggestedNext, shippedChild.Title) {
		t.Errorf("dashboard suggested_next included done active-plan child %q: %+v", shippedChild.Title, dash.SuggestedNext)
	}
	if !containsSuggestionTitle(dash.SuggestedNext, openChild.Title) {
		t.Errorf("dashboard suggested_next missing non-terminal control child %q: %+v", openChild.Title, dash.SuggestedNext)
	}
	if containsSuggestionTitle(next, shippedChild.Title) {
		t.Errorf("/next included done active-plan child %q: %+v", shippedChild.Title, next)
	}
	if !containsSuggestionTitle(next, openChild.Title) {
		t.Errorf("/next missing non-terminal control child %q: %+v", openChild.Title, next)
	}
	if containsStandupTitle(standup.SuggestedNext, shippedChild.Title) {
		t.Errorf("standup suggested_next included done active-plan child %q: %+v", shippedChild.Title, standup.SuggestedNext)
	}
	if !containsStandupTitle(standup.SuggestedNext, openChild.Title) {
		t.Errorf("standup suggested_next missing non-terminal control child %q: %+v", openChild.Title, standup.SuggestedNext)
	}
}

// TestBUG3707_OrphanBranchSkipsDoneByBoardGroupBy pins Problem 2 on the orphan
// branch: standalone items whose done field (stage) is terminal ("shipped")
// must not be admitted via either the high-priority open path or the
// in-progress path.
func TestBUG3707_OrphanBranchSkipsDoneByBoardGroupBy(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	createStageCollection(t, srv, slug)

	shippedOpen := createItem(t, srv, slug, "releases", map[string]any{
		"title":  "Shipped high open orphan",
		"fields": `{"status":"open","priority":"high","stage":"shipped"}`,
	})
	shippedInProg := createItem(t, srv, slug, "releases", map[string]any{
		"title":  "Shipped in-progress orphan",
		"fields": `{"status":"in-progress","priority":"medium","stage":"shipped"}`,
	})
	control := createItem(t, srv, slug, "releases", map[string]any{
		"title":  "Building high open orphan",
		"fields": `{"status":"open","priority":"high","stage":"building"}`,
	})

	dash := getDashboard(t, srv, slug)
	next := getProjectNext(t, srv, slug)
	standup := getProjectStandup(t, srv, slug)

	for _, forbidden := range []string{shippedOpen.Title, shippedInProg.Title} {
		if containsSuggestionTitle(dash.SuggestedNext, forbidden) {
			t.Errorf("dashboard suggested_next included done orphan %q: %+v", forbidden, dash.SuggestedNext)
		}
		if containsSuggestionTitle(next, forbidden) {
			t.Errorf("/next included done orphan %q: %+v", forbidden, next)
		}
		if containsStandupTitle(standup.SuggestedNext, forbidden) {
			t.Errorf("standup suggested_next included done orphan %q: %+v", forbidden, standup.SuggestedNext)
		}
	}
	if !containsSuggestionTitle(dash.SuggestedNext, control.Title) {
		t.Errorf("dashboard suggested_next missing non-terminal control %q: %+v", control.Title, dash.SuggestedNext)
	}
	if !containsSuggestionTitle(next, control.Title) {
		t.Errorf("/next missing non-terminal control %q: %+v", control.Title, next)
	}
	if !containsStandupTitle(standup.SuggestedNext, control.Title) {
		t.Errorf("standup suggested_next missing non-terminal control %q: %+v", control.Title, standup.SuggestedNext)
	}
}

// TestBUG3705_ActionableModeParityWithFullDashboard pins that /next and
// /standup (backed by the actionable-only dashboard build) produce the exact
// same SuggestedNext and Attention as the full dashboard build, even with a
// rich mix of open, blocked, overdue, stalled, plan-linked, and many completed
// items.
func TestBUG3705_ActionableModeParityWithFullDashboard(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// Active plan with open, in-progress, and done children.
	activePlan := createItem(t, srv, slug, "plans", map[string]any{
		"title":  "Active Roadmap",
		"fields": `{"status":"active"}`,
	})
	planInProg := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Plan task in flight",
		"fields": `{"status":"in-progress","priority":"high"}`,
	})
	linkParent(t, srv, slug, planInProg.Slug, activePlan.ID)
	planOpen := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Plan task open",
		"fields": `{"status":"open","priority":"high"}`,
	})
	linkParent(t, srv, slug, planOpen.Slug, activePlan.ID)
	planDone := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Plan task done",
		"fields": `{"status":"done","priority":"medium"}`,
	})
	linkParent(t, srv, slug, planDone.Slug, activePlan.ID)

	// Active plan whose children are all done -> triggers plan_completion attention.
	completedPlan := createItem(t, srv, slug, "plans", map[string]any{
		"title":  "Finished Plan",
		"fields": `{"status":"active"}`,
	})
	finishedChild := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "All done child",
		"fields": `{"status":"done"}`,
	})
	linkParent(t, srv, slug, finishedChild.Slug, completedPlan.ID)

	// Blocked item + its open blocker (orphaned tasks -> also exercise orphaned_task attention).
	blocker := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Blocker task",
		"fields": `{"status":"open","priority":"high"}`,
	})
	blocked := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Blocked task",
		"fields": `{"status":"open","priority":"critical"}`,
	})
	createBlocksLink(t, srv, slug, blocker.Slug, blocked.ID)

	// Overdue open task.
	createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Overdue task",
		"fields": `{"status":"open","priority":"medium","due_date":"2020-01-01"}`,
	})

	// Stalled in-progress task.
	stalled := createItem(t, srv, slug, "tasks", map[string]any{
		"title":  "Stalled task",
		"fields": `{"status":"in-progress","priority":"low"}`,
	})
	backdateItemUpdatedAt(t, srv, stalled.ID, 5)

	// 50 completed tasks (noise that actionable mode must exclude at SQL level).
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	tasksColl, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasksColl == nil {
		t.Fatalf("GetCollectionBySlug tasks: %v", err)
	}
	for i := 0; i < 50; i++ {
		if _, err := srv.store.CreateItem(ws.ID, tasksColl.ID, models.ItemCreate{
			Title:  fmt.Sprintf("Completed task %02d", i),
			Fields: `{"status":"done","priority":"medium"}`,
		}); err != nil {
			t.Fatalf("create completed task %d: %v", i, err)
		}
	}

	dash := getDashboard(t, srv, slug)
	next := getProjectNext(t, srv, slug)
	standup := getProjectStandup(t, srv, slug)

	if len(dash.SuggestedNext) == 0 {
		t.Fatal("expected non-empty dashboard.suggested_next")
	}
	if len(dash.Attention) == 0 {
		t.Fatal("expected non-empty dashboard.attention")
	}

	// Verify /next equals dashboard.suggested_next element-for-element.
	if len(next) != len(dash.SuggestedNext) {
		t.Fatalf("/next len=%d != dashboard.suggested_next len=%d\nnext=%+v\ndash=%+v",
			len(next), len(dash.SuggestedNext), next, dash.SuggestedNext)
	}
	for i := range next {
		if next[i] != dash.SuggestedNext[i] {
			t.Fatalf("/next[%d] = %+v, want %+v", i, next[i], dash.SuggestedNext[i])
		}
	}

	// Verify standup.blockers equals dashboard.attention mapped as {Ref, Title, Reason} in order.
	if len(standup.Blockers) != len(dash.Attention) {
		t.Fatalf("standup.blockers len=%d != dashboard.attention len=%d\nstandup=%+v\ndash=%+v",
			len(standup.Blockers), len(dash.Attention), standup.Blockers, dash.Attention)
	}
	for i, a := range dash.Attention {
		want := StandupItem{Ref: a.ItemRef, Title: a.ItemTitle, Reason: a.Reason}
		if standup.Blockers[i] != want {
			t.Fatalf("standup.blockers[%d] = %+v, want %+v", i, standup.Blockers[i], want)
		}
	}

	// Verify standup.suggested_next equals dashboard.suggested_next mapped as {Ref, Title, Reason} in order.
	if len(standup.SuggestedNext) != len(dash.SuggestedNext) {
		t.Fatalf("standup.suggested_next len=%d != dashboard.suggested_next len=%d",
			len(standup.SuggestedNext), len(dash.SuggestedNext))
	}
	for i, sg := range dash.SuggestedNext {
		want := StandupItem{Ref: sg.ItemRef, Title: sg.ItemTitle, Reason: sg.Reason}
		if standup.SuggestedNext[i] != want {
			t.Fatalf("standup.suggested_next[%d] = %+v, want %+v", i, standup.SuggestedNext[i], want)
		}
	}
}

// TestBUG3705_ActionableModeBoundsItemScanAndSkipsUnusedSections verifies (d):
//   - dashboardAllItemsParams sets NonTerminal: true (and preserves NoContent +
//     visibility) in actionableOnly mode, and leaves NonTerminal: false in full
//     mode; dashboardOrphanTasksParams sets NonTerminal: true in both modes.
//   - At the store level, issuing ListItems with the actionable params skips
//     all terminal items (0 of 50 completed tasks returned).
//   - Calling buildDashboard directly with actionableOnly: true skips
//     HasAgentActivity, NeedsOnboarding, Summary, OnboardingSeed, ActiveItems,
//     RecentActivity, ByRole, and StarredItems, while still producing the exact
//     same Attention, SuggestedNext, Timezone, and TimezoneNote as full mode.
func TestBUG3705_ActionableModeBoundsItemScanAndSkipsUnusedSections(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	// 1. Unit-check the item-list param helpers directly.
	collIDs := []string{"c1", "c2"}
	itemIDs := []string{"i1"}
	fullParams := dashboardAllItemsParams(collIDs, itemIDs, dashboardBuildOpts{})
	if fullParams.NonTerminal || !fullParams.NoContent || !reflect.DeepEqual(fullParams.CollectionIDs, collIDs) || !reflect.DeepEqual(fullParams.ItemIDs, itemIDs) {
		t.Fatalf("full dashboardAllItemsParams = %+v, want NonTerminal=false NoContent=true with visibility preserved", fullParams)
	}
	actParams := dashboardAllItemsParams(collIDs, itemIDs, dashboardBuildOpts{actionableOnly: true})
	if !actParams.NonTerminal || !actParams.NoContent || !reflect.DeepEqual(actParams.CollectionIDs, collIDs) || !reflect.DeepEqual(actParams.ItemIDs, itemIDs) {
		t.Fatalf("actionable dashboardAllItemsParams = %+v, want NonTerminal=true NoContent=true with visibility preserved", actParams)
	}
	orphanParams := dashboardOrphanTasksParams(collIDs, itemIDs)
	if !orphanParams.NonTerminal || !orphanParams.NoContent || orphanParams.CollectionSlug != "tasks" || !reflect.DeepEqual(orphanParams.CollectionIDs, collIDs) || !reflect.DeepEqual(orphanParams.ItemIDs, itemIDs) {
		t.Fatalf("dashboardOrphanTasksParams = %+v, want CollectionSlug=tasks NonTerminal=true NoContent=true with visibility preserved", orphanParams)
	}

	// 2. Seed a workspace where every full-dashboard section is populated,
	//    plus 50 completed tasks.
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}
	owner := mustUser(t, srv, "bounded-owner@example.com", "boundedowner", "admin")
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	tasksColl, err := srv.store.GetCollectionBySlug(ws.ID, "tasks")
	if err != nil || tasksColl == nil {
		t.Fatalf("GetCollectionBySlug tasks: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{
		Name: "Builder",
		Slug: "builder",
	})
	if err != nil {
		t.Fatalf("CreateAgentRole: %v", err)
	}
	activeTask, err := srv.store.CreateItem(ws.ID, tasksColl.ID, models.ItemCreate{
		Title:       "Active CLI task",
		Fields:      `{"status":"in-progress","priority":"high","due_date":"2020-01-01"}`,
		Source:      "cli",
		AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("CreateItem activeTask: %v", err)
	}
	if err := srv.store.StarItem(owner.ID, activeTask.ID); err != nil {
		t.Fatalf("StarItem: %v", err)
	}
	if _, err := srv.store.CreateActivity(models.Activity{
		WorkspaceID: ws.ID,
		DocumentID:  activeTask.ID,
		Action:      "updated",
		Actor:       owner.Email,
		Source:      "cli",
	}); err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}

	const completedCount = 50
	for i := 0; i < completedCount; i++ {
		if _, err := srv.store.CreateItem(ws.ID, tasksColl.ID, models.ItemCreate{
			Title:  fmt.Sprintf("Done task %02d", i),
			Fields: `{"status":"done"}`,
		}); err != nil {
			t.Fatalf("create done task %d: %v", i, err)
		}
	}

	// 3. Store-level check: the actionable params read only non-terminal items.
	allList, err := srv.store.ListItems(ws.ID, dashboardAllItemsParams(nil, nil, dashboardBuildOpts{}))
	if err != nil {
		t.Fatalf("ListItems full: %v", err)
	}
	actList, err := srv.store.ListItems(ws.ID, dashboardAllItemsParams(nil, nil, dashboardBuildOpts{actionableOnly: true}))
	if err != nil {
		t.Fatalf("ListItems actionable: %v", err)
	}
	if len(allList) != len(actList)+completedCount {
		t.Fatalf("expected full scan (%d) to exceed actionable scan (%d) by %d completed items",
			len(allList), len(actList), completedCount)
	}
	for _, it := range actList {
		if extractFieldValue(it.Fields, "status") == "done" {
			t.Fatalf("actionable item scan returned terminal item %q (%s)", it.Title, it.ID)
		}
	}

	// 4. Direct builder comparison: full vs actionableOnly.
	req := httptest.NewRequest("GET", "/api/v1/workspaces/"+slug+"/next", nil)
	ctx := WithCurrentUser(req.Context(), owner)
	ctx = contextWithWorkspaceRoleForTest(ctx, "owner")
	ctx = contextWithResolvedWorkspaceIDForTest(ctx, ws.ID)
	req = req.WithContext(ctx)

	fullResp, err := srv.buildDashboardResponse(ws.ID, req)
	if err != nil {
		t.Fatalf("buildDashboardResponse: %v", err)
	}
	actResp, err := srv.buildDashboard(ws.ID, req, dashboardBuildOpts{actionableOnly: true})
	if err != nil {
		t.Fatalf("buildDashboard(actionableOnly): %v", err)
	}

	// Sanity-check that full mode populated every section actionableOnly skips.
	if !fullResp.HasAgentActivity {
		t.Fatal("expected full mode HasAgentActivity=true")
	}
	if fullResp.Summary.TotalItems < completedCount+1 {
		t.Fatalf("expected full mode Summary.TotalItems >= %d, got %d", completedCount+1, fullResp.Summary.TotalItems)
	}
	if len(fullResp.ActiveItems) == 0 {
		t.Fatal("expected full mode ActiveItems non-empty")
	}
	if len(fullResp.RecentActivity) == 0 {
		t.Fatal("expected full mode RecentActivity non-empty")
	}
	if len(fullResp.ByRole) == 0 {
		t.Fatal("expected full mode ByRole non-empty")
	}
	if len(fullResp.StarredItems) == 0 {
		t.Fatal("expected full mode StarredItems non-empty")
	}

	// Assert actionable mode skipped every unused section.
	if actResp.HasAgentActivity {
		t.Error("actionableOnly should skip HasAgentActivity (expected false)")
	}
	if actResp.NeedsOnboarding {
		t.Error("actionableOnly should skip NeedsOnboarding (expected false)")
	}
	if actResp.Summary.TotalItems != 0 || len(actResp.Summary.ByCollection) != 0 {
		t.Errorf("actionableOnly should skip Summary, got %+v", actResp.Summary)
	}
	if actResp.OnboardingSeed != nil {
		t.Errorf("actionableOnly should skip OnboardingSeed, got %+v", actResp.OnboardingSeed)
	}
	if len(actResp.ActiveItems) != 0 {
		t.Errorf("actionableOnly should skip ActiveItems, got %d", len(actResp.ActiveItems))
	}
	if len(actResp.RecentActivity) != 0 {
		t.Errorf("actionableOnly should skip RecentActivity, got %d", len(actResp.RecentActivity))
	}
	if len(actResp.ByRole) != 0 {
		t.Errorf("actionableOnly should skip ByRole, got %d", len(actResp.ByRole))
	}
	if len(actResp.StarredItems) != 0 {
		t.Errorf("actionableOnly should skip StarredItems, got %d", len(actResp.StarredItems))
	}

	// Assert Attention, SuggestedNext, Timezone, TimezoneNote still match full mode.
	if !reflect.DeepEqual(actResp.Attention, fullResp.Attention) {
		t.Errorf("Attention mismatch:\nactionable=%+v\nfull=%+v", actResp.Attention, fullResp.Attention)
	}
	if !reflect.DeepEqual(actResp.SuggestedNext, fullResp.SuggestedNext) {
		t.Errorf("SuggestedNext mismatch:\nactionable=%+v\nfull=%+v", actResp.SuggestedNext, fullResp.SuggestedNext)
	}
	if actResp.Timezone != fullResp.Timezone || actResp.TimezoneNote != fullResp.TimezoneNote {
		t.Errorf("Timezone mismatch: actionable=(%q,%q) full=(%q,%q)",
			actResp.Timezone, actResp.TimezoneNote, fullResp.Timezone, fullResp.TimezoneNote)
	}
}
