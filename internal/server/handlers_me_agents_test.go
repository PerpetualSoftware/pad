package server

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type meAgentsTestResponse struct {
	Agents []struct {
		Slug     string                     `json:"slug"`
		Name     string                     `json:"name"`
		Icon     string                     `json:"icon"`
		Lanes    []models.RoleQueueResponse `json:"lanes"`
		Finished []struct {
			Workspace   string `json:"workspace"`
			Ref         string `json:"ref"`
			Title       string `json:"title"`
			Value       string `json:"value"`
			CompletedAt string `json:"completed_at"`
		} `json:"finished"`
		Summary struct {
			NowCount   int    `json:"now_count"`
			QueueTotal int    `json:"queue_total"`
			Parked     string `json:"parked"`
		} `json:"summary"`
	} `json:"agents"`
	Degraded []string `json:"degraded"`
}

type countingQueryer struct {
	inner store.Queryer
	count atomic.Int64
}

func (c *countingQueryer) Query(query string, args ...any) (*sql.Rows, error) {
	c.count.Add(1)
	return c.inner.Query(query, args...)
}

func (c *countingQueryer) QueryRow(query string, args ...any) *sql.Row {
	c.count.Add(1)
	return c.inner.QueryRow(query, args...)
}

func mkAgentsUser(t *testing.T, srv *Server, email, name string) *models.User {
	t.Helper()
	u, err := srv.store.CreateUser(models.UserCreate{
		Email:    email,
		Name:     name,
		Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", email, err)
	}
	return u
}

func mkAgentsPAT(t *testing.T, srv *Server, userID, wsID string) string {
	t.Helper()
	tok, err := srv.store.CreateAPIToken(userID, models.APITokenCreate{
		Name:        "agents-pat",
		WorkspaceID: wsID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	return tok.Token
}

const meAgentsStdSchema = `{"fields":[{"key":"status","type":"select","options":["open","in_progress","done","cancelled"],"terminal_options":["done","cancelled"],"default":"open"},{"key":"priority","type":"select","options":["high","medium","low"],"default":"medium"}]}`

func TestMeAgents_MergesSlugAcrossWorkspaces(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "merge-owner@example.com", "Merge Owner")
	tok := mkAgentsPAT(t, srv, owner.ID, "")

	wsAlpha, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Alpha WS", Slug: "alpha-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create alpha: %v", err)
	}
	wsBeta, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Beta WS", Slug: "beta-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("create beta: %v", err)
	}
	for _, ws := range []*models.Workspace{wsAlpha, wsBeta} {
		if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("add member: %v", err)
		}
	}

	collA, err := srv.store.CreateCollection(wsAlpha.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "ALP", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("collA: %v", err)
	}
	collB, err := srv.store.CreateCollection(wsBeta.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "BET", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("collB: %v", err)
	}

	// Same slug "implementer" in both workspaces; wsBeta has a Now item so wsBeta lane must come FIRST,
	// giving the merged agent wsBeta's name/icon ("Beta Implementer" / "B").
	roleAImp, err := srv.store.CreateAgentRole(wsAlpha.ID, models.AgentRoleCreate{Name: "Alpha Implementer", Slug: "implementer", Icon: "A"})
	if err != nil {
		t.Fatalf("roleAImp: %v", err)
	}
	roleBImp, err := srv.store.CreateAgentRole(wsBeta.ID, models.AgentRoleCreate{Name: "Beta Implementer", Slug: "implementer", Icon: "B"})
	if err != nil {
		t.Fatalf("roleBImp: %v", err)
	}
	// Distinct slugs to verify agent ordering: minimum sort_order asc, then name asc, then slug asc.
	rolePlanner, err := srv.store.CreateAgentRole(wsAlpha.ID, models.AgentRoleCreate{Name: "Planner", Slug: "planner", Icon: "P"})
	if err != nil {
		t.Fatalf("rolePlanner: %v", err)
	}
	roleZeta, err := srv.store.CreateAgentRole(wsAlpha.ID, models.AgentRoleCreate{Name: "Alpha Name", Slug: "zeta-slug", Icon: "Z"})
	if err != nil {
		t.Fatalf("roleZeta: %v", err)
	}
	roleBetaSlug, err := srv.store.CreateAgentRole(wsAlpha.ID, models.AgentRoleCreate{Name: "Alpha Name", Slug: "beta-slug", Icon: "Q"})
	if err != nil {
		t.Fatalf("roleBetaSlug: %v", err)
	}
	// Empty role with no items -> must be omitted.
	if _, err := srv.store.CreateAgentRole(wsAlpha.ID, models.AgentRoleCreate{Name: "Idle Role", Slug: "idle-role"}); err != nil {
		t.Fatalf("idle role: %v", err)
	}

	// Sort orders:
	// planner: 1
	// implementer: min(10, 2) = 2
	// beta-slug and zeta-slug: both sort_order=5 and name="Alpha Name" -> beta-slug before zeta-slug
	if err := srv.store.UpdateAgentRoleOrder(wsAlpha.ID, []store.RoleOrderUpdate{
		{RoleID: rolePlanner.ID, SortOrder: 1},
		{RoleID: roleAImp.ID, SortOrder: 10},
		{RoleID: roleZeta.ID, SortOrder: 5},
		{RoleID: roleBetaSlug.ID, SortOrder: 5},
	}); err != nil {
		t.Fatalf("order alpha roles: %v", err)
	}
	if err := srv.store.UpdateAgentRoleOrder(wsBeta.ID, []store.RoleOrderUpdate{
		{RoleID: roleBImp.ID, SortOrder: 2},
	}); err != nil {
		t.Fatalf("order beta roles: %v", err)
	}

	// Populate items:
	// wsAlpha implementer: 2 queued items (no Now)
	if _, err := srv.store.CreateItem(wsAlpha.ID, collA.ID, models.ItemCreate{Title: "A Queued 1", Fields: `{"status":"open"}`, AgentRoleID: &roleAImp.ID}); err != nil {
		t.Fatalf("item: %v", err)
	}
	if _, err := srv.store.CreateItem(wsAlpha.ID, collA.ID, models.ItemCreate{Title: "A Queued 2", Fields: `{"status":"open"}`, AgentRoleID: &roleAImp.ID}); err != nil {
		t.Fatalf("item: %v", err)
	}
	// wsBeta implementer: 1 Now item + 1 queued item
	if _, err := srv.store.CreateItem(wsBeta.ID, collB.ID, models.ItemCreate{Title: "B Active", Fields: `{"status":"in_progress"}`, AgentRoleID: &roleBImp.ID}); err != nil {
		t.Fatalf("item: %v", err)
	}
	if _, err := srv.store.CreateItem(wsBeta.ID, collB.ID, models.ItemCreate{Title: "B Queued", Fields: `{"status":"open"}`, AgentRoleID: &roleBImp.ID}); err != nil {
		t.Fatalf("item: %v", err)
	}
	// Other roles: 1 open item each
	for _, rid := range []string{rolePlanner.ID, roleZeta.ID, roleBetaSlug.ID} {
		rCopy := rid
		if _, err := srv.store.CreateItem(wsAlpha.ID, collA.ID, models.ItemCreate{Title: "Work " + rid, Fields: `{"status":"open"}`, AgentRoleID: &rCopy}); err != nil {
			t.Fatalf("item: %v", err)
		}
	}

	// Park wsAlpha implementer at top, leave wsBeta implementer unparked -> summary.parked == "some".
	if _, err := srv.store.SetRolePark(wsAlpha.ID, roleAImp.ID, store.ParkAtTop(), nil, owner.ID); err != nil {
		t.Fatalf("park alpha imp: %v", err)
	}

	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents: got %d %s", rr.Code, rr.Body.String())
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "private, no-store" {
		t.Errorf("Cache-Control = %q, want %q", cc, "private, no-store")
	}

	var got meAgentsTestResponse
	parseJSON(t, rr, &got)

	if len(got.Degraded) != 0 {
		t.Errorf("degraded = %v, want empty", got.Degraded)
	}

	wantOrder := []string{"planner", "implementer", "beta-slug", "zeta-slug"}
	if len(got.Agents) != len(wantOrder) {
		t.Fatalf("got %d agents, want %d: %+v", len(got.Agents), len(wantOrder), got.Agents)
	}
	for i, slug := range wantOrder {
		if got.Agents[i].Slug != slug {
			t.Errorf("agents[%d].slug = %q, want %q", i, got.Agents[i].Slug, slug)
		}
	}

	imp := got.Agents[1]
	if imp.Name != "Beta Implementer" || imp.Icon != "B" {
		t.Errorf("merged agent name/icon = %q/%q, want Beta Implementer/B", imp.Name, imp.Icon)
	}
	if len(imp.Lanes) != 2 {
		t.Fatalf("implementer lanes = %d, want 2", len(imp.Lanes))
	}
	if imp.Lanes[0].Workspace != wsBeta.Slug || imp.Lanes[1].Workspace != wsAlpha.Slug {
		t.Errorf("lane order = [%s, %s], want [%s, %s]", imp.Lanes[0].Workspace, imp.Lanes[1].Workspace, wsBeta.Slug, wsAlpha.Slug)
	}
	if imp.Lanes[1].Park == nil || imp.Lanes[1].Park.SetBy == nil || *imp.Lanes[1].Park.SetBy != owner.Name {
		t.Errorf("alpha lane park.set_by = %+v, want %q", imp.Lanes[1].Park, owner.Name)
	}
	if imp.Summary.NowCount != 1 || imp.Summary.QueueTotal != 3 || imp.Summary.Parked != "some" {
		t.Errorf("implementer summary = %+v, want now=1 queue=3 parked=some", imp.Summary)
	}

	// Park wsBeta implementer at top too -> summary.parked becomes "all".
	if _, err := srv.store.SetRolePark(wsBeta.ID, roleBImp.ID, store.ParkAtTop(), nil, owner.ID); err != nil {
		t.Fatalf("park beta imp: %v", err)
	}
	rr = doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents after park all: %d %s", rr.Code, rr.Body.String())
	}
	var gotAll meAgentsTestResponse
	parseJSON(t, rr, &gotAll)
	if gotAll.Agents[1].Summary.Parked != "all" {
		t.Errorf("implementer summary.parked = %q, want all", gotAll.Agents[1].Summary.Parked)
	}
}

