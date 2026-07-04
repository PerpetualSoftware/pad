package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ── next ─────────────────────────────────────────────────────────────────────

// TestProjectNextEndpoint_MatchesDashboardSuggestedNext pins the "same data
// the CLI computes" acceptance criterion: /next must return exactly the
// dashboard's suggested_next array (that's all `pad project next` ever
// showed, post BUG-987 bug 6).
func TestProjectNextEndpoint_MatchesDashboardSuggestedNext(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Fix the thing",
		"fields": `{"status":"open","priority":"high"}`,
	})

	dash := getDashboard(t, srv, slug)
	if len(dash.SuggestedNext) == 0 {
		t.Fatal("expected dashboard to suggest the high-priority open task")
	}

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/next", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("next: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var next []DashboardSuggestion
	parseJSON(t, rr, &next)

	if len(next) != len(dash.SuggestedNext) {
		t.Fatalf("expected /next to mirror dashboard.suggested_next (%d entries), got %d",
			len(dash.SuggestedNext), len(next))
	}
	for i := range next {
		if next[i] != dash.SuggestedNext[i] {
			t.Fatalf("suggestion %d mismatch: /next=%+v dashboard=%+v", i, next[i], dash.SuggestedNext[i])
		}
	}
}

// TestProjectNextEndpoint_EmptyWorkspaceReturnsEmptyArrayNotNull pins the
// bare-array response shape (matches the CLI's `PrintJSON(dash.SuggestedNext)`
// and the handleListItems bare-array convention) — never a null/undefined
// body a client would have to special-case.
func TestProjectNextEndpoint_EmptyWorkspaceReturnsEmptyArrayNotNull(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/next", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("next: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if body := strings.TrimSpace(rr.Body.String()); body != "[]" {
		t.Fatalf("expected bare empty array '[]', got %q", body)
	}
}

func TestProjectNextEndpoint_RequiresAuth(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/next", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session once users exist, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── standup ──────────────────────────────────────────────────────────────────

// backdateItemUpdatedAt directly rewrites an item's updated_at column so
// cutoff-window tests don't depend on wall-clock sleeps. Mirrors the
// backdate helpers already used in oplog_gc_test.go / orphan_gc_test.go /
// internal/store/reports_test.go's backdateItem.
func backdateItemUpdatedAt(t *testing.T, srv *Server, itemID string, daysAgo int) {
	t.Helper()
	ts := time.Now().AddDate(0, 0, -daysAgo).UTC().Format(time.RFC3339)
	if _, err := srv.store.DB().Exec(`UPDATE items SET updated_at = ? WHERE id = ?`, ts, itemID); err != nil {
		t.Fatalf("backdate item %s: %v", itemID, err)
	}
}

func TestProjectStandupEndpoint_HappyPath(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	done := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Shipped it",
		"fields": `{"status":"done"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Still working",
		"fields": `{"status":"in-progress","priority":"medium"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp StandupResponse
	parseJSON(t, rr, &resp)

	if resp.Days != 1 {
		t.Errorf("expected default days=1, got %d", resp.Days)
	}
	if resp.Date != time.Now().Format("2006-01-02") {
		t.Errorf("expected date=%s, got %s", time.Now().Format("2006-01-02"), resp.Date)
	}
	if len(resp.Completed) != 1 || resp.Completed[0].Ref != done.Ref {
		t.Fatalf("expected 1 completed item (%s), got %+v", done.Ref, resp.Completed)
	}
	if resp.Completed[0].Status != "done" {
		t.Errorf("expected completed status=done, got %q", resp.Completed[0].Status)
	}
	if len(resp.InProgress) != 1 || resp.InProgress[0].Priority != "medium" {
		t.Fatalf("expected 1 in-progress item with priority=medium, got %+v", resp.InProgress)
	}
}

// TestProjectStandupEndpoint_DaysParam_FiltersOldCompletions pins the days
// cutoff and the "malformed days silently falls back to the default"
// semantics — this codebase's REST convention for lenient numeric query
// params (handleGetWorkspaceGraph's `depth`, GetReport's `window`). The MCP
// HTTP transport forwards `days` as-is and relies on this same gate
// (TASK-1916) rather than replicating it.
func TestProjectStandupEndpoint_DaysParam_FiltersOldCompletions(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	old := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Shipped a while ago",
		"fields": `{"status":"done"}`,
	})
	backdateItemUpdatedAt(t, srv, old.ID, 5)

	// Default days=1: the 5-day-old completion must be excluded.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup", nil)
	var resp StandupResponse
	parseJSON(t, rr, &resp)
	if len(resp.Completed) != 0 {
		t.Fatalf("expected 0 completed within default 1-day window, got %+v", resp.Completed)
	}

	// days=7: now in range.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup?days=7", nil)
	parseJSON(t, rr, &resp)
	if resp.Days != 7 || len(resp.Completed) != 1 {
		t.Fatalf("expected days=7 with 1 completed, got days=%d completed=%+v", resp.Days, resp.Completed)
	}

	// days=abc (malformed) and days=-3 (non-positive) both silently fall
	// back to the default (1), NOT a 400.
	for _, malformed := range []string{"abc", "-3", "0"} {
		rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup?days="+malformed, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("days=%s: expected 200 (lenient fallback, not a 400), got %d: %s", malformed, rr.Code, rr.Body.String())
		}
		parseJSON(t, rr, &resp)
		if resp.Days != 1 {
			t.Fatalf("days=%s: expected fallback to default 1, got %d", malformed, resp.Days)
		}
	}
}

