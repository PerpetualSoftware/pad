package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

type roleQueueTestResponse struct {
	Workspace string `json:"workspace"`
	Role      struct {
		Slug string `json:"slug"`
		Name string `json:"name"`
		Icon string `json:"icon"`
	} `json:"role"`
	CanReorder bool `json:"can_reorder"`
	CanPark    bool `json:"can_park"`
	Now        []struct {
		Ref    string    `json:"ref"`
		Title  string    `json:"title"`
		Status string    `json:"status"`
		Since  time.Time `json:"since"`
		Lease  *struct {
			Holder    string    `json:"holder"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"lease"`
		Live string `json:"live"`
	} `json:"now"`
	Queue []struct {
		Ref           string `json:"ref"`
		Title         string `json:"title"`
		Status        string `json:"status"`
		Priority      string `json:"priority"`
		RoleSortOrder int    `json:"role_sort_order"`
		Blocked       bool   `json:"blocked"`
		Ready         bool   `json:"ready"`
		CanEdit       bool   `json:"can_edit"`
	} `json:"queue"`
	Park *struct {
		Mode           string     `json:"mode"`
		Index          *int       `json:"index"`
		FirstParkedRef *string    `json:"first_parked_ref"`
		SetBy          *string    `json:"set_by"`
		SetAt          *time.Time `json:"set_at"`
		ParkSeq        int        `json:"park_seq"`
	} `json:"park"`
	QueueTotal  int  `json:"queue_total"`
	ParkedTotal int  `json:"parked_total"`
	QueueMore   bool `json:"queue_more"`
}

func TestRoleQueue_ReadyStopsAtLine(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	// Remove hiddenItem so the unrestricted owner sees exactly the 4 open items we set up below.
	if err := f.srv.store.DeleteItem(f.hiddenItem.ID); err != nil {
		t.Fatalf("delete hiddenItem: %v", err)
	}

	// Add a 4th open item in visibleColl assigned to role so lane has 4 open queue items.
	fourth, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Four", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("create fourth: %v", err)
	}
	if err := f.srv.store.UpdateRoleSortOrder(f.ws.ID, []store.RoleSortUpdate{{ItemID: fourth.ID, RoleSortOrder: 4096}}); err != nil {
		t.Fatalf("sort fourth: %v", err)
	}
	fourth, _ = f.srv.store.GetItem(fourth.ID)

	// Add an in-progress item assigned to role -> must appear in `now`, NOT `queue`.
	nowItem, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Active Work", Fields: `{"status":"in_progress"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("create nowItem: %v", err)
	}
	nowItem, _ = f.srv.store.GetItem(nowItem.ID)

	// Wire presence and claim a leased Now item owned by owner to verify now[].live + lease.
	presence := NewMemorySessionPresence()
	f.srv.SetSessionPresence(presence)
	ownerWorker := "wk_0123456789abcdef0123456789abcdef"
	leasedNow, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Leased Work", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("create leasedNow: %v", err)
	}
	if _, err := f.srv.store.ClaimItemLease(leasedNow.ID, "agent-owner", ownerWorker, f.owner.ID, 15*time.Minute); err != nil {
		t.Fatalf("claim leasedNow: %v", err)
	}
	leasedNow, _ = f.srv.store.GetItem(leasedNow.ID)
	presence.Add(f.owner.ID, SessionIdentity{Agent: "agent-owner", WorkerID: ownerWorker}, SessionOrigin{})
	if err := presence.SetActivity(f.owner.ID, ownerWorker, true, time.Now().UTC(), nil); err != nil {
		t.Fatalf("SetActivity: %v", err)
	}

	queuePath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/queue"
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// Before parking: "park" key must be omitted.
	rr := doRequestWithHeaders(f.srv, http.MethodGet, queuePath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET unparked queue: got %d %s, want 200", rr.Code, rr.Body.String())
	}
	var rawUnparked map[string]any
	parseJSON(t, rr, &rawUnparked)
	if _, hasPark := rawUnparked["park"]; hasPark {
		t.Fatalf("unparked queue response must omit park key, got %v", rawUnparked["park"])
	}

	// Park before item 3 (f.items[2]): items 1-2 ready, items 3-4 parked.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.items[2].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park before item 3: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath+"?limit=10", nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET parked queue: %d %s", rr.Code, rr.Body.String())
	}
	var got roleQueueTestResponse
	parseJSON(t, rr, &got)

	if got.Workspace != f.ws.Slug || got.Role.Slug != f.role.Slug || got.Role.Name != f.role.Name {
		t.Fatalf("workspace/role mismatch: %+v", got)
	}
	if !got.CanReorder || !got.CanPark {
		t.Errorf("owner can_reorder=%v can_park=%v, want true/true", got.CanReorder, got.CanPark)
	}
	if got.QueueTotal != 4 || got.ParkedTotal != 2 || got.QueueMore {
		t.Fatalf("counts = total:%d parked:%d more:%v, want 4/2/false", got.QueueTotal, got.ParkedTotal, got.QueueMore)
	}
	if len(got.Queue) != 4 {
		t.Fatalf("len(queue) = %d, want 4", len(got.Queue))
	}
	wantRefs := []string{f.items[0].Ref, f.items[1].Ref, f.items[2].Ref, fourth.Ref}
	wantReady := []bool{true, true, false, false}
	for i := range wantRefs {
		if got.Queue[i].Ref != wantRefs[i] {
			t.Errorf("queue[%d].ref = %q, want %q", i, got.Queue[i].Ref, wantRefs[i])
		}
		if got.Queue[i].Ready != wantReady[i] {
			t.Errorf("queue[%d].ready = %v, want %v", i, got.Queue[i].Ready, wantReady[i])
		}
		if !got.Queue[i].CanEdit {
			t.Errorf("queue[%d].can_edit = false for owner, want true", i)
		}
	}
	if got.Park == nil || got.Park.Mode != "line" {
		t.Fatalf("park = %+v, want mode=line", got.Park)
	}
	if got.Park.Index == nil || *got.Park.Index != 2 {
		t.Errorf("park.index = %v, want 2", got.Park.Index)
	}
	if got.Park.FirstParkedRef == nil || *got.Park.FirstParkedRef != f.items[2].Ref {
		t.Errorf("park.first_parked_ref = %v, want %q", got.Park.FirstParkedRef, f.items[2].Ref)
	}
	if got.Park.SetBy == nil || *got.Park.SetBy != f.owner.Name {
		t.Errorf("park.set_by = %v, want %q", got.Park.SetBy, f.owner.Name)
	}

	// Verify Now items: in_progress item AND leased item are in `now`, NOT `queue`.
	if len(got.Now) != 2 {
		t.Fatalf("len(now) = %d, want 2 (%+v)", len(got.Now), got.Now)
	}
	nowByRef := map[string]struct {
		live     string
		hasLease bool
		holder   string
		status   string
	}{}
	for _, n := range got.Now {
		holder := ""
		if n.Lease != nil {
			holder = n.Lease.Holder
		}
		nowByRef[n.Ref] = struct {
			live     string
			hasLease bool
			holder   string
			status   string
		}{live: n.Live, hasLease: n.Lease != nil, holder: holder, status: n.Status}
	}
	if n, ok := nowByRef[nowItem.Ref]; !ok || n.hasLease || n.live != "unknown" || n.status != "in_progress" {
		t.Errorf("in_progress now entry = %+v (ok=%v), want unleased/unknown/in_progress", n, ok)
	}
	if n, ok := nowByRef[leasedNow.Ref]; !ok || !n.hasLease || n.holder != "agent-owner" || n.live != "busy" {
		t.Errorf("leased now entry = %+v (ok=%v), want leased/agent-owner/busy", n, ok)
	}

	// Viewer calling queue must NEVER see owner's live presence (must be "unknown"), and can_reorder/can_park/can_edit are false.
	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath+"?limit=10", nil, bearerHdr(f.viewerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("viewer GET queue: %d %s", rr.Code, rr.Body.String())
	}
	var viewerGot roleQueueTestResponse
	parseJSON(t, rr, &viewerGot)
	if viewerGot.CanReorder || viewerGot.CanPark {
		t.Errorf("viewer can_reorder=%v can_park=%v, want false/false", viewerGot.CanReorder, viewerGot.CanPark)
	}
	for _, q := range viewerGot.Queue {
		if q.CanEdit {
			t.Errorf("viewer queue item %s has can_edit=true, want false", q.Ref)
		}
	}
	for _, n := range viewerGot.Now {
		if n.Ref == leasedNow.Ref && n.Live != "unknown" {
			t.Errorf("LEAK: viewer saw owner's presence as %q, want unknown", n.Live)
		}
	}

	// Park at top: none ready, index 0, first_parked_ref = items[0], parked_total = 4.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park top: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath+"?limit=10", nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET top-parked queue: %d %s", rr.Code, rr.Body.String())
	}
	var topGot roleQueueTestResponse
	parseJSON(t, rr, &topGot)
	if topGot.Park == nil || topGot.Park.Mode != "top" || topGot.Park.Index == nil || *topGot.Park.Index != 0 {
		t.Fatalf("top park = %+v, want mode=top index=0", topGot.Park)
	}
	if topGot.Park.FirstParkedRef == nil || *topGot.Park.FirstParkedRef != f.items[0].Ref {
		t.Errorf("top first_parked_ref = %v, want %q", topGot.Park.FirstParkedRef, f.items[0].Ref)
	}
	if topGot.ParkedTotal != 4 || topGot.QueueTotal != 4 {
		t.Errorf("top counts = parked:%d total:%d, want 4/4", topGot.ParkedTotal, topGot.QueueTotal)
	}
	for _, q := range topGot.Queue {
		if q.Ready {
			t.Errorf("top-parked item %s has ready=true, want false", q.Ref)
		}
	}
}