func TestMeAgents_RestrictedMemberVisibleOnly(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	// Add a hidden finished item and a visible finished item.
	visDone, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Visible Shipped", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("visDone: %v", err)
	}
	doneFields := `{"status":"done"}`
	if _, err := f.srv.store.UpdateItem(visDone.ID, models.ItemUpdate{Fields: &doneFields}); err != nil {
		t.Fatalf("update visDone: %v", err)
	}
	visDone, _ = f.srv.store.GetItem(visDone.ID)

	hidDone, err := f.srv.store.CreateItem(f.ws.ID, f.hiddenColl.ID, models.ItemCreate{
		Title: "Secret Shipped Work", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("hidDone: %v", err)
	}
	if _, err := f.srv.store.UpdateItem(hidDone.ID, models.ItemUpdate{Fields: &doneFields}); err != nil {
		t.Fatalf("update hidDone: %v", err)
	}
	hidDone, _ = f.srv.store.GetItem(hidDone.ID)

	// Park before f.items[1] (sort order 2048): hiddenItem @ 512, items[0] @ 1024, items[1] @ 2048, items[2] @ 3072.
	if _, err := f.srv.store.SetRolePark(f.ws.ID, f.role.ID, store.ParkBefore(f.items[1].ID), nil, f.owner.ID); err != nil {
		t.Fatalf("SetRolePark: %v", err)
	}

	rr := doRequestWithHeaders(f.srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents restricted: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, secret := range []string{f.hiddenItem.Ref, f.hiddenItem.Title, hidDone.Ref, hidDone.Title} {
		if strings.Contains(body, secret) {
			t.Fatalf("LEAK: restricted response contained %q:\n%s", secret, body)
		}
	}

	var got meAgentsTestResponse
	parseJSON(t, rr, &got)
	if len(got.Agents) != 1 || len(got.Agents[0].Lanes) != 1 {
		t.Fatalf("got agents=%+v, want 1 agent with 1 lane", got.Agents)
	}
	lane := got.Agents[0].Lanes[0]
	if lane.QueueTotal != 3 || lane.ParkedTotal != 2 {
		t.Errorf("lane counts total=%d parked=%d, want 3/2", lane.QueueTotal, lane.ParkedTotal)
	}
	if lane.Park == nil || lane.Park.Index == nil || *lane.Park.Index != 1 {
		t.Errorf("lane park.index = %+v, want 1", lane.Park)
	}
	if lane.Park.FirstParkedRef == nil || *lane.Park.FirstParkedRef != f.items[1].Ref {
		t.Errorf("lane park.first_parked_ref = %v, want %q", lane.Park.FirstParkedRef, f.items[1].Ref)
	}
	if !lane.CanReorder || lane.CanPark {
		t.Errorf("restricted editor can_reorder=%v can_park=%v, want true/false", lane.CanReorder, lane.CanPark)
	}
	for _, q := range lane.Queue {
		if !q.CanEdit {
			t.Errorf("queue item %s in visibleColl should have can_edit=true for restricted editor", q.Ref)
		}
	}
	if len(got.Agents[0].Finished) != 1 || got.Agents[0].Finished[0].Ref != visDone.Ref {
		t.Errorf("finished = %+v, want only %s", got.Agents[0].Finished, visDone.Ref)
	}
	if got.Agents[0].Summary.QueueTotal != 3 || got.Agents[0].Summary.Parked != "some" {
		t.Errorf("summary = %+v, want queue=3 parked=some", got.Agents[0].Summary)
	}
}

func TestMeAgents_UnknownWorkspaceEmpty200Identical(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	alice := mkAgentsUser(t, srv, "empty-alice@example.com", "Alice")
	bob := mkAgentsUser(t, srv, "empty-bob@example.com", "Bob")
	aliceTok := mkAgentsPAT(t, srv, alice.ID, "")

	wsEmpty, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Empty WS", Slug: "empty-ws", OwnerID: alice.ID})
	if err != nil {
		t.Fatalf("wsEmpty: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsEmpty.ID, alice.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	wsOther, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Bobs WS", Slug: "bobs-ws", OwnerID: bob.ID})
	if err != nil {
		t.Fatalf("wsOther: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(wsOther.ID, bob.ID, "owner"); err != nil {
		t.Fatalf("member bob: %v", err)
	}
	if _, err := srv.store.CreateAgentRole(wsOther.ID, models.AgentRoleCreate{Name: "Secret Role", Slug: "secret-role"}); err != nil {
		t.Fatalf("secret role: %v", err)
	}

	rrKnownEmpty := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?workspace="+wsEmpty.Slug, nil, bearerHdr(aliceTok))
	rrUnknown := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?workspace=no-such-ws", nil, bearerHdr(aliceTok))
	rrNotMember := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?workspace="+wsOther.Slug, nil, bearerHdr(aliceTok))

	for name, rr := range map[string]*httptest.ResponseRecorder{
		"known_empty": rrKnownEmpty,
		"unknown":     rrUnknown,
		"not_member":  rrNotMember,
	} {
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: got %d %s, want 200", name, rr.Code, rr.Body.String())
		}
	}

	wantBody := `{"agents":[],"degraded":[]}` + "\n"
	if rrKnownEmpty.Body.String() != wantBody {
		t.Errorf("known_empty body = %q, want %q", rrKnownEmpty.Body.String(), wantBody)
	}
	if rrUnknown.Body.String() != rrKnownEmpty.Body.String() {
		t.Errorf("unknown body = %q != known_empty %q", rrUnknown.Body.String(), rrKnownEmpty.Body.String())
	}
	if rrNotMember.Body.String() != rrKnownEmpty.Body.String() {
		t.Errorf("not_member body = %q != known_empty %q", rrNotMember.Body.String(), rrKnownEmpty.Body.String())
	}
}

func TestMeAgents_PinnedTokenAndAppPrincipal(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "pin-owner@example.com", "Pin Owner")
	wsA, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "WS A", Slug: "ws-a", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("wsA: %v", err)
	}
	wsB, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "WS B", Slug: "ws-b", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("wsB: %v", err)
	}
	for _, ws := range []*models.Workspace{wsA, wsB} {
		if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("member: %v", err)
		}
		coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: meAgentsStdSchema})
		if err != nil {
			t.Fatalf("coll: %v", err)
		}
		role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Worker", Slug: "worker"})
		if err != nil {
			t.Fatalf("role: %v", err)
		}
		if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: "Task in " + ws.Slug, Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		}); err != nil {
			t.Fatalf("item: %v", err)
		}
	}

	// 1. Pinned token to workspace A -> admitted, sees ONLY wsA.
	pinTokA := mkAgentsPAT(t, srv, owner.ID, wsA.ID)
	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(pinTokA))
	if rr.Code != http.StatusOK {
		t.Fatalf("pinned token GET /me/agents: got %d %s", rr.Code, rr.Body.String())
	}
	var got meAgentsTestResponse
	parseJSON(t, rr, &got)
	if len(got.Agents) != 1 || len(got.Agents[0].Lanes) != 1 || got.Agents[0].Lanes[0].Workspace != wsA.Slug {
		t.Fatalf("pinned token saw %+v, want only wsA", got.Agents)
	}
	if strings.Contains(rr.Body.String(), wsB.Slug) {
		t.Fatalf("pinned token leaked wsB: %s", rr.Body.String())
	}

	// 2. No auth -> 401 unauthorized.
	rrNoAuth := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, nil)
	if rrNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth GET /me/agents: got %d %s, want 401", rrNoAuth.Code, rrNoAuth.Body.String())
	}

	// 3. App principal -> 404 byte-identical to /me/needs app-principal 404.
	appReqNeeds := httptest.NewRequest(http.MethodGet, "/api/v1/me/needs", nil)
	appCtx := context.WithValue(WithCurrentUser(appReqNeeds.Context(), owner), appCtxKey{}, &appContext{})
	rrNeedsApp := httptest.NewRecorder()
	srv.handleMeNeeds(rrNeedsApp, appReqNeeds.WithContext(appCtx))

	appReqAgents := httptest.NewRequest(http.MethodGet, "/api/v1/me/agents?finished=999", nil)
	rrAgentsApp := httptest.NewRecorder()
	srv.handleMeAgents(rrAgentsApp, appReqAgents.WithContext(appCtx))

	if rrAgentsApp.Code != http.StatusNotFound || rrNeedsApp.Code != http.StatusNotFound {
		t.Fatalf("app principal codes: agents=%d needs=%d, want 404/404", rrAgentsApp.Code, rrNeedsApp.Code)
	}
	if rrAgentsApp.Body.String() != rrNeedsApp.Body.String() {
		t.Fatalf("app principal 404 mismatch: agents=%q needs=%q", rrAgentsApp.Body.String(), rrNeedsApp.Body.String())
	}
}

