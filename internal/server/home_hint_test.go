package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

func waitForHomeHint(t *testing.T, ch <-chan watchSSEEvent, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("watch stream closed while waiting for home_changed")
			}
			if ev.Type != "notification" {
				continue
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(ev.Data), &raw); err != nil {
				t.Fatalf("unmarshal SSE payload: %v (%s)", err, ev.Data)
			}
			if raw["kind"] == watchevents.KindHomeChanged {
				return raw
			}
		case <-deadline:
			t.Fatal("timed out waiting for home_changed event")
			return nil
		}
	}
}

func assertNoHomeHint(t *testing.T, ch <-chan watchSSEEvent, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if ev.Type != "notification" {
				continue
			}
			var raw map[string]any
			if err := json.Unmarshal([]byte(ev.Data), &raw); err == nil && raw["kind"] == watchevents.KindHomeChanged {
				t.Fatalf("unexpected home_changed delivered: %s", ev.Data)
			}
		case <-deadline:
			return
		}
	}
}

func TestWatchStream_HomeHintOptInAndVisibility(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner := mkAgentsUser(t, srv, "hh-owner@example.com", "HH Owner")
	restricted := mkAgentsUser(t, srv, "hh-restricted@example.com", "HH Restricted")
	outsider := mkAgentsUser(t, srv, "hh-outsider@example.com", "HH Outsider")

	ownerTok := mkAgentsPAT(t, srv, owner.ID, "")
	restrictedTok := mkAgentsPAT(t, srv, restricted.ID, "")
	outsiderTok := mkAgentsPAT(t, srv, outsider.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Hint WS", Slug: "hint-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, restricted.ID, "editor"); err != nil {
		t.Fatalf("member restricted: %v", err)
	}

	visColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Visible", Slug: "visible", Prefix: "VIS", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("visColl: %v", err)
	}
	hidColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Hidden", Slug: "hidden", Prefix: "HID", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("hidColl: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, restricted.ID, "specific", []string{visColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	visItem, err := srv.store.CreateItem(ws.ID, visColl.ID, models.ItemCreate{Title: "Visible Item", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("visItem: %v", err)
	}
	hidItem, err := srv.store.CreateItem(ws.ID, hidColl.ID, models.ItemCreate{Title: "Hidden Item", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("hidItem: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Stream WITHOUT ?home=true (even for owner)
	chNoOpt := connectWatchStream(ctx, t, ts.URL, ownerTok)
	waitForWatchEvent(t, chNoOpt, 3*time.Second) // connected

	// 2. Streams WITH ?home=true (unarmed — home_changed is not behind armed gate)
	chRestricted := connectWatchStreamWithHeadersAndQuery(ctx, t, ts.URL, restrictedTok, nil, "home=true")
	waitForWatchEvent(t, chRestricted, 3*time.Second)

	chOutsider := connectWatchStreamWithHeadersAndQuery(ctx, t, ts.URL, outsiderTok, nil, "home=true")
	waitForWatchEvent(t, chOutsider, 3*time.Second)

	// Publish item-level hint for HIDDEN item: restricted must NOT receive it, noOpt must NOT receive it.
	srv.publishHomeChanged(ws.ID, hidColl.ID, hidItem.ID)
	assertNoHomeHint(t, chNoOpt, 150*time.Millisecond)
	assertNoHomeHint(t, chRestricted, 150*time.Millisecond)
	assertNoHomeHint(t, chOutsider, 150*time.Millisecond)

	// Publish item-level hint for VISIBLE item: restricted receives it, noOpt and outsider do NOT.
	srv.publishHomeChanged(ws.ID, visColl.ID, visItem.ID)
	gotVis := waitForHomeHint(t, chRestricted, 3*time.Second)
	assertNoHomeHint(t, chNoOpt, 150*time.Millisecond)
	assertNoHomeHint(t, chOutsider, 150*time.Millisecond)

	// Assert exact JSON keys: id, ts, workspace, kind ONLY.
	keys := make([]string, 0, len(gotVis))
	for k := range gotVis {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	wantKeys := []string{"id", "kind", "ts", "workspace"}
	if !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("home_changed JSON keys = %v, want exact %v (payload: %+v)", keys, wantKeys, gotVis)
	}
	if gotVis["workspace"] != ws.Slug {
		t.Errorf("workspace = %v, want %q", gotVis["workspace"], ws.Slug)
	}
	if gotVis["kind"] != watchevents.KindHomeChanged {
		t.Errorf("kind = %v, want %q", gotVis["kind"], watchevents.KindHomeChanged)
	}

	// Publish workspace-level hint (no item): restricted (member) receives it; outsider (non-member) does NOT.
	srv.publishHomeChanged(ws.ID, "", "")
	gotWS := waitForHomeHint(t, chRestricted, 3*time.Second)
	if gotWS["workspace"] != ws.Slug || gotWS["kind"] != watchevents.KindHomeChanged {
		t.Errorf("ws-level hint = %+v, want workspace=%s kind=home_changed", gotWS, ws.Slug)
	}
	assertNoHomeHint(t, chOutsider, 150*time.Millisecond)
	assertNoHomeHint(t, chNoOpt, 150*time.Millisecond)
}

func TestHomeHint_ProducersCovered(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	// Bypass per-key coalescing so consecutive rows touching the same item
	// each observe their own immediate hint.
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}
	// This table exercises 35+ routes sequentially under one owner token; bypass
	// the general API burst limiter so the later rows are not 429'd.
	srv.rateLimiters.Stop()
	srv.rateLimiters = nil

	owner := mkAgentsUser(t, srv, "prod-owner@example.com", "Prod Owner")
	viewer := mkAgentsUser(t, srv, "prod-viewer@example.com", "Prod Viewer")
	ownerTok := mkAgentsPAT(t, srv, owner.ID, "")
	viewerTok := mkAgentsPAT(t, srv, viewer.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Prod WS", Slug: "prod-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, viewer.ID, "viewer"); err != nil {
		t.Fatalf("member viewer: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "PRD", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	role, err := srv.store.CreateAgentRole(ws.ID, models.AgentRoleCreate{Name: "Builder", Slug: "builder"})
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Base Item", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	item, _ = srv.store.GetItem(item.ID)

	schedItem, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
		Title: "Sched Item", Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
	})
	if err != nil {
		t.Fatalf("schedItem: %v", err)
	}
	schedItem, _ = srv.store.GetItem(schedItem.ID)

	// Extra items for recurring resolve states, renegotiate (+ cascade), move, links, gates, milestone children, and reminders.
	mkItem := func(title string) *models.Item {
		it, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{
			Title: title, Fields: `{"status":"open"}`, AgentRoleID: &role.ID,
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		it, _ = srv.store.GetItem(it.ID)
		return it
	}
	recurItem := mkItem("Recur Item")
	ackSchedItem := mkItem("Ack Sched Item")
	renegItem := mkItem("Reneg Item")
	renegChild := mkItem("Reneg Child")
	moveItem := mkItem("Move Item")
	linkParent := mkItem("Link Parent")
	linkChild := mkItem("Link Child")
	gateBlocked := mkItem("Gate Blocked")
	gateBlocker := mkItem("Gate Blocker")
	milestoneItem := mkItem("Milestone Item")
	milestoneChild := mkItem("Milestone Child")
	remItem := mkItem("Reminder Item")
	remAckItem := mkItem("Reminder Ack Item")

	latestBusID := func() int64 {
		evs := srv.watchEvents.EventsSince(0)
		if len(evs) == 0 {
			return 0
		}
		return evs[len(evs)-1].ID
	}

	homeHintsSince := func(sinceID int64) []watchevents.Notification {
		var out []watchevents.Notification
		for _, n := range srv.watchEvents.EventsSince(sinceID) {
			if n.Kind == watchevents.KindHomeChanged {
				out = append(out, n)
			}
		}
		return out
	}

	wsBase := "/api/v1/workspaces/" + ws.Slug
	var reopenResolutionID string
	var createdLinkID string
	var createdGateID string
	var createdReminderID string
	var ackByIDReminderID string

	getNextOccAt := func(t *testing.T, slug string) string {
		t.Helper()
		rr := doRequestWithHeaders(srv, http.MethodGet, wsBase+"/items/"+slug+"/commitment", nil, bearerHdr(ownerTok))
		if rr.Code != http.StatusOK {
			t.Fatalf("GET commitment %s: %d %s", slug, rr.Code, rr.Body.String())
		}
		var cm map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &cm); err != nil {
			t.Fatalf("unmarshal commitment: %v", err)
		}
		next, ok := cm["next_occurrence"].(map[string]any)
		if !ok {
			t.Fatalf("missing next_occurrence on %s: %s", slug, rr.Body.String())
		}
		at, _ := next["at"].(string)
		if at == "" {
			t.Fatalf("empty next_occurrence.at on %s: %s", slug, rr.Body.String())
		}
		return at
	}

	mustCreateCommitment := func(t *testing.T, it *models.Item, body map[string]any) {
		t.Helper()
		rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+it.Slug+"/commitment", body, bearerHdr(ownerTok))
		if rr.Code != http.StatusCreated {
			t.Fatalf("setup create commitment on %s: %d %s", it.Slug, rr.Code, rr.Body.String())
		}
	}

	type producerRow struct {
		name        string
		itemLevel   bool
		expectItems []string // every listed item ID must appear in hints; empty with itemLevel=true means any non-empty ItemID
		setup       func(t *testing.T)
		runPreview  func(t *testing.T) *httptest.ResponseRecorder // optional: must return 200 and publish 0 home_changed hints
		runOK       func(t *testing.T) *httptest.ResponseRecorder
		wantOKCode  int
		runRefused  func(t *testing.T) *httptest.ResponseRecorder
	}

	rows := []producerRow{
		{
			name:       "single_item_create",
			itemLevel:  true,
			wantOKCode: http.StatusCreated,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/collections/"+coll.Slug+"/items",
					map[string]any{"title": "Created Item", "fields": `{"status":"open"}`}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/collections/"+coll.Slug+"/items",
					map[string]any{"title": "Refused Create"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "single_item_update",
			itemLevel:   true,
			expectItems: []string{item.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/items/"+item.Slug,
					map[string]any{"title": "Updated Title"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/items/"+item.Slug,
					map[string]any{"title": "Viewer Update"}, bearerHdr(viewerTok))
			},
		},
		{
			name:       "bulk_items_update",
			itemLevel:  false,
			wantOKCode: http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/bulk",
					map[string]any{"op": "set-priority", "ids": []string{item.ID}, "priority": "high"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/bulk",
					map[string]any{"op": "set-priority", "ids": []string{item.ID}, "priority": "low"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "lease_claim",
			itemLevel:   true,
			expectItems: []string{item.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/claim",
					map[string]any{"holder": "agent-prod"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/claim",
					map[string]any{"holder": "viewer-agent"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "lease_release",
			itemLevel:   true,
			expectItems: []string{item.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/release",
					map[string]any{"holder": "agent-prod"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/release",
					map[string]any{"holder": "viewer-agent"}, bearerHdr(viewerTok))
			},
		},
		{
			name:       "role_board_reorder",
			itemLevel:  false,
			wantOKCode: http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/roles/board/reorder",
					[]store.RoleSortUpdate{{ItemID: item.ID, RoleSortOrder: 2048}}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/roles/board/reorder",
					[]store.RoleSortUpdate{{ItemID: item.ID, RoleSortOrder: 3072}}, bearerHdr(viewerTok))
			},
		},
		{
			name:       "role_board_lane_reorder",
			itemLevel:  false,
			wantOKCode: http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/roles/board/lane-order",
					[]store.RoleOrderUpdate{{RoleID: role.ID, SortOrder: 1}}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/roles/board/lane-order",
					[]store.RoleOrderUpdate{{RoleID: role.ID, SortOrder: 2}}, bearerHdr(viewerTok))
			},
		},
		{
			name:       "role_park_set",
			itemLevel:  false,
			wantOKCode: http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/agent-roles/"+role.Slug+"/park",
					map[string]any{"at": "top"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/agent-roles/"+role.Slug+"/park",
					map[string]any{"at": "top"}, bearerHdr(viewerTok))
			},
		},
		{
			name:       "role_park_clear",
			itemLevel:  false,
			wantOKCode: http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/agent-roles/"+role.Slug+"/park",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/agent-roles/"+role.Slug+"/park",
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment",
			itemLevel:   true,
			expectItems: []string{schedItem.ID},
			wantOKCode:  http.StatusCreated,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment",
					map[string]any{"roles": map[string]any{}, "dtstart": "2099-01-01T09:00", "tz": "UTC"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment",
					map[string]any{"roles": map[string]any{}, "dtstart": "2099-01-01T09:00", "tz": "UTC"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "PATCH /commitment",
			itemLevel:   true,
			expectItems: []string{schedItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/items/"+schedItem.Slug+"/commitment",
					map[string]any{"dtstart": "2099-01-02T09:00"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/items/"+schedItem.Slug+"/commitment",
					map[string]any{"dtstart": "2099-01-03T09:00"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/resolve (done recurring)",
			itemLevel:   true,
			expectItems: []string{recurItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				startDay := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
				mustCreateCommitment(t, recurItem, map[string]any{
					"roles": map[string]any{}, "dtstart": startDay + "T09:00", "tz": "UTC", "rrule": "FREQ=DAILY",
				})
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "done"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "done"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/resolve (skip)",
			itemLevel:   true,
			expectItems: []string{recurItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "skip"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "skip"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/resolve (snooze)",
			itemLevel:   true,
			expectItems: []string{recurItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				until := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second).Format(time.RFC3339)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "snooze", "until": until}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				until := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second).Format(time.RFC3339)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "snooze", "until": until}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/resolve (move)",
			itemLevel:   true,
			expectItems: []string{recurItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				until := time.Now().UTC().AddDate(0, 0, 12).Truncate(time.Second).Format(time.RFC3339)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "move", "until": until}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, recurItem.Slug)
				until := time.Now().UTC().AddDate(0, 0, 12).Truncate(time.Second).Format(time.RFC3339)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+recurItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "move", "until": until}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/resolve (ack)",
			itemLevel:   true,
			expectItems: []string{ackSchedItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				startDay := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")
				mustCreateCommitment(t, ackSchedItem, map[string]any{
					"roles": map[string]any{}, "dtstart": startDay + "T09:00", "tz": "UTC",
				})
				at := getNextOccAt(t, ackSchedItem.Slug)
				if _, err := srv.store.DB().Exec(`UPDATE item_reminders SET fired_at = ? WHERE item_id = ?`, at, ackSchedItem.ID); err != nil {
					t.Fatalf("mark ackSchedItem fired: %v", err)
				}
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, ackSchedItem.Slug)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+ackSchedItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "ack"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				at := getNextOccAt(t, ackSchedItem.Slug)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+ackSchedItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": at, "state": "ack"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/resolve (done+end)",
			itemLevel:   true,
			expectItems: []string{schedItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": "2099-01-02T09:00:00Z", "state": "done", "end_schedule": true}, bearerHdr(ownerTok))
				if rr.Code == http.StatusOK {
					// Fetch resolution id for reopen row below.
					getRR := doRequestWithHeaders(srv, http.MethodGet, wsBase+"/items/"+schedItem.Slug+"/commitment", nil, bearerHdr(ownerTok))
					var cm map[string]any
					if err := json.Unmarshal(getRR.Body.Bytes(), &cm); err == nil {
						if lr, ok := cm["last_resolution"].(map[string]any); ok {
							reopenResolutionID, _ = lr["resolution_id"].(string)
						}
					}
				}
				return rr
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": "2099-01-02T09:00:00Z", "state": "done", "end_schedule": true}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/reopen",
			itemLevel:   true,
			expectItems: []string{schedItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				if reopenResolutionID == "" {
					t.Fatal("missing reopenResolutionID from prior resolve row")
				}
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/reopen",
					map[string]any{"resolution_id": reopenResolutionID}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/reopen",
					map[string]any{"resolution_id": "11111111-1111-4111-8111-111111111111"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/dismiss-ended",
			itemLevel:   true,
			expectItems: []string{schedItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/resolve",
					map[string]any{"occurrence_at": "2099-01-02T09:00:00Z", "state": "done", "end_schedule": true}, bearerHdr(ownerTok))
				if rr.Code != http.StatusOK {
					t.Fatalf("setup resolve before dismiss-ended: %d %s", rr.Code, rr.Body.String())
				}
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/dismiss-ended",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+schedItem.Slug+"/commitment/dismiss-ended",
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/renegotiate",
			itemLevel:   true,
			expectItems: []string{renegItem.ID, renegChild.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				startDay := time.Now().UTC().AddDate(0, 0, 5).Format("2006-01-02")
				childDay := time.Now().UTC().AddDate(0, 0, 6).Format("2006-01-02")
				mustCreateCommitment(t, renegItem, map[string]any{
					"roles":   map[string]any{"deadline_days": 0, "deadline_secs": 8 * 3600, "deadline_behavior": "lapses"},
					"dtstart": startDay + "T09:00", "tz": "UTC",
				})
				mustCreateCommitment(t, renegChild, map[string]any{
					"roles":   map[string]any{},
					"dtstart": childDay + "T09:00", "tz": "UTC",
				})
				// Link renegItem.deadline -> renegChild.target so renegotiate cascades to renegChild too.
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+renegItem.Slug+"/commitment/links",
					map[string]any{
						"child":       renegChild.Slug,
						"parent_role": "deadline",
						"child_role":  "target",
						"offset":      map[string]any{"mode": "elapsed", "elapsed_secs": 3600},
					}, bearerHdr(ownerTok))
				if rr.Code != http.StatusCreated {
					t.Fatalf("setup link for renegotiate cascade: %d %s", rr.Code, rr.Body.String())
				}
			},
			runPreview: func(t *testing.T) *httptest.ResponseRecorder {
				newDeadline := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second).Format(time.RFC3339)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+renegItem.Slug+"/commitment/renegotiate",
					map[string]any{"reason": "scope change", "scope": "occurrence", "deadline": newDeadline, "preview": true}, bearerHdr(ownerTok))
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				newDeadline := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second).Format(time.RFC3339)
				prev := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+renegItem.Slug+"/commitment/renegotiate",
					map[string]any{"reason": "scope change", "scope": "occurrence", "deadline": newDeadline, "preview": true}, bearerHdr(ownerTok))
				if prev.Code != http.StatusOK {
					t.Fatalf("renegotiate preview for revision: %d %s", prev.Code, prev.Body.String())
				}
				var p struct {
					Revision string `json:"revision"`
				}
				if err := json.Unmarshal(prev.Body.Bytes(), &p); err != nil {
					t.Fatalf("unmarshal renegotiate preview: %v", err)
				}
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+renegItem.Slug+"/commitment/renegotiate",
					map[string]any{"reason": "scope change", "scope": "occurrence", "deadline": newDeadline, "revision": p.Revision}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				newDeadline := time.Now().UTC().AddDate(0, 0, 10).Truncate(time.Second).Format(time.RFC3339)
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+renegItem.Slug+"/commitment/renegotiate",
					map[string]any{"reason": "scope change", "scope": "occurrence", "deadline": newDeadline}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/move",
			itemLevel:   true,
			expectItems: []string{moveItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				mustCreateCommitment(t, moveItem, map[string]any{
					"roles": map[string]any{}, "dtstart": "2099-04-01T09:00", "tz": "UTC",
				})
			},
			runPreview: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+moveItem.Slug+"/commitment/move-preview",
					map[string]any{"role": "target", "to": "2099-04-02T09:00:00Z", "reason": "shift"}, bearerHdr(ownerTok))
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+moveItem.Slug+"/commitment/move",
					map[string]any{"role": "target", "to": "2099-04-02T09:00:00Z", "reason": "shift"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+moveItem.Slug+"/commitment/move",
					map[string]any{"role": "target", "to": "2099-04-03T09:00:00Z", "reason": "shift"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/links",
			itemLevel:   true,
			expectItems: []string{linkParent.ID, linkChild.ID},
			wantOKCode:  http.StatusCreated,
			setup: func(t *testing.T) {
				mustCreateCommitment(t, linkParent, map[string]any{
					"roles": map[string]any{}, "dtstart": "2099-05-01T09:00", "tz": "UTC",
				})
				mustCreateCommitment(t, linkChild, map[string]any{
					"roles": map[string]any{}, "dtstart": "2099-05-02T09:00", "tz": "UTC",
				})
			},
			runPreview: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+linkParent.Slug+"/commitment/links",
					map[string]any{
						"child": linkChild.Slug, "parent_role": "target", "child_role": "target",
						"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 3600}, "preview": true,
					}, bearerHdr(ownerTok))
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+linkParent.Slug+"/commitment/links",
					map[string]any{
						"child": linkChild.Slug, "parent_role": "target", "child_role": "target",
						"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 3600},
					}, bearerHdr(ownerTok))
				if rr.Code == http.StatusCreated {
					var resp struct {
						Link struct {
							ID string `json:"id"`
						} `json:"link"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &resp); err == nil {
						createdLinkID = resp.Link.ID
					}
				}
				return rr
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+linkParent.Slug+"/commitment/links",
					map[string]any{
						"child": linkChild.Slug, "parent_role": "target", "child_role": "target",
						"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 3600},
					}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "PATCH /commitment-links/{linkID}",
			itemLevel:   true,
			expectItems: []string{linkParent.ID, linkChild.ID},
			wantOKCode:  http.StatusOK,
			runPreview: func(t *testing.T) *httptest.ResponseRecorder {
				if createdLinkID == "" {
					t.Fatal("missing createdLinkID from prior POST /commitment/links row")
				}
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/commitment-links/"+createdLinkID,
					map[string]any{"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 7200}, "preview": true}, bearerHdr(ownerTok))
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				if createdLinkID == "" {
					t.Fatal("missing createdLinkID from prior POST /commitment/links row")
				}
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/commitment-links/"+createdLinkID,
					map[string]any{"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 7200}}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/commitment-links/"+createdLinkID,
					map[string]any{"offset": map[string]any{"mode": "elapsed", "elapsed_secs": 7200}}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "DELETE /commitment-links/{linkID}",
			itemLevel:   true,
			expectItems: []string{linkParent.ID, linkChild.ID},
			wantOKCode:  http.StatusNoContent,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				if createdLinkID == "" {
					t.Fatal("missing createdLinkID from prior POST /commitment/links row")
				}
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/commitment-links/"+createdLinkID,
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/commitment-links/"+createdLinkID,
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /commitment/gates",
			itemLevel:   true,
			expectItems: []string{gateBlocked.ID, gateBlocker.ID},
			wantOKCode:  http.StatusCreated,
			setup: func(t *testing.T) {
				mustCreateCommitment(t, gateBlocked, map[string]any{
					"roles": map[string]any{}, "dtstart": "2099-06-01T09:00", "tz": "UTC",
				})
				mustCreateCommitment(t, gateBlocker, map[string]any{
					"roles": map[string]any{}, "dtstart": "2099-05-30T09:00", "tz": "UTC",
				})
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+gateBlocked.Slug+"/commitment/gates",
					map[string]any{"blocker": gateBlocker.Slug}, bearerHdr(ownerTok))
				if rr.Code == http.StatusCreated {
					var gate struct {
						ID string `json:"id"`
					}
					if err := json.Unmarshal(rr.Body.Bytes(), &gate); err == nil {
						createdGateID = gate.ID
					}
				}
				return rr
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+gateBlocked.Slug+"/commitment/gates",
					map[string]any{"blocker": gateBlocker.Slug}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "DELETE /commitment-gates/{gateID}",
			itemLevel:   true,
			expectItems: []string{gateBlocked.ID, gateBlocker.ID},
			wantOKCode:  http.StatusNoContent,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				if createdGateID == "" {
					t.Fatal("missing createdGateID from prior POST /commitment/gates row")
				}
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/commitment-gates/"+createdGateID,
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/commitment-gates/"+createdGateID,
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "PUT /commitment/forecast",
			itemLevel:   true,
			expectItems: []string{milestoneItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				mustCreateCommitment(t, milestoneItem, map[string]any{
					"roles": map[string]any{}, "dtstart": "2099-07-01T09:00", "tz": "UTC",
				})
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/items/"+milestoneItem.Slug+"/commitment/forecast",
					map[string]any{"value": "2099-07-05", "source": "plan"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/items/"+milestoneItem.Slug+"/commitment/forecast",
					map[string]any{"value": "2099-07-05"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "DELETE /commitment/forecast",
			itemLevel:   true,
			expectItems: []string{milestoneItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+milestoneItem.Slug+"/commitment/forecast",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+milestoneItem.Slug+"/commitment/forecast",
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "PUT /commitment/children/{childRef}",
			itemLevel:   true,
			expectItems: []string{milestoneItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/items/"+milestoneItem.Slug+"/commitment/children/"+milestoneChild.Slug,
					map[string]any{"kind": "child"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/items/"+milestoneItem.Slug+"/commitment/children/"+milestoneChild.Slug,
					map[string]any{"kind": "child"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "DELETE /commitment/children/{childRef}",
			itemLevel:   true,
			expectItems: []string{milestoneItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+milestoneItem.Slug+"/commitment/children/"+milestoneChild.Slug,
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+milestoneItem.Slug+"/commitment/children/"+milestoneChild.Slug,
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "DELETE /commitment",
			itemLevel:   true,
			expectItems: []string{schedItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+schedItem.Slug+"/commitment",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+schedItem.Slug+"/commitment",
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /reminders",
			itemLevel:   true,
			expectItems: []string{remItem.ID},
			wantOKCode:  http.StatusCreated,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+remItem.Slug+"/reminders",
					map[string]any{"remind_at": "2099-08-01T09:00:00Z"}, bearerHdr(ownerTok))
				if rr.Code == http.StatusCreated {
					var rem models.Reminder
					if err := json.Unmarshal(rr.Body.Bytes(), &rem); err == nil {
						createdReminderID = rem.ID
					}
				}
				return rr
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+remItem.Slug+"/reminders",
					map[string]any{"remind_at": "2099-08-01T09:00:00Z"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "PATCH /reminders/{reminderID}",
			itemLevel:   true,
			expectItems: []string{remItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				if createdReminderID == "" {
					t.Fatal("missing createdReminderID from prior POST /reminders row")
				}
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/reminders/"+createdReminderID,
					map[string]any{"remind_at": "2020-01-01T09:00:00Z"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPatch, wsBase+"/reminders/"+createdReminderID,
					map[string]any{"remind_at": "2099-08-02T09:00:00Z"}, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /reminders/ack",
			itemLevel:   true,
			expectItems: []string{remItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				srv.runReminderTick()
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+remItem.Slug+"/reminders/ack",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+remItem.Slug+"/reminders/ack",
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "POST /reminders/{reminderID}/ack",
			itemLevel:   true,
			expectItems: []string{remAckItem.ID},
			wantOKCode:  http.StatusOK,
			setup: func(t *testing.T) {
				rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+remAckItem.Slug+"/reminders",
					map[string]any{"remind_at": "2020-01-01T09:00:00Z"}, bearerHdr(ownerTok))
				if rr.Code != http.StatusCreated {
					t.Fatalf("setup arm reminder for id ack: %d %s", rr.Code, rr.Body.String())
				}
				var rem models.Reminder
				if err := json.Unmarshal(rr.Body.Bytes(), &rem); err != nil {
					t.Fatalf("unmarshal reminder: %v", err)
				}
				ackByIDReminderID = rem.ID
				srv.runReminderTick()
			},
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/reminders/"+ackByIDReminderID+"/ack",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPost, wsBase+"/reminders/"+ackByIDReminderID+"/ack",
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "DELETE /reminders/{reminderID}",
			itemLevel:   true,
			expectItems: []string{remItem.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				if createdReminderID == "" {
					t.Fatal("missing createdReminderID from prior POST /reminders row")
				}
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/reminders/"+createdReminderID,
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/reminders/"+createdReminderID,
					nil, bearerHdr(viewerTok))
			},
		},
		{
			name:        "needs_snooze",
			itemLevel:   true,
			expectItems: []string{item.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/items/"+item.Ref+"/needs/snooze",
					map[string]any{"kind": "dismiss"}, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodPut, wsBase+"/items/NOPE-9999/needs/snooze",
					map[string]any{"kind": "dismiss"}, bearerHdr(ownerTok))
			},
		},
		{
			name:        "needs_unsnooze",
			itemLevel:   true,
			expectItems: []string{item.ID},
			wantOKCode:  http.StatusOK,
			runOK: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/"+item.Ref+"/needs/snooze",
					nil, bearerHdr(ownerTok))
			},
			runRefused: func(t *testing.T) *httptest.ResponseRecorder {
				return doRequestWithHeaders(srv, http.MethodDelete, wsBase+"/items/NOPE-9999/needs/snooze",
					nil, bearerHdr(ownerTok))
			},
		},
	}

	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup(t)
			}
			// 1. Refused request must publish NO home_changed.
			beforeRefused := latestBusID()
			rrRef := tc.runRefused(t)
			if rrRef.Code < 400 {
				t.Fatalf("refused request unexpectedly succeeded: %d %s", rrRef.Code, rrRef.Body.String())
			}
			if hints := homeHintsSince(beforeRefused); len(hints) != 0 {
				t.Fatalf("refused request published %d home_changed hint(s): %+v", len(hints), hints)
			}

			// 1b. Preview request (when present) must succeed and publish NO home_changed.
			if tc.runPreview != nil {
				beforePrev := latestBusID()
				rrPrev := tc.runPreview(t)
				if rrPrev.Code != http.StatusOK {
					t.Fatalf("preview request got %d %s, want 200", rrPrev.Code, rrPrev.Body.String())
				}
				if hints := homeHintsSince(beforePrev); len(hints) != 0 {
					t.Fatalf("preview request published %d home_changed hint(s): %+v", len(hints), hints)
				}
			}

			// 2. Successful request must publish >= 1 home_changed with right workspace (and item(s) when itemLevel).
			beforeOK := latestBusID()
			rrOK := tc.runOK(t)
			if rrOK.Code != tc.wantOKCode {
				t.Fatalf("ok request got %d %s, want %d", rrOK.Code, rrOK.Body.String(), tc.wantOKCode)
			}
			hints := homeHintsSince(beforeOK)
			if len(hints) == 0 {
				t.Fatalf("successful request published 0 home_changed hints")
			}
			seenItems := make(map[string]bool, len(hints))
			for _, h := range hints {
				if h.WorkspaceID != ws.ID {
					t.Errorf("hint.WorkspaceID = %q, want %q", h.WorkspaceID, ws.ID)
				}
				if tc.itemLevel {
					if h.ItemID == "" {
						t.Errorf("item-level producer published empty ItemID: %+v", h)
					}
					if h.CollectionID == "" {
						t.Errorf("item-level producer published empty CollectionID: %+v", h)
					}
					seenItems[h.ItemID] = true
				} else {
					if h.ItemID != "" {
						t.Errorf("workspace-level producer published non-empty ItemID %q", h.ItemID)
					}
				}
			}
			for _, wantID := range tc.expectItems {
				if !seenItems[wantID] {
					t.Errorf("expected home_changed hint for item %q; got hints=%+v", wantID, hints)
				}
			}
		})
	}

	// Verify pad_home_hints_total metric was incremented.
	if srv.metrics != nil {
		if got := counterValue(t, srv.metrics.HomeHintsTotal); got < float64(len(rows)) {
			t.Errorf("pad_home_hints_total = %v, want >= %d", got, len(rows))
		}
	}
}

type fakeHomeHintTimer struct {
	mu      sync.Mutex
	d       time.Duration
	fn      func()
	stopped bool
	fired   bool
}

func (ft *fakeHomeHintTimer) Stop() bool {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.stopped || ft.fired {
		return false
	}
	ft.stopped = true
	return true
}

func (ft *fakeHomeHintTimer) fire() {
	ft.mu.Lock()
	if ft.stopped || ft.fired {
		ft.mu.Unlock()
		return
	}
	ft.fired = true
	fn := ft.fn
	ft.mu.Unlock()
	fn()
}

type fakeHomeHintScheduler struct {
	mu     sync.Mutex
	timers []*fakeHomeHintTimer
}

func (s *fakeHomeHintScheduler) afterFunc(d time.Duration, f func()) homeHintTimer {
	ft := &fakeHomeHintTimer{d: d, fn: f}
	s.mu.Lock()
	s.timers = append(s.timers, ft)
	s.mu.Unlock()
	return ft
}

func (s *fakeHomeHintScheduler) fireAll() int {
	s.mu.Lock()
	batch := append([]*fakeHomeHintTimer(nil), s.timers...)
	s.timers = nil
	s.mu.Unlock()
	fired := 0
	for _, ft := range batch {
		ft.fire()
		fired++
	}
	return fired
}

func (s *fakeHomeHintScheduler) pendingDurations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]time.Duration, len(s.timers))
	for i, ft := range s.timers {
		out[i] = ft.d
	}
	return out
}

func TestHomeHint_CoalescesBurstPerKey(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	sched := &fakeHomeHintScheduler{}
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{afterFunc: sched.afterFunc}

	wsID := "ws-burst"
	collID := "coll-burst"
	itemA := "item-a"
	itemB := "item-b"

	countFor := func(itemID string) int {
		n := 0
		for _, ev := range srv.watchEvents.EventsSince(0) {
			if ev.Kind == watchevents.KindHomeChanged && ev.WorkspaceID == wsID && ev.CollectionID == collID && ev.ItemID == itemID {
				n++
			}
		}
		return n
	}

	// Burst 10 hints for itemA inside one window, plus 1 hint for itemB (independent key).
	for i := 0; i < 10; i++ {
		srv.publishHomeChanged(wsID, collID, itemA)
	}
	srv.publishHomeChanged(wsID, collID, itemB)

	if got := countFor(itemA); got != 1 {
		t.Fatalf("immediate hints for itemA = %d, want 1", got)
	}
	if got := countFor(itemB); got != 1 {
		t.Fatalf("immediate hints for itemB = %d, want 1", got)
	}
	durs := sched.pendingDurations()
	if len(durs) != 2 || durs[0] != defaultHomeHintCoalesceWindow || durs[1] != defaultHomeHintCoalesceWindow {
		t.Fatalf("scheduled window durations = %v, want two %v timers", durs, defaultHomeHintCoalesceWindow)
	}

	// Close first window: itemA emits 1 trailing hint and opens a new window;
	// itemB had nothing pending so its entry is evicted.
	sched.fireAll()
	if got := countFor(itemA); got != 2 {
		t.Fatalf("total hints for itemA after first window = %d, want 2 (1 immediate + 1 trailing)", got)
	}
	if got := countFor(itemB); got != 1 {
		t.Fatalf("total hints for itemB after first window = %d, want 1", got)
	}

	// Trailing publish opened a new window for itemA: a burst during it is
	// suppressed until that new window ends.
	for i := 0; i < 4; i++ {
		srv.publishHomeChanged(wsID, collID, itemA)
	}
	if got := countFor(itemA); got != 2 {
		t.Fatalf("hints for itemA during trailing window = %d, want 2", got)
	}
	sched.fireAll()
	if got := countFor(itemA); got != 3 {
		t.Fatalf("hints for itemA after trailing window = %d, want 3", got)
	}

	// Close quiet window: no extra hint, and map is cleaned up.
	sched.fireAll()
	if got := countFor(itemA); got != 3 {
		t.Fatalf("hints for itemA after quiet window = %d, want 3", got)
	}
	srv.homeHintMu.Lock()
	remaining := len(srv.homeHintEntries)
	srv.homeHintMu.Unlock()
	if remaining != 0 {
		t.Fatalf("homeHintEntries len = %d after quiet windows, want 0", remaining)
	}
}

func TestHomeHint_TrailingAfterLastWrite(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	sched := &fakeHomeHintScheduler{}
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{afterFunc: sched.afterFunc}

	wsID := "ws-trail"
	collID := "coll-trail"
	itemID := "item-trail"

	hintsFor := func() []watchevents.Notification {
		var out []watchevents.Notification
		for _, ev := range srv.watchEvents.EventsSince(0) {
			if ev.Kind == watchevents.KindHomeChanged && ev.WorkspaceID == wsID && ev.CollectionID == collID && ev.ItemID == itemID {
				out = append(out, ev)
			}
		}
		return out
	}

	// First write publishes immediately.
	srv.publishHomeChanged(wsID, collID, itemID)
	if got := len(hintsFor()); got != 1 {
		t.Fatalf("after first write: %d hints, want 1", got)
	}

	// Subsequent writes inside the window are coalesced.
	for i := 0; i < 5; i++ {
		srv.publishHomeChanged(wsID, collID, itemID)
	}
	afterLastWrite := time.Now().UTC().UnixMilli()
	if got := len(hintsFor()); got != 1 {
		t.Fatalf("during burst inside window: %d hints, want 1 (further hints must wait for trailing edge)", got)
	}

	// Publish a sentinel event on the bus AFTER the last write call so we can
	// prove ordering strictly by monotonic bus ID (not just wall clock).
	srv.publishHomeChanged(wsID, collID, "sentinel-after-last-write")
	var sentinelID int64
	for _, ev := range srv.watchEvents.EventsSince(0) {
		if ev.ItemID == "sentinel-after-last-write" {
			sentinelID = ev.ID
		}
	}
	if sentinelID == 0 {
		t.Fatal("sentinel event was not published")
	}

	// Close the window: trailing hint must be published strictly after the last write.
	sched.fireAll()
	got := hintsFor()
	if len(got) != 2 {
		t.Fatalf("after window: %d hints, want 2", len(got))
	}
	trailing := got[1]
	if trailing.ID <= sentinelID {
		t.Errorf("trailing hint ID %d <= sentinel ID %d; trailing hint must be published after the last write", trailing.ID, sentinelID)
	}
	if trailing.Timestamp < afterLastWrite {
		t.Errorf("trailing hint timestamp %d < afterLastWrite %d", trailing.Timestamp, afterLastWrite)
	}
}

func TestHomeHint_LeaseRenewalAndNoopReleasePublishNone(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	// Bypass coalescing so we test the lease handler's own suppression directly.
	srv.homeHintCoalesceForTesting = &homeHintCoalesceConfig{disabled: true}

	owner := mkAgentsUser(t, srv, "lease-hint-owner@example.com", "Lease Owner")
	ownerTok := mkAgentsPAT(t, srv, owner.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Lease WS", Slug: "lease-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member: %v", err)
	}
	coll, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "LSE", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("coll: %v", err)
	}
	item, err := srv.store.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "Lease Item", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	item, _ = srv.store.GetItem(item.ID)

	wsBase := "/api/v1/workspaces/" + ws.Slug

	latestBusID := func() int64 {
		evs := srv.watchEvents.EventsSince(0)
		if len(evs) == 0 {
			return 0
		}
		return evs[len(evs)-1].ID
	}
	homeHintsSince := func(sinceID int64) []watchevents.Notification {
		var out []watchevents.Notification
		for _, n := range srv.watchEvents.EventsSince(sinceID) {
			if n.Kind == watchevents.KindHomeChanged {
				out = append(out, n)
			}
		}
		return out
	}

	// 1. Initial claim (unclaimed -> held): publishes 1 hint.
	before := latestBusID()
	rr := doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/claim",
		map[string]any{"holder": "agent-1"}, bearerHdr(ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("initial claim: %d %s", rr.Code, rr.Body.String())
	}
	if hints := homeHintsSince(before); len(hints) != 1 {
		t.Fatalf("initial claim published %d hints, want 1", len(hints))
	}

	// 2. Renewal / extension by the same holder while live: publishes 0 hints.
	before = latestBusID()
	rr = doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/claim",
		map[string]any{"holder": "agent-1", "ttl_seconds": 1800}, bearerHdr(ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("renewal claim: %d %s", rr.Code, rr.Body.String())
	}
	if hints := homeHintsSince(before); len(hints) != 0 {
		t.Fatalf("lease renewal published %d hint(s), want 0: %+v", len(hints), hints)
	}

	// 3. Release of held lease (released=true): publishes 1 hint.
	before = latestBusID()
	rr = doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/release",
		map[string]any{"holder": "agent-1"}, bearerHdr(ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("release: %d %s", rr.Code, rr.Body.String())
	}
	if hints := homeHintsSince(before); len(hints) != 1 {
		t.Fatalf("active release published %d hints, want 1", len(hints))
	}

	// 4. Second release when already released (released=false): publishes 0 hints.
	before = latestBusID()
	rr = doRequestWithHeaders(srv, http.MethodPost, wsBase+"/items/"+item.Ref+"/release",
		map[string]any{"holder": "agent-1"}, bearerHdr(ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("noop release: %d %s", rr.Code, rr.Body.String())
	}
	if hints := homeHintsSince(before); len(hints) != 0 {
		t.Fatalf("noop release published %d hint(s), want 0: %+v", len(hints), hints)
	}
}

func TestHomeHint_MoveNotifiesSourceCollectionViewer(t *testing.T) {
	t.Parallel()
	srv := testServerWithWatchEvents(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	owner := mkAgentsUser(t, srv, "move-owner@example.com", "Move Owner")
	sourceOnly := mkAgentsUser(t, srv, "move-source-only@example.com", "Source Only")
	ownerTok := mkAgentsPAT(t, srv, owner.ID, "")
	sourceOnlyTok := mkAgentsPAT(t, srv, sourceOnly.ID, "")

	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: "Move WS", Slug: "move-ws", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("ws: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("member owner: %v", err)
	}
	if err := srv.store.AddWorkspaceMember(ws.ID, sourceOnly.ID, "viewer"); err != nil {
		t.Fatalf("member sourceOnly: %v", err)
	}
	srcColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Source", Slug: "source", Prefix: "SRC", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("srcColl: %v", err)
	}
	dstColl, err := srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Target", Slug: "target", Prefix: "DST", Schema: meAgentsStdSchema})
	if err != nil {
		t.Fatalf("dstColl: %v", err)
	}
	if err := srv.store.SetMemberCollectionAccess(ws.ID, sourceOnly.ID, "specific", []string{srcColl.ID}); err != nil {
		t.Fatalf("SetMemberCollectionAccess: %v", err)
	}

	it, err := srv.store.CreateItem(ws.ID, srcColl.ID, models.ItemCreate{Title: "Leaving Item", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatalf("item: %v", err)
	}
	it, _ = srv.store.GetItem(it.ID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chSourceOnly := connectWatchStreamWithHeadersAndQuery(ctx, t, ts.URL, sourceOnlyTok, nil, "home=true")
	waitForWatchEvent(t, chSourceOnly, 3*time.Second)

	rr := doRequestWithHeaders(srv, http.MethodPost, "/api/v1/workspaces/"+ws.Slug+"/items/"+it.Slug+"/move",
		map[string]any{"target_collection": dstColl.Slug}, bearerHdr(ownerTok))
	if rr.Code != http.StatusOK {
		t.Fatalf("move item: %d %s", rr.Code, rr.Body.String())
	}

	got := waitForHomeHint(t, chSourceOnly, 2*time.Second)
	if got["workspace"] != ws.Slug || got["kind"] != watchevents.KindHomeChanged {
		t.Errorf("source-only viewer hint = %+v, want workspace=%s kind=home_changed", got, ws.Slug)
	}
}