func TestRoleQueue_RestrictedCountsVisibleOnly(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	// Fixture already has:
	//   hiddenItem @ 512 (in hiddenColl)
	//   items[0] @ 1024, items[1] @ 2048, items[2] @ 3072 (in visibleColl)
	// restrictedEd sees ONLY visibleColl.

	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"
	queuePath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/queue?limit=10"

	// Park at top so hiddenItem (512) is first workspace-wide.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park top: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath, nil, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted GET queue: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.hiddenItem.Ref) {
		t.Fatalf("LEAK: restricted response named hidden item %q:\n%s", f.hiddenItem.Ref, rr.Body.String())
	}
	var got roleQueueTestResponse
	parseJSON(t, rr, &got)
	if got.CanPark {
		t.Errorf("restricted editor must have can_park=false")
	}
	if got.QueueTotal != 3 || got.ParkedTotal != 3 || len(got.Queue) != 3 {
		t.Fatalf("restricted counts = total:%d parked:%d len:%d, want 3/3/3", got.QueueTotal, got.ParkedTotal, len(got.Queue))
	}
	if got.Park == nil || got.Park.Index == nil || *got.Park.Index != 0 {
		t.Fatalf("restricted park = %+v, want index=0", got.Park)
	}
	if got.Park.FirstParkedRef == nil || *got.Park.FirstParkedRef != f.items[0].Ref {
		t.Fatalf("restricted first_parked_ref = %v, want %q", got.Park.FirstParkedRef, f.items[0].Ref)
	}

	// Now park before a hidden item placed AFTER items[2] (at 4096): restricted caller sees 0 parked items, index null, first_parked_ref null.
	hiddenLate, err := f.srv.store.CreateItem(f.ws.ID, f.hiddenColl.ID, models.ItemCreate{
		Title: "Hidden Late", Fields: `{"status":"open"}`, AgentRoleID: &f.role.ID,
	})
	if err != nil {
		t.Fatalf("create hiddenLate: %v", err)
	}
	if err := f.srv.store.UpdateRoleSortOrder(f.ws.ID, []store.RoleSortUpdate{{ItemID: hiddenLate.ID, RoleSortOrder: 4096}}); err != nil {
		t.Fatalf("sort hiddenLate: %v", err)
	}
	hiddenLate, _ = f.srv.store.GetItem(hiddenLate.ID)

	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": hiddenLate.Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park before hiddenLate: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath, nil, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted GET queue (hidden parked): %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), hiddenLate.Ref) {
		t.Fatalf("LEAK: restricted response named hiddenLate %q:\n%s", hiddenLate.Ref, rr.Body.String())
	}
	var gotHidden roleQueueTestResponse
	parseJSON(t, rr, &gotHidden)
	if gotHidden.QueueTotal != 3 || gotHidden.ParkedTotal != 0 {
		t.Fatalf("restricted hidden-parked counts = total:%d parked:%d, want 3/0", gotHidden.QueueTotal, gotHidden.ParkedTotal)
	}
	if gotHidden.Park == nil || gotHidden.Park.Index != nil || gotHidden.Park.FirstParkedRef != nil {
		t.Fatalf("restricted hidden-parked park = %+v, want index=nil first_parked_ref=nil", gotHidden.Park)
	}

	// Also verify guest (non-member with an item-level grant to only items[1]): sees 1 item only.
	guest, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "park-guest@example.com", Name: "Gina Guest", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser guest: %v", err)
	}
	if _, err := f.srv.store.CreateItemGrant(f.ws.ID, f.items[1].ID, guest.ID, "view", f.owner.ID); err != nil {
		t.Fatalf("CreateItemGrant: %v", err)
	}
	guestTok, err := f.srv.store.CreateAPIToken(guest.ID, models.APITokenCreate{
		Name: "guest-pat", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("CreateAPIToken guest: %v", err)
	}

	// Park before items[1] so guest's sole visible item is parked at index 0.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.items[1].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park before items[1]: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath, nil, bearerHdr(guestTok.Token))
	if rr.Code != http.StatusOK {
		t.Fatalf("guest GET queue: %d %s", rr.Code, rr.Body.String())
	}
	var guestGot roleQueueTestResponse
	parseJSON(t, rr, &guestGot)
	if guestGot.QueueTotal != 1 || guestGot.ParkedTotal != 1 || len(guestGot.Queue) != 1 || guestGot.Queue[0].Ref != f.items[1].Ref {
		t.Fatalf("guest queue = %+v, want only %s", guestGot, f.items[1].Ref)
	}
	if guestGot.Park == nil || guestGot.Park.Index == nil || *guestGot.Park.Index != 0 || guestGot.Park.FirstParkedRef == nil || *guestGot.Park.FirstParkedRef != f.items[1].Ref {
		t.Fatalf("guest park = %+v, want index=0 ref=%s", guestGot.Park, f.items[1].Ref)
	}
}