func TestMeAgents_LiveOnlyOwnSessions(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	presence := NewMemorySessionPresence()
	srv.SetSessionPresence(presence)

	alice := mkAgentsUser(t, srv, "live-alice@example.com", "Alice")
	bob := mkAgentsUser(t, srv, "live-bob@example.com", "Bob")
	aliceTok := mkAgentsPAT(t, srv, alice.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Live WS", Slug: "live-ws", OwnerID: alice.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, alice.ID, "owner"); err != nil {
		t.Fatalf("alice member: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, bob.ID, "editor"); err != nil {
		t.Fatalf("bob member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "LIV", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Coder", Slug: "coder"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}

	wBusy := "wk_11111111111111111111111111111111"
	wIdle := "wk_22222222222222222222222222222222"
	wOff := "wk_33333333333333333333333333333333"
	wBob := "wk_44444444444444444444444444444444"

	mkLeased := func(title, holder, worker, uid string) string {
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: title, Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if _, err := srv.store.ClaimItemLease(it.ID, holder, worker, uid, 15*time.Minute); err != nil {
			t.Fatalf("claim %s: %v", title, err)
		}
		it, _ = srv.store.GetItem(it.ID)
		return it.Ref
	}

	refBusy := mkLeased("Alice Busy", "a-busy", wBusy, alice.ID)
	refIdle := mkLeased("Alice Idle", "a-idle", wIdle, alice.ID)
	refOff := mkLeased("Alice Offline", "a-off", wOff, alice.ID)
	refBob := mkLeased("Bob Busy", "b-busy", wBob, bob.ID)

	presence.Add(alice.ID, SessionIdentity{Agent: "a-busy", WorkerID: wBusy}, SessionOrigin{})
	presence.Add(alice.ID, SessionIdentity{Agent: "a-idle", WorkerID: wIdle}, SessionOrigin{})
	presence.Add(bob.ID, SessionIdentity{Agent: "b-busy", WorkerID: wBob}, SessionOrigin{})
	if err := presence.SetActivity(alice.ID, wBusy, true, time.Now().UTC(), nil); err != nil {
		t.Fatalf("set busy alice: %v", err)
	}
	if err := presence.SetActivity(bob.ID, wBob, true, time.Now().UTC(), nil); err != nil {
		t.Fatalf("set busy bob: %v", err)
	}

	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(aliceTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents: %d %s", rr.Code, rr.Body.String())
	}
	var got meAgentsTestResponse
	parseJSON(t, rr, &got)
	if len(got.Agents) != 1 || len(got.Agents[0].Lanes) != 1 {
		t.Fatalf("unexpected shape: %+v", got)
	}
	liveByRef := map[string]string{}
	for _, n := range got.Agents[0].Lanes[0].Now {
		liveByRef[n.Ref] = n.Live
	}
	want := map[string]string{
		refBusy: "busy",
		refIdle: "idle",
		refOff:  "offline",
		refBob:  "unknown",
	}
	for ref, wantLive := range want {
		if liveByRef[ref] != wantLive {
			t.Errorf("now[%s].live = %q, want %q", ref, liveByRef[ref], wantLive)
		}
	}
}

func TestMeAgents_FinishedExcludesCustomAbandonedAndReopened(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "fin-owner@example.com", "Fin Owner")
	tok := mkAgentsPAT(t, srv, owner.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Fin WS", Slug: "fin-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	// Custom schema declaring "overturned" as abandoned_options, "done" and "shipped" as positive terminals.
	customSchema := `{"fields":[{"key":"status","type":"select","options":["open","done","shipped","overturned"],"terminal_options":["done","shipped","overturned"],"abandoned_options":["overturned"],"default":"open"}]}`
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "FIN", Schema: customSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Builder", Slug: "builder"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	// Keep one open item so the role has a lane too.
	if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Queued Work", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	}); err != nil {
		t.Fatalf("queued: %v", err)
	}

	setTransitionTime := func(itemID string, at time.Time) {
		t.Helper()
		if _, err := srv.store.DB().Exec(`UPDATE status_transitions SET created_at = ? WHERE item_id = ?`, at.UTC().Format(time.RFC3339), itemID); err != nil {
			t.Fatalf("set transition time: %v", err)
		}
	}

	mkTransitioned := func(title, toStatus string, at time.Time) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: title, Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		f := fmt.Sprintf(`{"status":%q}`, toStatus)
		if _, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{Fields: &f}); err != nil {
			t.Fatalf("update %s -> %s: %v", title, toStatus, err)
		}
		setTransitionTime(it.ID, at)
		it, _ = srv.store.GetItem(it.ID)
		return it
	}

	now := time.Now().UTC().Truncate(time.Second)
	doneOld := mkTransitioned("Done Old", "done", now.Add(-3*time.Hour))
	doneMid := mkTransitioned("Shipped Mid", "shipped", now.Add(-2*time.Hour))
	doneNew := mkTransitioned("Done New", "done", now.Add(-1*time.Hour))
	// Too old (> 30 days) -> excluded
	_ = mkTransitioned("Done Ancient", "done", now.Add(-35*24*time.Hour))
	// Declared custom abandoned -> excluded
	_ = mkTransitioned("Overturned Work", "overturned", now.Add(-30*time.Minute))
	// Done then reopened -> excluded
	reopened := mkTransitioned("Reopened Work", "done", now.Add(-20*time.Minute))
	openFields := `{"status":"open"}`
	if _, err := srv.store.UpdateItem(reopened.ID, models.ItemUpdate{Fields: &openFields}); err != nil {
		t.Fatalf("reopen: %v", err)
	}

	// Request with finished=2 -> expect [doneNew, doneMid] newest first (capped at 2).
	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?finished=2", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents?finished=2: %d %s", rr.Code, rr.Body.String())
	}
	var got meAgentsTestResponse
	parseJSON(t, rr, &got)
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(got.Agents))
	}
	fin := got.Agents[0].Finished
	if len(fin) != 2 {
		t.Fatalf("finished len = %d, want 2: %+v", len(fin), fin)
	}
	if fin[0].Ref != doneNew.Ref || fin[0].Value != "done" || fin[0].Workspace != ws.Slug {
		t.Errorf("fin[0] = %+v, want %s/done/%s", fin[0], doneNew.Ref, ws.Slug)
	}
	if fin[1].Ref != doneMid.Ref || fin[1].Value != "shipped" {
		t.Errorf("fin[1] = %+v, want %s/shipped", fin[1], doneMid.Ref)
	}

	// Request with finished=5 -> expect all 3 completed items, never overturned/reopened/ancient.
	rr = doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?finished=5", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents?finished=5: %d %s", rr.Code, rr.Body.String())
	}
	var gotAll meAgentsTestResponse
	parseJSON(t, rr, &gotAll)
	finAll := gotAll.Agents[0].Finished
	if len(finAll) != 3 {
		t.Fatalf("finished len = %d, want 3: %+v", len(finAll), finAll)
	}
	if finAll[0].Ref != doneNew.Ref || finAll[1].Ref != doneMid.Ref || finAll[2].Ref != doneOld.Ref {
		t.Errorf("finished order = [%s, %s, %s], want [%s, %s, %s]",
			finAll[0].Ref, finAll[1].Ref, finAll[2].Ref, doneNew.Ref, doneMid.Ref, doneOld.Ref)
	}
}

