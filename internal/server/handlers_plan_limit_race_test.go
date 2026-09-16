package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2808: a plan limit is a count taken before the write. When a competing
// row lands between the count and the handler's own insert, the cap is
// exceeded. planLimitAdmittedHook runs right after a check admits a request,
// and the "competing" legs insert a second row there, as a concurrent request
// would. Each such leg is paired with a control that inserts nothing
// (CONVE-12): without it, a fix that refused everything would pass.
//
// These drive the real router in cloud mode (limits are no-ops otherwise).
// They run on SQLite because testServer is SQLite-backed.

// planLimitEnv is a cloud-mode server with one PAT-authenticated user on the
// free plan, who owns one workspace seeded with the startup collections.
type planLimitEnv struct {
	*consentEnv
	home *models.Workspace
}

func newPlanLimitEnv(t *testing.T) *planLimitEnv {
	t.Helper()
	e := newConsentEnv(t, true)
	e.srv.cloudMode = true
	if err := e.srv.store.SetUserPlan(e.user.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	all, err := e.srv.store.ListWorkspaces()
	if err != nil || len(all) != 1 {
		t.Fatalf("ListWorkspaces = %d, %v; want exactly the fixture's home", len(all), err)
	}
	home := all[0]
	if err := e.srv.store.SeedCollectionsFromTemplate(home.ID, "startup"); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	return &planLimitEnv{consentEnv: e, home: &home}
}

// capAtOneMore sets the user's override for feature to current+1, so exactly
// one more row is admitted.
func (e *planLimitEnv) capAtOneMore(t *testing.T, feature string, current int) int {
	t.Helper()
	limit := current + 1
	if err := e.srv.store.SetUserPlanOverrides(e.user.ID, fmt.Sprintf(`{%q:%d}`, feature, limit)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	return limit
}

func (e *planLimitEnv) ownedWorkspaces(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(e.srv.store.D().Rebind(
		`SELECT COUNT(*) FROM workspaces WHERE owner_id = ?`), e.user.ID).Scan(&n); err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	return n
}

func (e *planLimitEnv) liveItems(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.srv.store.DB().QueryRow(e.srv.store.D().Rebind(
		`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), e.home.ID).Scan(&n); err != nil {
		t.Fatalf("count items: %v", err)
	}
	return n
}

func (e *planLimitEnv) tasksCollectionID(t *testing.T) string {
	t.Helper()
	colls, err := e.srv.store.ListCollections(e.home.ID)
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	for _, c := range colls {
		if c.Slug == "tasks" {
			return c.ID
		}
	}
	t.Fatal("no tasks collection in the seeded workspace")
	return ""
}

func (e *planLimitEnv) createItem(title string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"title": title})
	return e.do("POST", "/api/v1/workspaces/"+e.home.Slug+"/collections/tasks/items", "application/json", body, "")
}

// --- U1: workspaces (create door) ---

func TestPlanLimitRace_Workspaces_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	limit := e.capAtOneMore(t, "workspaces", e.ownedWorkspaces(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "workspaces" || scope != e.user.ID {
			return
		}
		ran = true
		if _, err := e.srv.store.CreateWorkspace(models.WorkspaceCreate{
			Name: "Competitor", OwnerID: e.user.ID,
		}); err != nil {
			t.Errorf("competing CreateWorkspace: %v", err)
		}
	}

	rr := e.createWorkspace("Racer", "")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for workspaces; the leg measured nothing")
	}
	if got := e.ownedWorkspaces(t); got > limit {
		t.Errorf("user owns %d workspaces under a cap of %d (create answered %d) — the "+
			"competing create landed in the window and both were admitted", got, limit, rr.Code)
	}
}

func TestPlanLimitRace_Workspaces_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	limit := e.capAtOneMore(t, "workspaces", e.ownedWorkspaces(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, _ string) {
		if feature == "workspaces" {
			ran = true
		}
	}

	rr := e.createWorkspace("Alone", "")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for workspaces; the leg measured nothing")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.ownedWorkspaces(t); got != limit {
		t.Errorf("user owns %d workspaces, want exactly the cap %d", got, limit)
	}
}

// --- W1: items_per_workspace (item create door) ---

func TestPlanLimitRace_Items_CompetingCreateInWindow_Refused(t *testing.T) {
	e := newPlanLimitEnv(t)
	collID := e.tasksCollectionID(t)
	limit := e.capAtOneMore(t, "items_per_workspace", e.liveItems(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, scope string) {
		if feature != "items_per_workspace" || scope != e.home.ID {
			return
		}
		ran = true
		if _, err := e.srv.store.CreateItem(e.home.ID, collID, models.ItemCreate{Title: "Competitor"}); err != nil {
			t.Errorf("competing CreateItem: %v", err)
		}
	}

	rr := e.createItem("Racer")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for items; the leg measured nothing")
	}
	if got := e.liveItems(t); got > limit {
		t.Errorf("workspace holds %d live items under a cap of %d (create answered %d) — the "+
			"competing create landed in the window and both were admitted", got, limit, rr.Code)
	}
}

func TestPlanLimitRace_Items_NoCompetitor_Admitted(t *testing.T) {
	e := newPlanLimitEnv(t)
	limit := e.capAtOneMore(t, "items_per_workspace", e.liveItems(t))

	ran := false
	e.srv.planLimitAdmittedHook = func(feature, _ string) {
		if feature == "items_per_workspace" {
			ran = true
		}
	}

	rr := e.createItem("Alone")
	if !ran {
		t.Fatal("planLimitAdmittedHook never ran for items; the leg measured nothing")
	}
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := e.liveItems(t); got != limit {
		t.Errorf("workspace holds %d live items, want exactly the cap %d", got, limit)
	}
}