func TestRoleQueue_BlockedNotReady(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	if err := f.srv.store.DeleteItem(f.hiddenItem.ID); err != nil {
		t.Fatalf("delete hiddenItem: %v", err)
	}

	// Create an open blocker item and link blocker -> blocks -> items[0].
	blocker, err := f.srv.store.CreateItem(f.ws.ID, f.visibleColl.ID, models.ItemCreate{
		Title: "Blocker", Fields: `{"status":"open"}`,
	})
	if err != nil {
		t.Fatalf("create blocker: %v", err)
	}
	if _, err := f.srv.store.CreateItemLink(f.ws.ID, models.ItemLinkCreate{
		TargetID: f.items[0].ID,
		LinkType: models.ItemLinkTypeBlocks,
	}, blocker.ID); err != nil {
		t.Fatalf("create blocks link: %v", err)
	}

	// Park before items[2]: items[0] and items[1] are ABOVE the line, but items[0] is blocked.
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.items[2].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park: %d %s", rr.Code, rr.Body.String())
	}

	queuePath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/queue?limit=10"
	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET queue: %d %s", rr.Code, rr.Body.String())
	}
	var got roleQueueTestResponse
	parseJSON(t, rr, &got)
	if len(got.Queue) != 3 {
		t.Fatalf("len(queue) = %d, want 3", len(got.Queue))
	}
	if !got.Queue[0].Blocked || got.Queue[0].Ready {
		t.Fatalf("blocked item above line: blocked=%v ready=%v, want true/false", got.Queue[0].Blocked, got.Queue[0].Ready)
	}
	if got.Queue[1].Blocked || !got.Queue[1].Ready {
		t.Fatalf("unblocked item above line: blocked=%v ready=%v, want false/true", got.Queue[1].Blocked, got.Queue[1].Ready)
	}

	// Close the blocker -> items[0] becomes unblocked and ready.
	done := `{"status":"done"}`
	if _, err := f.srv.store.UpdateItem(blocker.ID, models.ItemUpdate{Fields: &done}); err != nil {
		t.Fatalf("close blocker: %v", err)
	}

	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET queue after close: %d %s", rr.Code, rr.Body.String())
	}
	var after roleQueueTestResponse
	parseJSON(t, rr, &after)
	if after.Queue[0].Blocked || !after.Queue[0].Ready {
		t.Fatalf("after blocker closed: blocked=%v ready=%v, want false/true", after.Queue[0].Blocked, after.Queue[0].Ready)
	}
}