func TestMeAgents_FinishedCompletedAtIsTheDoneTransition(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "done-ts-owner@example.com", "Done TS Owner")
	tok := mkAgentsPAT(t, srv, owner.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Done TS WS", Slug: "done-ts-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	schema := `{"fields":[{"key":"status","type":"select","options":["open","done","shipped","cancelled"],"terminal_options":["done","shipped","cancelled"],"abandoned_options":["cancelled"],"default":"open"},{"key":"stage","type":"select","options":["draft","reviewed"],"default":"draft"}]}`
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "DTS", Schema: schema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Builder", Slug: "builder"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	tItemADone := now.Add(-4 * time.Hour)
	tItemAOtherField := now.Add(-10 * time.Minute)
	tItemBDone := now.Add(-3 * time.Hour)
	tItemBShipped := now.Add(-2 * time.Hour)
	tItemCFirstDone := now.Add(-5 * time.Hour)
	tItemCReopen := now.Add(-3*time.Hour - 30*time.Minute)
	tItemCSecondDone := now.Add(-1 * time.Hour)
	tItemCOtherField := now.Add(-5 * time.Minute)

	insertTransition := func(id, itemID, fieldKey, fromStatus, toStatus string, at time.Time) {
		t.Helper()
		if _, err := srv.store.DB().Exec(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, itemID, ws.ID, coll.ID, fieldKey, fromStatus, toStatus, at.UTC().Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert transition %s: %v", id, err)
		}
	}

	// Item A: set done at -4h, then a later transition on another select field ("stage") at -10m.
	itemA, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Item A Other Field Later", Fields: `{"status":"done","stage":"reviewed"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("itemA: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, itemA.ID); err != nil {
		t.Fatalf("clear itemA transitions: %v", err)
	}
	insertTransition("a-done", itemA.ID, "status", "open", "done", tItemADone)
	insertTransition("a-stage", itemA.ID, "stage", "draft", "reviewed", tItemAOtherField)
	itemA, _ = srv.store.GetItem(itemA.ID)

	// Item B: done at -3h, then moved to another completed-work value ("shipped") at -2h.
	itemB, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Item B Done To Shipped", Fields: `{"status":"shipped","stage":"draft"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("itemB: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, itemB.ID); err != nil {
		t.Fatalf("clear itemB transitions: %v", err)
	}
	insertTransition("b-done", itemB.ID, "status", "open", "done", tItemBDone)
	insertTransition("b-shipped", itemB.ID, "status", "done", "shipped", tItemBShipped)
	itemB, _ = srv.store.GetItem(itemB.ID)

	// Item C: done (-5h) -> reopened (-3h30m) -> done (-1h) -> other select field (-5m).
	// Must report -1h (newest transition into current done value), NOT -5m and NOT -5h.
	itemC, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Item C Recompleted", Fields: `{"status":"done","stage":"reviewed"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("itemC: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, itemC.ID); err != nil {
		t.Fatalf("clear itemC transitions: %v", err)
	}
	insertTransition("c-done-1", itemC.ID, "status", "open", "done", tItemCFirstDone)
	insertTransition("c-reopen", itemC.ID, "status", "done", "open", tItemCReopen)
	insertTransition("c-done-2", itemC.ID, "status", "open", "done", tItemCSecondDone)
	insertTransition("c-stage", itemC.ID, "stage", "draft", "reviewed", tItemCOtherField)
	itemC, _ = srv.store.GetItem(itemC.ID)

	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?finished=5", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents: %d %s", rr.Code, rr.Body.String())
	}
	var got meAgentsTestResponse
	parseJSON(t, rr, &got)
	if len(got.Agents) != 1 {
		t.Fatalf("agents = %d, want 1", len(got.Agents))
	}
	fin := got.Agents[0].Finished
	if len(fin) != 3 {
		t.Fatalf("finished len = %d, want 3: %+v", len(fin), fin)
	}

	want := []struct {
		ref         string
		value       string
		completedAt string
	}{
		{itemC.Ref, "done", tItemCSecondDone.Format(time.RFC3339)},
		{itemB.Ref, "shipped", tItemBShipped.Format(time.RFC3339)},
		{itemA.Ref, "done", tItemADone.Format(time.RFC3339)},
	}
	for i, w := range want {
		if fin[i].Ref != w.ref || fin[i].Value != w.value || fin[i].CompletedAt != w.completedAt {
			t.Errorf("fin[%d] = {%s, %s, %s}, want {%s, %s, %s}",
				i, fin[i].Ref, fin[i].Value, fin[i].CompletedAt, w.ref, w.value, w.completedAt)
		}
	}
}