func TestProjectStandupEndpoint_RequiresAuth(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/standup", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session once users exist, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── changelog ────────────────────────────────────────────────────────────────

func TestProjectChangelogEndpoint_HappyPath(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	task := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Shipped it",
		"fields": `{"status":"done"}`,
	})
	idea := createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Landed idea",
		"fields": `{"status":"implemented"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Still open",
		"fields": `{"status":"open"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("changelog: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ChangelogResponse
	parseJSON(t, rr, &resp)

	if resp.Total != 2 {
		t.Fatalf("expected 2 completed items, got %d (%+v)", resp.Total, resp)
	}
	if resp.Period != "last 7 days" {
		t.Errorf("expected default period 'last 7 days', got %q", resp.Period)
	}
	byCollection := map[string]ChangelogGroup{}
	for _, g := range resp.Groups {
		byCollection[g.Collection] = g
	}
	tasksGroup, ok := byCollection["Tasks"]
	if !ok || tasksGroup.Count != 1 || tasksGroup.Items[0].Ref != task.Ref {
		t.Fatalf("expected a 'Tasks' group with 1 item (%s), got %+v", task.Ref, byCollection)
	}
	ideasGroup, ok := byCollection["Ideas"]
	if !ok || ideasGroup.Count != 1 || ideasGroup.Items[0].Ref != idea.Ref {
		t.Fatalf("expected an 'Ideas' group with 1 item (%s), got %+v", idea.Ref, byCollection)
	}
}

// TestProjectChangelogEndpoint_SinceOverridesDays pins the CLI's silent
// since-wins behavior (cmd/pad/main.go changelogCmd's if/else): both since
// and days given → since wins, no 400.
func TestProjectChangelogEndpoint_SinceOverridesDays(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Shipped it",
		"fields": `{"status":"done"}`,
	})

	since := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog?days=9999&since="+since, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("changelog: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ChangelogResponse
	parseJSON(t, rr, &resp)
	if resp.Period != "since "+since {
		t.Fatalf("expected since to override days in the period label, got %q", resp.Period)
	}
	if resp.Since != since {
		t.Fatalf("expected since=%s, got %s", since, resp.Since)
	}
}

func TestProjectChangelogEndpoint_InvalidSinceReturns400(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog?since=not-a-date", nil)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for malformed since, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestProjectChangelogEndpoint_ParentFilter pins the parent-ref filter via
// itemMatchesParentFilter: case-insensitive match against the item's parent
// ref.
func TestProjectChangelogEndpoint_ParentFilter(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	plan := createItem(t, srv, slug, "plans", map[string]interface{}{
		"title":  "Q1 Launch",
		"fields": `{"status":"active"}`,
	})
	child := createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Scoped to plan",
		"fields": `{"status":"done","parent":"` + plan.Ref + `"}`,
	})
	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Unrelated",
		"fields": `{"status":"done"}`,
	})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog?parent="+plan.Ref, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("changelog: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp ChangelogResponse
	parseJSON(t, rr, &resp)
	if resp.Total != 1 {
		t.Fatalf("expected 1 item scoped to parent %s, got %d (%+v)", plan.Ref, resp.Total, resp)
	}
	if len(resp.Groups) != 1 || len(resp.Groups[0].Items) != 1 || resp.Groups[0].Items[0].Ref != child.Ref {
		t.Fatalf("expected only the child scoped to %s, got %+v", plan.Ref, resp.Groups)
	}
	if !strings.Contains(resp.Period, "parent: "+plan.Ref) {
		t.Errorf("expected period label to note the parent scope, got %q", resp.Period)
	}
}