func TestRoleQueue_LimitValidationAnd404(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	if err := f.srv.store.DeleteItem(f.hiddenItem.ID); err != nil {
		t.Fatalf("delete hiddenItem: %v", err)
	}
	queuePath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/queue"

	for _, bad := range []string{"0", "-1", "51", "x", "1.5", ""} {
		t.Run("limit="+bad, func(t *testing.T) {
			rr := doRequestWithHeaders(f.srv, http.MethodGet, queuePath+"?limit="+bad, nil, bearerHdr(f.ownerTok))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("limit=%q: got %d %s, want 400", bad, rr.Code, rr.Body.String())
			}
			var env struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			parseJSON(t, rr, &env)
			if env.Error.Code != "validation_error" {
				t.Fatalf("limit=%q: code = %q, want validation_error", bad, env.Error.Code)
			}
		})
	}

	// Unknown role -> 404 not_found.
	unknownPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/no-such-role/queue"
	rr := doRequestWithHeaders(f.srv, http.MethodGet, unknownPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("unknown role: got %d %s, want 404", rr.Code, rr.Body.String())
	}
	var nf struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	parseJSON(t, rr, &nf)
	if nf.Error.Code != "not_found" {
		t.Fatalf("unknown role code = %q, want not_found", nf.Error.Code)
	}

	// Limit 2 on 3 items -> returns 2 items, queue_total 3, queue_more true.
	rr = doRequestWithHeaders(f.srv, http.MethodGet, queuePath+"?limit=2", nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("limit=2: got %d %s, want 200", rr.Code, rr.Body.String())
	}
	var got roleQueueTestResponse
	parseJSON(t, rr, &got)
	if len(got.Queue) != 2 || got.QueueTotal != 3 || !got.QueueMore {
		t.Fatalf("limit=2: len=%d total=%d more=%v, want 2/3/true", len(got.Queue), got.QueueTotal, got.QueueMore)
	}
}