func TestMeAgents_FinishedNotCrowdedOut(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "crowd-owner@example.com", "Crowd Owner")
	tok := mkAgentsPAT(t, srv, owner.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Crowd WS", Slug: "crowd-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "CRD", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	roleA, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Role A", Slug: "role-a"})
	if err != nil {
		t.Fatalf("roleA: %v", err)
	}
	roleB, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Role B", Slug: "role-b"})
	if err != nil {
		t.Fatalf("roleB: %v", err)
	}
	roleC, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Role C", Slug: "role-c"})
	if err != nil {
		t.Fatalf("roleC: %v", err)
	}

	insertTransition := func(id, itemID, fieldKey, fromStatus, toStatus string, at time.Time) {
		t.Helper()
		if _, err := srv.store.DB().Exec(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id, itemID, ws.ID, coll.ID, fieldKey, fromStatus, toStatus, at.UTC().Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert transition %s: %v", id, err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)

	// Role B: 3 real completions (older than role A's 250 non-completion moves, still inside 30-day window).
	var roleBRefs []string
	for i := 0; i < 3; i++ {
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: fmt.Sprintf("B Done %d", i), Fields: `{"status":"done"}`, AgentRoleID: &roleB.ID,
		})
		if err != nil {
			t.Fatalf("roleB item %d: %v", i, err)
		}
		if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, it.ID); err != nil {
			t.Fatalf("clear transitions: %v", err)
		}
		insertTransition(fmt.Sprintf("b-done-%d", i), it.ID, "status", "open", "done", now.Add(-time.Duration(10+i)*time.Hour))
		it, _ = srv.store.GetItem(it.ID)
		roleBRefs = append(roleBRefs, it.Ref)
	}

	// Role A: 250 non-completion transitions on open items (more recent than Role B's completions).
	roleAItem, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "A Churn", Fields: `{"status":"open"}`, AgentRoleID: &roleA.ID,
	})
	if err != nil {
		t.Fatalf("roleA item: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, roleAItem.ID); err != nil {
		t.Fatalf("clear roleA transitions: %v", err)
	}
	for i := 0; i < 250; i++ {
		from, to := "open", "in_progress"
		if i%2 == 1 {
			from, to = "in_progress", "open"
		}
		insertTransition(fmt.Sprintf("a-churn-%03d", i), roleAItem.ID, "status", from, to, now.Add(-time.Duration(250-i)*time.Minute))
	}

	// Role C: item 1 completed/reopened 150 times (newest), item 2 completed once (older).
	// Repeated completions of item 1 must not crowd out item 2.
	roleCItem2, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "C Second Done", Fields: `{"status":"done"}`, AgentRoleID: &roleC.ID,
	})
	if err != nil {
		t.Fatalf("roleC item2: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, roleCItem2.ID); err != nil {
		t.Fatalf("clear roleC item2 transitions: %v", err)
	}
	insertTransition("c-item2-done", roleCItem2.ID, "status", "open", "done", now.Add(-12*time.Hour))
	roleCItem2, _ = srv.store.GetItem(roleCItem2.ID)

	roleCItem1, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "C Repeated Done", Fields: `{"status":"done"}`, AgentRoleID: &roleC.ID,
	})
	if err != nil {
		t.Fatalf("roleC item1: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, roleCItem1.ID); err != nil {
		t.Fatalf("clear roleC item1 transitions: %v", err)
	}
	baseC := now.Add(-6 * time.Hour)
	for i := 0; i < 150; i++ {
		if i > 0 {
			insertTransition(fmt.Sprintf("c-reopen-%03d", i), roleCItem1.ID, "status", "done", "open", baseC.Add(time.Duration(2*i-1)*time.Minute))
		}
		insertTransition(fmt.Sprintf("c-done-%03d", i), roleCItem1.ID, "status", "open", "done", baseC.Add(time.Duration(2*i)*time.Minute))
	}
	roleCItem1, _ = srv.store.GetItem(roleCItem1.ID)

	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?finished=5", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents: %d %s", rr.Code, rr.Body.String())
	}
	var got meAgentsTestResponse
	parseJSON(t, rr, &got)

	bySlug := make(map[string]int, len(got.Agents))
	for idx, a := range got.Agents {
		bySlug[a.Slug] = idx
	}

	bIdx, ok := bySlug["role-b"]
	if !ok {
		t.Fatalf("role-b missing from response: %+v", got.Agents)
	}
	bGroup := got.Agents[bIdx]
	if len(bGroup.Finished) != 3 {
		t.Fatalf("role-b finished len = %d, want 3: %+v", len(bGroup.Finished), bGroup.Finished)
	}
	for i, wantRef := range roleBRefs {
		if bGroup.Finished[i].Ref != wantRef {
			t.Errorf("role-b finished[%d].Ref = %q, want %q", i, bGroup.Finished[i].Ref, wantRef)
		}
	}

	cIdx, ok := bySlug["role-c"]
	if !ok {
		t.Fatalf("role-c missing from response: %+v", got.Agents)
	}
	cGroup := got.Agents[cIdx]
	if len(cGroup.Finished) != 2 {
		t.Fatalf("role-c finished len = %d, want 2: %+v", len(cGroup.Finished), cGroup.Finished)
	}
	if cGroup.Finished[0].Ref != roleCItem1.Ref || cGroup.Finished[1].Ref != roleCItem2.Ref {
		t.Errorf("role-c finished refs = [%s, %s], want [%s, %s]",
			cGroup.Finished[0].Ref, cGroup.Finished[1].Ref, roleCItem1.Ref, roleCItem2.Ref)
	}
}

