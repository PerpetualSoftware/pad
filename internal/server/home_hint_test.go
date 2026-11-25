package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
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

	type producerRow struct {
		name       string
		itemLevel  bool
		expectItem string // empty means any non-empty itemID when itemLevel=true
		setup      func(t *testing.T)
		runOK      func(t *testing.T) *httptest.ResponseRecorder
		wantOKCode int
		runRefused func(t *testing.T) *httptest.ResponseRecorder
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
			name:       "single_item_update",
			itemLevel:  true,
			expectItem: item.ID,
			wantOKCode: http.StatusOK,
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
			name:       "lease_claim",
			itemLevel:  true,
			expectItem: item.ID,
			wantOKCode: http.StatusOK,
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
			name:       "lease_release",
			itemLevel:  true,
			expectItem: item.ID,
			wantOKCode: http.StatusOK,
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
			name:       "schedule_create",
			itemLevel:  true,
			expectItem: schedItem.ID,
			wantOKCode: http.StatusCreated,
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
			name:       "schedule_update",
			itemLevel:  true,
			expectItem: schedItem.ID,
			wantOKCode: http.StatusOK,
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
			name:       "schedule_resolve_done_end",
			itemLevel:  true,
			expectItem: schedItem.ID,
			wantOKCode: http.StatusOK,
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
			name:       "schedule_reopen",
			itemLevel:  true,
			expectItem: schedItem.ID,
			wantOKCode: http.StatusOK,
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
			name:       "schedule_dismiss_ended",
			itemLevel:  true,
			expectItem: schedItem.ID,
			wantOKCode: http.StatusOK,
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
			name:       "schedule_delete",
			itemLevel:  true,
			expectItem: schedItem.ID,
			wantOKCode: http.StatusOK,
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
			name:       "needs_snooze",
			itemLevel:  true,
			expectItem: item.ID,
			wantOKCode: http.StatusOK,
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
			name:       "needs_unsnooze",
			itemLevel:  true,
			expectItem: item.ID,
			wantOKCode: http.StatusOK,
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

			// 2. Successful request must publish >= 1 home_changed with right workspace (and item when itemLevel).
			beforeOK := latestBusID()
			rrOK := tc.runOK(t)
			if rrOK.Code != tc.wantOKCode {
				t.Fatalf("ok request got %d %s, want %d", rrOK.Code, rrOK.Body.String(), tc.wantOKCode)
			}
			hints := homeHintsSince(beforeOK)
			if len(hints) == 0 {
				t.Fatalf("successful request published 0 home_changed hints")
			}
			for _, h := range hints {
				if h.WorkspaceID != ws.ID {
					t.Errorf("hint.WorkspaceID = %q, want %q", h.WorkspaceID, ws.ID)
				}
				if tc.itemLevel {
					if h.ItemID == "" {
						t.Errorf("item-level producer published empty ItemID: %+v", h)
					}
					if tc.expectItem != "" && h.ItemID != tc.expectItem {
						t.Errorf("hint.ItemID = %q, want %q", h.ItemID, tc.expectItem)
					}
					if h.CollectionID == "" {
						t.Errorf("item-level producer published empty CollectionID: %+v", h)
					}
				} else {
					if h.ItemID != "" {
						t.Errorf("workspace-level producer published non-empty ItemID %q", h.ItemID)
					}
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