func TestSuggestedNext_ExcludesParked(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	if err := f.srv.store.DeleteItem(f.hiddenItem.ID); err != nil {
		t.Fatalf("delete hiddenItem: %v", err)
	}

	// Make items[0] high priority open (eligible for suggested_next) and items[1] in_progress (Now item).
	hiOpen := `{"status":"open","priority":"high"}`
	if _, err := f.srv.store.UpdateItem(f.items[0].ID, models.ItemUpdate{Fields: &hiOpen}); err != nil {
		t.Fatalf("update items[0]: %v", err)
	}
	inProg := `{"status":"in_progress","priority":"medium"}`
	if _, err := f.srv.store.UpdateItem(f.items[1].ID, models.ItemUpdate{Fields: &inProg}); err != nil {
		t.Fatalf("update items[1]: %v", err)
	}
	// Delete items[2] so the workspace has only these two items.
	if err := f.srv.store.DeleteItem(f.items[2].ID); err != nil {
		t.Fatalf("delete items[2]: %v", err)
	}

	dashPath := "/api/v1/workspaces/" + f.ws.Slug + "/dashboard"
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	hasSuggestion := func(slug string) bool {
		rr := doRequestWithHeaders(f.srv, http.MethodGet, dashPath, nil, bearerHdr(f.ownerTok))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET dashboard: %d %s", rr.Code, rr.Body.String())
		}
		var dash DashboardResponse
		parseJSON(t, rr, &dash)
		for _, s := range dash.SuggestedNext {
			if s.ItemSlug == slug {
				return true
			}
		}
		return false
	}

	// Before parking: both items[0] (high open) and items[1] (in_progress) are suggested.
	if !hasSuggestion(f.items[0].Slug) {
		t.Fatalf("premise: unparked high-priority item %s should be in suggested_next", f.items[0].Slug)
	}

	// Park the role at top: high-priority queue item leaves suggested_next, but in_progress Now item STAYS.
	rr := doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park top: %d %s", rr.Code, rr.Body.String())
	}
	if hasSuggestion(f.items[0].Slug) {
		t.Fatalf("parked high-priority queue item %s must NOT appear in suggested_next", f.items[0].Slug)
	}
	if !hasSuggestion(f.items[1].Slug) {
		t.Fatalf("in-progress Now item %s of top-parked role must STILL appear in suggested_next", f.items[1].Slug)
	}

	// Unpark -> high-priority item returns to suggested_next.
	rr = doRequestWithHeaders(f.srv, http.MethodDelete, parkPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("DELETE park: %d %s", rr.Code, rr.Body.String())
	}
	if !hasSuggestion(f.items[0].Slug) {
		t.Fatalf("unparked high-priority item %s should return to suggested_next", f.items[0].Slug)
	}
}