func TestMeAgents_QueryBudget(t *testing.T) {
	t.Parallel()

	// Budget: 9*2 (2 workspaces with roles) + 1 (1 workspace without roles) + 2 (request-level) = 21.
	const maxBound = 9*2 + 1 + 2

	measure := func(t *testing.T, principal string, rolesPerWS int) int64 {
		t.Helper()
		srv := testServer(t)
		presence := NewMemorySessionPresence()
		srv.SetSessionPresence(presence)

		owner := mkAgentsUser(t, srv, fmt.Sprintf("budget-owner-%s-%d@example.com", principal, rolesPerWS), "Budget Owner")
		caller := owner
		if principal != "owner" {
			caller = mkAgentsUser(t, srv, fmt.Sprintf("budget-%s-%d@example.com", principal, rolesPerWS), "Budget "+principal)
		}
		tok := mkAgentsPAT(t, srv, caller.ID, "")
		workerID := "wk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		presence.Add(caller.ID, SessionIdentity{Agent: "agent-budget", WorkerID: workerID}, SessionOrigin{})

		for w := 0; w < 2; w++ {
			ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
				Name: fmt.Sprintf("Budget WS %d", w), Slug: fmt.Sprintf("budget-ws-%d", w), OwnerID: owner.ID,
			})
			if err != nil {
				t.Fatalf("ws: %v", err)
			}
			if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
				t.Fatalf("member owner: %v", err)
			}
			coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
				Name: "Tasks", Slug: "tasks", Prefix: "BDG", Schema: meAgentsStdSchema,
			})
			if err != nil {
				t.Fatalf("coll: %v", err)
			}
			hiddenColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
				Name: "Hidden", Slug: "hidden", Prefix: "HID", Schema: meAgentsStdSchema,
			})
			if err != nil {
				t.Fatalf("hiddenColl: %v", err)
			}

			switch principal {
			case "restricted_member":
				if err := srv.store.AddWorkspaceMember(ws.ID, caller.ID, "editor"); err != nil {
					t.Fatalf("member restricted: %v", err)
				}
				if err := srv.store.SetMemberCollectionAccess(ws.ID, caller.ID, "specific", []string{coll.ID}); err != nil {
					t.Fatalf("SetMemberCollectionAccess: %v", err)
				}
			case "guest":
				if _, err := srv.store.CreateCollectionGrant(ws.ID, coll.ID, caller.ID, "read", owner.ID); err != nil {
					t.Fatalf("CreateCollectionGrant: %v", err)
				}
			}

			for r := 0; r < rolesPerWS; r++ {
				role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{
					Name: fmt.Sprintf("Role %d", r), Slug: fmt.Sprintf("role-%d", r),
				})
				if err != nil {
					t.Fatalf("role: %v", err)
				}
				// 1 hidden item in hiddenColl to exercise visibility filtering.
				if _, err := srv.store.CreateItem(ws.ID, hiddenColl.ID, models.ItemCreate{
					Title: fmt.Sprintf("Hidden %d-%d", w, r), Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
				}); err != nil {
					t.Fatalf("hidden item: %v", err)
				}
				// 3 queued items
				var lastQueued *models.Item
				for k := 0; k < 3; k++ {
					it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
						Title: fmt.Sprintf("Q %d-%d-%d", w, r, k), Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
					})
					if err != nil {
						t.Fatalf("queued: %v", err)
					}
					lastQueued = it
				}
				// 1 leased Now item
				leased, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
					Title: fmt.Sprintf("Now %d-%d", w, r), Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
				})
				if err != nil {
					t.Fatalf("leased: %v", err)
				}
				if _, err := srv.store.ClaimItemLease(leased.ID, "agent-budget", workerID, caller.ID, 15*time.Minute); err != nil {
					t.Fatalf("claim: %v", err)
				}
				// Park line before last queued item
				if _, err := srv.store.SetRolePark(ws.ID, role.ID, store.ParkBefore(lastQueued.ID), nil, owner.ID); err != nil {
					t.Fatalf("park: %v", err)
				}
				// 2 finished items
				doneFields := `{"status":"done"}`
				for k := 0; k < 2; k++ {
					fin, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
						Title: fmt.Sprintf("Done %d-%d-%d", w, r, k), Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
					})
					if err != nil {
						t.Fatalf("fin: %v", err)
					}
					if _, err := srv.store.UpdateItem(fin.ID, models.ItemUpdate{Fields: &doneFields}); err != nil {
						t.Fatalf("update fin: %v", err)
					}
				}
			}
		}

		// Also add a 3rd workspace with NO roles to check the 1-query-per-empty-ws bound.
		wsNoRoles, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
			Name: "No Roles WS", Slug: "no-roles-ws", OwnerID: owner.ID,
		})
		if err != nil {
			t.Fatalf("wsNoRoles: %v", err)
		}
		if err := srv.store.AddWorkspaceMember(wsNoRoles.ID, owner.ID, "owner"); err != nil {
			t.Fatalf("member no roles: %v", err)
		}
		emptyColl, err := srv.store.CreateCollection(wsNoRoles.ID, models.CollectionCreate{
			Name: "Tasks", Slug: "tasks", Prefix: "EMP", Schema: meAgentsStdSchema,
		})
		if err != nil {
			t.Fatalf("emptyColl: %v", err)
		}
		switch principal {
		case "restricted_member":
			if err := srv.store.AddWorkspaceMember(wsNoRoles.ID, caller.ID, "editor"); err != nil {
				t.Fatalf("member restricted empty ws: %v", err)
			}
			if err := srv.store.SetMemberCollectionAccess(wsNoRoles.ID, caller.ID, "specific", []string{emptyColl.ID}); err != nil {
				t.Fatalf("SetMemberCollectionAccess empty ws: %v", err)
			}
		case "guest":
			if _, err := srv.store.CreateCollectionGrant(wsNoRoles.ID, emptyColl.ID, caller.ID, "read", owner.ID); err != nil {
				t.Fatalf("CreateCollectionGrant empty ws: %v", err)
			}
		}

		var cq *countingQueryer
		srv.meAgentsQueryerOverride = func(r *http.Request) store.Queryer {
			cq = &countingQueryer{inner: srv.store.QContext(r.Context())}
			return cq
		}

		rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(tok))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /me/agents (%s, %d roles): %d %s", principal, rolesPerWS, rr.Code, rr.Body.String())
		}
		var got meAgentsTestResponse
		parseJSON(t, rr, &got)
		if len(got.Agents) != rolesPerWS {
			t.Fatalf("%s: got %d agents, want %d", principal, len(got.Agents), rolesPerWS)
		}
		if cq == nil {
			t.Fatal("meAgentsQueryerOverride was not called")
		}
		return cq.count.Load()
	}

	for _, principal := range []string{"owner", "restricted_member", "guest"} {
		t.Run(principal, func(t *testing.T) {
			count1 := measure(t, principal, 1)
			count8 := measure(t, principal, 8)
			t.Logf("TestMeAgents_QueryBudget/%s: 1-role=%d queries, 8-role=%d queries (bound<=%d)", principal, count1, count8, maxBound)

			if count1 != count8 {
				t.Fatalf("%s: query count grew with role count: 1-role=%d, 8-role=%d (must be equal)", principal, count1, count8)
			}
			if count1 > maxBound {
				t.Fatalf("%s: query count %d exceeds budget %d (9*2 + 1 + 2)", principal, count1, maxBound)
			}
		})
	}
}