func TestProjectChangelogEndpoint_RequiresAuth(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without a session once users exist, got %d: %s", rr.Code, rr.Body.String())
	}
}

// ── ACL scoping ──────────────────────────────────────────────────────────────

// TestProjectIntelEndpoints_ScopeToVisibleCollections pins collection
// visibility on all three new endpoints in one pass: a member restricted to
// the "ideas" collection must never see "tasks" data through /standup or
// /changelog, mirroring the restricted-member pattern already used for
// child-progress (handlers_items_test.go) and the report endpoint.
func TestProjectIntelEndpoints_ScopeToVisibleCollections(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug: %v", err)
	}

	createItem(t, srv, slug, "tasks", map[string]interface{}{
		"title":  "Hidden completion",
		"fields": `{"status":"done"}`,
	})
	visibleItem := createItem(t, srv, slug, "ideas", map[string]interface{}{
		"title":  "Visible completion",
		"fields": `{"status":"implemented"}`,
	})

	restrictedUser, err := srv.store.CreateUser(models.UserCreate{
		Email: "restricted@example.com", Name: "Restricted", Username: "restricted-user",
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser restricted: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, restrictedUser.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	ideasColl, err := srv.store.GetCollectionBySlug(ws.ID, "ideas")
	if err != nil || ideasColl == nil {
		t.Fatalf("GetCollectionBySlug ideas: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, restrictedUser.ID, "specific", []string{ideasColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}
	token, err := srv.store.CreateSession(restrictedUser.ID, "go-test", "192.0.2.1", "", 24*time.Hour)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr := doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/standup", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var standup StandupResponse
	parseJSON(t, rr, &standup)
	if len(standup.Completed) != 1 || standup.Completed[0].Ref != visibleItem.Ref {
		t.Fatalf("restricted standup should see only the visible completion (%s), got %+v",
			visibleItem.Ref, standup.Completed)
	}

	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+slug+"/changelog", nil, token)
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted changelog: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var changelog ChangelogResponse
	parseJSON(t, rr, &changelog)
	if changelog.Total != 1 {
		t.Fatalf("restricted changelog should see only 1 item, got %d (%+v)", changelog.Total, changelog)
	}
	for _, g := range changelog.Groups {
		if g.Collection == "Tasks" {
			t.Fatalf("restricted changelog leaked the hidden 'tasks' collection: %+v", changelog.Groups)
		}
	}
}

// TestProjectIntelEndpoints_BearerAdminRestrictedMemberIsScoped pins the
// codex round-1 finding: projectIntelVisibility must be bearer-aware
// (mirrors reportVisibleCollections / the BUG-1616/1617 pattern —
// handlers_admin_bearer_gate_test.go). A platform admin who is only a
// RESTRICTED member of this workspace gets the full unrestricted view over
// a cookie session (the web UI admin affordance) but must be scoped to
// their actual grant over a bearer token (PAT/CLI/OAuth) — never leaking
// the hidden collection's completions through standup or changelog.
func TestProjectIntelEndpoints_BearerAdminRestrictedMemberIsScoped(t *testing.T) {
	srv := testServer(t)

	admin, err := srv.store.CreateUser(models.UserCreate{
		Email: "admin@example.com", Name: "Admin", Password: "correct-horse-battery-staple", Role: "admin",
	})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "BearerScoped", OwnerID: admin.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, admin.ID, "editor"); err != nil {
		t.Fatalf("add member: %v", err)
	}

	schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"default":"open"}]}`
	visible, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Visible", Slug: "visible", Prefix: "VIS", Schema: schema,
	})
	if err != nil {
		t.Fatalf("create visible: %v", err)
	}
	hidden, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Hidden", Slug: "hidden", Prefix: "HID", Schema: schema,
	})
	if err != nil {
		t.Fatalf("create hidden: %v", err)
	}
	visibleItem, err := srv.store.CreateItem(ws.ID, visible.ID, models.ItemCreate{
		Title: "Visible completion", Fields: `{"status":"done"}`,
	})
	if err != nil {
		t.Fatalf("create visible item: %v", err)
	}
	if _, err := srv.store.CreateItem(ws.ID, hidden.ID, models.ItemCreate{
		Title: "Hidden completion", Fields: `{"status":"done"}`,
	}); err != nil {
		t.Fatalf("create hidden item: %v", err)
	}
	// An overdue task in the hidden collection surfaces in
	// dashboard.attention (type "overdue") and, via buildDashboardResponse,
	// in standup.blockers — the exact section the file-header "Known
	// asymmetry" comment called out as staying ungated until BUG-1917.
	if _, err := srv.store.CreateItem(ws.ID, hidden.ID, models.ItemCreate{
		Title: "Hidden overdue blocker", Fields: `{"status":"open","due_date":"2020-01-01"}`,
	}); err != nil {
		t.Fatalf("create hidden overdue item: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, admin.ID, "specific", []string{visible.ID}); err != nil {
		t.Fatalf("set access: %v", err)
	}
	visibleItem.ComputeRef()

	tok, err := srv.store.CreateAPIToken(admin.ID, models.APITokenCreate{
		Name: "admin-pat", WorkspaceID: ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Bearer admin → scoped: changelog sees only the visible completion.
	rr := doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/changelog", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer changelog: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var changelog ChangelogResponse
	parseJSON(t, rr, &changelog)
	if changelog.Total != 1 {
		t.Fatalf("bearer admin changelog should be scoped to 1 item, got %d (%+v)", changelog.Total, changelog)
	}
	for _, g := range changelog.Groups {
		if g.Collection == "Hidden" {
			t.Fatalf("bearer admin changelog leaked the hidden collection: %+v", changelog.Groups)
		}
	}

	// Bearer admin → standup's completed/in_progress are scoped too.
	rr = doRequestWithBearer(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/standup", tok.Token, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var standup StandupResponse
	parseJSON(t, rr, &standup)
	if len(standup.Completed) != 1 || standup.Completed[0].Ref != visibleItem.Ref {
		t.Fatalf("bearer admin standup.completed should be scoped to the visible item (%s), got %+v",
			visibleItem.Ref, standup.Completed)
	}
	// BUG-1917: standup's dashboard-derived blockers section must now be
	// scoped too — this is the asymmetry the file-header comment on
	// handlers_project_intel.go documented until this fix.
	for _, b := range standup.Blockers {
		if b.Title == "Hidden overdue blocker" {
			t.Fatalf("bearer admin standup.blockers leaked the hidden collection's overdue item: %+v", standup.Blockers)
		}
	}

	// Cookie admin → unrestricted (the pre-existing web UI admin affordance):
	// changelog sees both completions, and standup.blockers includes the
	// hidden collection's overdue item too (checked below).
	sessTok, err := srv.store.CreateSession(admin.ID, "web-test", "192.0.2.1", "", webSessionTTL)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/changelog", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("cookie changelog: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &changelog)
	if changelog.Total != 2 {
		t.Fatalf("cookie admin changelog should be unrestricted (2 items), got %d (%+v)", changelog.Total, changelog)
	}

	rr = doRequestWithCookie(srv, "GET", "/api/v1/workspaces/"+ws.Slug+"/standup", nil, sessTok)
	if rr.Code != http.StatusOK {
		t.Fatalf("cookie standup: expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	parseJSON(t, rr, &standup)
	foundHiddenBlocker := false
	for _, b := range standup.Blockers {
		if b.Title == "Hidden overdue blocker" {
			foundHiddenBlocker = true
		}
	}
	if !foundHiddenBlocker {
		t.Fatalf("cookie admin standup.blockers should be unrestricted (see hidden overdue item); got %+v", standup.Blockers)
	}
}