func TestBootstrap_ParkOmittedByteIdentical(t *testing.T) {
	t.Parallel()
	f := newParkEndpointFixture(t)
	bootPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent/bootstrap"
	parkPath := "/api/v1/workspaces/" + f.ws.Slug + "/agent-roles/" + f.role.Slug + "/park"

	// With no park: bootstrap JSON must contain no "park" key anywhere.
	rr := doRequestWithHeaders(f.srv, http.MethodGet, bootPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET unparked bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), `"park"`) {
		t.Fatalf("unparked bootstrap JSON must not contain \"park\" key:\n%s", rr.Body.String())
	}

	// Park before items[1] (visibleColl); hiddenItem is at 512 (above line) so items[1] is first parked for both owner and restrictedEd.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"before": f.items[1].Ref}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park: %d %s", rr.Code, rr.Body.String())
	}

	rr = doRequestWithHeaders(f.srv, http.MethodGet, bootPath, nil, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET parked bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	var boot struct {
		Roles []struct {
			Slug string `json:"slug"`
			Park *struct {
				Mode           string     `json:"mode"`
				FirstParkedRef *string    `json:"first_parked_ref"`
				SetBy          *string    `json:"set_by"`
				SetAt          *time.Time `json:"set_at"`
				ParkSeq        int        `json:"park_seq"`
			} `json:"park"`
		} `json:"roles"`
	}
	parseJSON(t, rr, &boot)

	var foundRole, foundOther bool
	for _, r := range boot.Roles {
		switch r.Slug {
		case f.role.Slug:
			foundRole = true
			if r.Park == nil {
				t.Fatalf("parked role %s missing park in bootstrap", r.Slug)
			}
			if r.Park.Mode != "line" {
				t.Errorf("bootstrap park.mode = %q, want line", r.Park.Mode)
			}
			if r.Park.FirstParkedRef == nil || *r.Park.FirstParkedRef != f.items[1].Ref {
				t.Errorf("bootstrap park.first_parked_ref = %v, want %q", r.Park.FirstParkedRef, f.items[1].Ref)
			}
			if r.Park.SetBy == nil || *r.Park.SetBy != f.owner.Name {
				t.Errorf("bootstrap park.set_by = %v, want %q", r.Park.SetBy, f.owner.Name)
			}
			if r.Park.SetAt == nil || r.Park.ParkSeq != 1 {
				t.Errorf("bootstrap park.set_at/park_seq = %v/%d", r.Park.SetAt, r.Park.ParkSeq)
			}
		case f.otherRole.Slug:
			foundOther = true
			if r.Park != nil {
				t.Errorf("unparked role %s must omit park in bootstrap, got %+v", r.Slug, r.Park)
			}
		}
	}
	if !foundRole || !foundOther {
		t.Fatalf("expected both roles in bootstrap, got foundRole=%v foundOther=%v", foundRole, foundOther)
	}

	// Park at top (hiddenItem @ 512 is first workspace-wide): restricted caller must get items[0].Ref, NEVER hiddenItem.Ref.
	rr = doRequestWithHeaders(f.srv, http.MethodPut, parkPath, map[string]any{"at": "top"}, bearerHdr(f.ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT park top: %d %s", rr.Code, rr.Body.String())
	}
	rr = doRequestWithHeaders(f.srv, http.MethodGet, bootPath, nil, bearerHdr(f.restrictedTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("restricted GET bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), f.hiddenItem.Ref) {
		t.Fatalf("LEAK: restricted bootstrap named hidden item %q", f.hiddenItem.Ref)
	}
	var rBoot struct {
		Roles []struct {
			Slug string `json:"slug"`
			Park *struct {
				FirstParkedRef *string `json:"first_parked_ref"`
			} `json:"park"`
		} `json:"roles"`
	}
	parseJSON(t, rr, &rBoot)
	for _, r := range rBoot.Roles {
		if r.Slug == f.role.Slug {
			if r.Park == nil || r.Park.FirstParkedRef == nil || *r.Park.FirstParkedRef != f.items[0].Ref {
				t.Fatalf("restricted bootstrap first_parked_ref = %+v, want %q", r.Park, f.items[0].Ref)
			}
		}
	}
}