func TestMeAgents_ParamsValidation(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "params-owner@example.com", "Params Owner")
	tok := mkAgentsPAT(t, srv, owner.ID, "")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Params WS", Slug: "params-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "PRM", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Runner", Slug: "runner"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: fmt.Sprintf("Task %d", i), Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		}); err != nil {
			t.Fatalf("item: %v", err)
		}
	}

	badQueries := []string{
		"?finished=",
		"?finished=-1",
		"?finished=21",
		"?finished=abc",
		"?finished=1&finished=2",
		"?queue=",
		"?queue=0",
		"?queue=51",
		"?queue=abc",
		"?queue=1&queue=2",
	}
	for _, q := range badQueries {
		rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents"+q, nil, bearerHdr(tok))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("GET /me/agents%s: got %d %s, want 400", q, rr.Code, rr.Body.String())
			continue
		}
		var errResp map[string]any
		parseJSON(t, rr, &errResp)
		errObj, _ := errResp["error"].(map[string]any)
		if errObj["code"] != "validation_error" {
			t.Errorf("GET /me/agents%s: error code = %v, want validation_error", q, errObj["code"])
		}
	}

	// Valid queue=2 on 4 items: len(queue)=2, queue_total=4, queue_more=true; finished=0 valid.
	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?queue=2&finished=0", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents?queue=2&finished=0: %d %s", rr.Code, rr.Body.String())
	}
	var got meAgentsTestResponse
	parseJSON(t, rr, &got)
	if len(got.Agents) != 1 || len(got.Agents[0].Lanes) != 1 {
		t.Fatalf("unexpected agents: %+v", got.Agents)
	}
	lane := got.Agents[0].Lanes[0]
	if len(lane.Queue) != 2 || lane.QueueTotal != 4 || !lane.QueueMore {
		t.Errorf("lane queue len=%d total=%d more=%v, want 2/4/true", len(lane.Queue), lane.QueueTotal, lane.QueueMore)
	}
	if len(got.Agents[0].Finished) != 0 {
		t.Errorf("finished = %+v, want empty when finished=0", got.Agents[0].Finished)
	}
}

func TestMeAgents_FinishedRestrictedNotCutByHiddenRows(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "fin-vis-owner@example.com", "Fin Owner")
	guest := mkAgentsUser(t, srv, "fin-vis-guest@example.com", "Fin Guest")
	restricted := mkAgentsUser(t, srv, "fin-vis-restricted@example.com", "Fin Restricted")

	guestTok := mkAgentsPAT(t, srv, guest.ID, "")
	restrictedTok := mkAgentsPAT(t, srv, restricted.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{
		Name: "Fin Vis WS", Slug: "fin-vis-ws", OwnerID: owner.ID,
	})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, restricted.ID, "editor"); err != nil {
		t.Fatalf("member restricted: %v", err)
	}

	visColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Visible", Slug: "visible", Prefix: "VIS", Schema: meAgentsStdSchema,
	})
	if err != nil {
		t.Fatalf("visColl: %v", err)
	}
	hidColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{
		Name: "Hidden", Slug: "hidden", Prefix: "HID", Schema: meAgentsStdSchema,
	})
	if err != nil {
		t.Fatalf("hidColl: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, restricted.ID, "specific", []string{visColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Builder", Slug: "builder"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}

	insertTransition := func(id, itemID, collID string, at time.Time) {
		t.Helper()
		if _, err := srv.store.DB().Exec(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at)
			VALUES (?, ?, ?, ?, 'status', 'open', 'done', ?)`,
			id, itemID, ws.ID, collID, at.UTC().Format(time.RFC3339),
		); err != nil {
			t.Fatalf("insert transition %s: %v", id, err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)

	// 1. One completed item in hidColl granted individually to guest (oldest of all).
	guestItem, err := srv.store.CreateItem(ws.ID, hidColl.ID, models.ItemCreate{
		Title: "Guest Granted Done", Fields: `{"status":"done"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("guestItem: %v", err)
	}
	if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, guestItem.ID); err != nil {
		t.Fatalf("clear guestItem transitions: %v", err)
	}
	insertTransition("tr-guest-done", guestItem.ID, hidColl.ID, now.Add(-20*time.Hour))
	guestItem, _ = srv.store.GetItem(guestItem.ID)
	if _, err := srv.store.CreateItemGrant(ws.ID, guestItem.ID, guest.ID, "view", owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}

	// 2. Five completed items in visColl (older than the hidden ones, newer than guestItem).
	var visRefs []string
	for i := 0; i < 5; i++ {
		it, err := srv.store.CreateItem(ws.ID, visColl.ID, models.ItemCreate{
			Title: fmt.Sprintf("Visible Done %d", i), Fields: `{"status":"done"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("visItem %d: %v", i, err)
		}
		if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, it.ID); err != nil {
			t.Fatalf("clear visItem transitions: %v", err)
		}
		insertTransition(fmt.Sprintf("tr-vis-done-%d", i), it.ID, visColl.ID, now.Add(-time.Duration(10+i)*time.Hour))
		it, _ = srv.store.GetItem(it.ID)
		visRefs = append(visRefs, it.Ref)
	}

	// 3. Six newer completed items in hidColl (newest of all, invisible to both guest and restricted).
	for i := 0; i < 6; i++ {
		it, err := srv.store.CreateItem(ws.ID, hidColl.ID, models.ItemCreate{
			Title: fmt.Sprintf("Hidden Newer Done %d", i), Fields: `{"status":"done"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("hidItem %d: %v", i, err)
		}
		if _, err := srv.store.DB().Exec(`DELETE FROM status_transitions WHERE item_id = ?`, it.ID); err != nil {
			t.Fatalf("clear hidItem transitions: %v", err)
		}
		insertTransition(fmt.Sprintf("tr-hid-done-%d", i), it.ID, hidColl.ID, now.Add(-time.Duration(1+i)*time.Minute))
	}

	// Guest with finished=5 must still see their 1 granted completed item despite 6 newer hidden completions.
	rrGuest := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?finished=5", nil, bearerHdr(guestTok))
	if rrGuest.Code != http.StatusOK {
		t.Fatalf("guest GET /me/agents: %d %s", rrGuest.Code, rrGuest.Body.String())
	}
	var gotGuest meAgentsTestResponse
	parseJSON(t, rrGuest, &gotGuest)
	if len(gotGuest.Agents) != 1 {
		t.Fatalf("guest agents len = %d, want 1: %+v", len(gotGuest.Agents), gotGuest.Agents)
	}
	if len(gotGuest.Agents[0].Finished) != 1 {
		t.Fatalf("guest finished len = %d, want 1: %+v", len(gotGuest.Agents[0].Finished), gotGuest.Agents[0].Finished)
	}
	if gotGuest.Agents[0].Finished[0].Ref != guestItem.Ref {
		t.Errorf("guest finished[0].Ref = %q, want %q", gotGuest.Agents[0].Finished[0].Ref, guestItem.Ref)
	}

	// Restricted member with one visible collection gets all N=5 visible completions despite 6 newer hidden ones.
	rrRest := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents?finished=5", nil, bearerHdr(restrictedTok))
	if rrRest.Code != http.StatusOK {
		t.Fatalf("restricted GET /me/agents: %d %s", rrRest.Code, rrRest.Body.String())
	}
	var gotRest meAgentsTestResponse
	parseJSON(t, rrRest, &gotRest)
	if len(gotRest.Agents) != 1 {
		t.Fatalf("restricted agents len = %d, want 1: %+v", len(gotRest.Agents), gotRest.Agents)
	}
	if len(gotRest.Agents[0].Finished) != 5 {
		t.Fatalf("restricted finished len = %d, want 5: %+v", len(gotRest.Agents[0].Finished), gotRest.Agents[0].Finished)
	}
	for i, wantRef := range visRefs {
		if gotRest.Agents[0].Finished[i].Ref != wantRef {
			t.Errorf("restricted finished[%d].Ref = %q, want %q", i, gotRest.Agents[0].Finished[i].Ref, wantRef)
		}
	}
}

func TestAgentLanes_CarryCollectionSlug(t *testing.T) {
	t.Parallel()
	srv := testServer(t)

	owner := mkAgentsUser(t, srv, "coll-owner@example.com", "Coll Owner")
	tok := mkAgentsPAT(t, srv, owner.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Coll WS", Slug: "coll-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	bugs, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Bugs", Slug: "bugs", Prefix: "BUG", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Fixer", Slug: "fixer"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	mk := func(title, status string) *models.Item {
		t.Helper()
		it, err := srv.store.CreateItem(ws.ID, bugs.ID, models.ItemCreate{
			Title: title, Fields: fmt.Sprintf(`{"status":%q}`, status), AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		it, _ = srv.store.GetItem(it.ID)
		return it
	}
	nowItem := mk("Bug in progress", "in_progress")
	queued := mk("Bug queued", "open")
	finished := mk("Bug fixed", "open")
	done := `{"status":"done"}`
	if _, err := srv.store.UpdateItem(finished.ID, models.ItemUpdate{Fields: &done}); err != nil {
		t.Fatalf("finish: %v", err)
	}

	type refColl struct {
		Ref        string `json:"ref"`
		Collection string `json:"collection"`
	}
	type lane struct {
		Now   []refColl `json:"now"`
		Queue []refColl `json:"queue"`
	}
	check := func(where string, got []refColl, wantRef string) {
		t.Helper()
		for _, e := range got {
			if e.Ref == wantRef {
				if e.Collection != "bugs" {
					t.Errorf("%s %s: collection = %q, want %q", where, wantRef, e.Collection, "bugs")
				}
				return
			}
		}
		t.Errorf("%s: %s missing from %+v", where, wantRef, got)
	}

	rr := doRequestWithHeaders(srv, http.MethodGet, "/api/v1/workspaces/"+ws.Slug+"/agent-roles/"+role.Slug+"/queue", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET queue: %d %s", rr.Code, rr.Body.String())
	}
	var q lane
	parseJSON(t, rr, &q)
	check("queue endpoint now", q.Now, nowItem.Ref)
	check("queue endpoint queue", q.Queue, queued.Ref)

	rr = doRequestWithHeaders(srv, http.MethodGet, "/api/v1/me/agents", nil, bearerHdr(tok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /me/agents: %d %s", rr.Code, rr.Body.String())
	}
	var me struct {
		Agents []struct {
			Lanes    []lane    `json:"lanes"`
			Finished []refColl `json:"finished"`
		} `json:"agents"`
	}
	parseJSON(t, rr, &me)
	if len(me.Agents) != 1 || len(me.Agents[0].Lanes) != 1 {
		t.Fatalf("agents/lanes shape = %+v, want 1 agent with 1 lane", me.Agents)
	}
	check("me/agents now", me.Agents[0].Lanes[0].Now, nowItem.Ref)
	check("me/agents queue", me.Agents[0].Lanes[0].Queue, queued.Ref)
	check("me/agents finished", me.Agents[0].Finished, finished.Ref)
}
