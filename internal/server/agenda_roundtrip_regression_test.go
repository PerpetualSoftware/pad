package server

import (
	"encoding/json"
	"net/http/httptest"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

func TestAgendaEndedScheduleRemainsUnplaced(t *testing.T) {
	for _, absent := range []string{"ended", "no_wall", "no_instant"} {
		t.Run(absent, func(t *testing.T) {
			w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
			ended := w.sched(t, w.owner, "Unplaced", "2031-03-10T09:00", "both")
			live := w.sched(t, w.owner, "Live", "2031-03-10T10:00", "both")
			assignment := map[string]string{"ended": "ended_at='2031-03-10T11:00:00Z',next_at=NULL,next_wall=NULL", "no_wall": "next_wall=NULL", "no_instant": "next_at=NULL"}[absent]
			meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET `+assignment+` WHERE item_id=?`, ended.ID)
			for _, personal := range []bool{false, true} {
				run := w.srv.newAgendaRead(meAgendaNow, meAgendaNow.Add(time.Hour), usertz.Resolve(nil), "")
				run.listNoDate = personal
				if err := w.srv.appendAgendaWork(run, w.ws.ID, store.PendingReminderScope{}); err != nil {
					t.Fatal(err)
				}
				a := run.response
				if len(a.PriorityList) != 1 || a.PriorityList[0].ItemRef != live.Ref {
					t.Fatalf("live priority lost: %+v", a)
				}
				if personal {
					if len(a.NoDate) != 1 || a.NoDate[0].ItemRef != ended.Ref || a.NoDate[0].Reason != "unplaced" || a.NoDate[0].ScheduleID != w.scheduleID(t, ended.ID) || !a.NoDateComplete {
						t.Errorf("unplaced schedule lost: %+v", a)
					}
				} else if a.PriorityListComplete {
					t.Error("unlisted unplaced schedule claimed complete")
				}
			}
		})
	}
}

func TestAgendaGroupReadErrorPreservesWindow(t *testing.T) {
	for _, source := range []string{"candidates", "details"} {
		t.Run(source, func(t *testing.T) {
			w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
			today := w.sched(t, w.owner, "Today", "2031-03-10T09:00", "both")
			carried := w.sched(t, w.owner, "Carried", "2031-03-09T09:00", "both")
			bad := w.sched(t, w.owner, "Future", "2031-04-10T09:00", "both")
			// Inject a scan error only in the secondary read. This ended schedule
			// has no history in the window and is excluded from due/carried reads.
			meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET ended_at='2031-03-01T00:00:00Z',next_at=NULL,next_wall=NULL WHERE item_id=?`, bad.ID)
			if source == "candidates" {
				meAgendaExec(t, w.agendaWorld, `UPDATE items SET item_number='bad' WHERE id=?`, bad.ID)
			} else {
				meAgendaExec(t, w.agendaWorld, `UPDATE item_schedules SET lead_minutes=0.5 WHERE item_id=?`, bad.ID)
			}
			req := httptest.NewRequest("GET", "/", nil)
			a, err := w.srv.buildBootstrapAgenda(w.ws.ID, w.owner, req, store.PendingReminderScope{})
			if err != nil || a == nil {
				t.Errorf("bootstrap discarded its window: agenda=%+v err=%v", a, err)
			} else if len(a.DueToday) != 1 || a.DueToday[0].Ref != today.Ref || len(a.Carried) != 1 || a.Carried[0].Ref != carried.Ref || a.PriorityListComplete || a.WaitingOnComplete || a.NoDateComplete {
				t.Errorf("bootstrap partial read: %+v", a)
			}
			for _, door := range []string{"workspace", "me"} {
				var status int
				var body map[string]any
				if door == "workspace" {
					status, body = w.get(t, w.owner, agendaWindow)
				} else {
					rr := meAgendaGet(t, w.agendaWorld, w.owner, agendaWindow)
					status = rr.Code
					_ = json.Unmarshal(rr.Body.Bytes(), &body)
				}
				if status != 200 || !strings.Contains(seen(body), "schedule:Today") || body["window_complete"] != true || body["priority_list_complete"] != false || body["waiting_on_complete"] != false || body["no_date_complete"] != false {
					t.Errorf("%s lost window: status=%d body=%v", door, status, body)
				}
			}
		})
	}
}

func TestAgendaGateReadErrorContinuesCandidates(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	items := []*models.Item{
		w.item(t, "tasks", "First", `{}`, nil),
		w.item(t, "tasks", "Second", `{}`, nil),
		w.item(t, "tasks", "Third", `{}`, nil),
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	blocked, blocker, good := items[0], items[1], items[2]
	access := models.CommitmentAccess{Readable: map[string]bool{blocked.ID: true, blocker.ID: true}, Editable: map[string]bool{blocked.ID: true, blocker.ID: true}}
	if _, err := w.srv.store.CreateScheduleGate(w.ws.ID, blocked.ID, blocker.ID, access); err != nil {
		t.Fatal(err)
	}
	// Keep gate presence readable but inject a NULL scan failure in gate details.
	meAgendaExec(t, w.agendaWorld, `ALTER TABLE schedule_gates RENAME TO agenda_test_gates`)
	meAgendaExec(t, w.agendaWorld, `CREATE VIEW schedule_gates AS SELECT id,workspace_id,blocked_item_id,blocker_item_id,blocker_schedule_id,blocker_occurrence_key,opened_at,NULL AS created_at FROM agenda_test_gates`)
	run := w.srv.newAgendaRead(meAgendaNow, meAgendaNow.Add(time.Hour), usertz.Resolve(nil), "")
	if err := w.srv.appendAgendaWork(run, w.ws.ID, store.PendingReminderScope{}); err != nil {
		t.Fatalf("gate error escaped: %v", err)
	}
	found := false
	for _, e := range run.response.NoDate {
		if e.ItemRef == good.Ref {
			found = true
		}
	}
	if !found || run.response.WaitingOnComplete || !run.response.PriorityListComplete || !run.response.NoDateComplete {
		t.Fatalf("gate failure stopped read or cut unrelated groups: %+v", run.response)
	}
}

func TestMeAgendaWindowsPrecedeGroups(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	other := meAgendaWorkspace(t, w.agendaWorld, "Other")
	for _, world := range []*agendaWorld{w.agendaWorld, other} {
		it := world.item(t, "tasks", "Window", `{"due_date":"2031-03-10"}`, nil)
		meAgendaExec(t, world, `UPDATE items SET assigned_user_id=?,created_at='2031-03-10T01:00:00Z' WHERE id=?`, w.owner.ID, it.ID)
	}
	// Both passes stamp workspace identity before groups run out of time.
	full := meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow)
	groups := full["no_date"].([]any)
	if len(groups) != 2 {
		t.Fatalf("workspace groups lost: %v", full)
	}
	for _, raw := range groups {
		e := raw.(map[string]any)
		if e["workspace"] == "" || e["workspace_name"] == "" || e["owner_username"] != w.owner.Username {
			t.Fatalf("group identity lost: %v", e)
		}
	}
	expired := false
	w.srv.agendaClock = func() time.Time {
		if strings.Contains(string(debug.Stack()), "(*Server).appendAgendaWork(") {
			expired = true
		}
		if expired {
			return meAgendaNow.Add(2 * time.Second)
		}
		return meAgendaNow
	}
	a := meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow)
	if a["window_complete"] != true || a["counts_truncated"] == true || len(entriesOf(a)) != 4 || a["priority_list_complete"] != false || a["waiting_on_complete"] != false || a["no_date_complete"] != false {
		t.Fatalf("groups spent later window's budget: %v", a)
	}
	for _, e := range entriesOf(a) {
		if e["workspace"] == "" || e["owner_username"] != w.owner.Username {
			t.Errorf("window identity lost: %v", e)
		}
	}
}

func TestMeAgendaGroupsBelongToCaller(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	other := mustUser(t, w.srv, "other-personal@example.com", "otherpersonal", "")
	for _, scheduled := range []bool{false, true} {
		for _, who := range []string{"own assigned", "own unassigned", "other assigned", "other unassigned"} {
			var sc *models.ScheduleCreate
			if scheduled {
				sc = oneShot()
			}
			it := w.item(t, "tasks", who, `{}`, sc)
			id := w.owner.ID
			if strings.HasPrefix(who, "other") {
				id = other.ID
			}
			if strings.HasSuffix(who, "unassigned") {
				meAgendaExec(t, w.agendaWorld, `UPDATE items SET created_by_user_id=? WHERE id=?`, id, it.ID)
			} else {
				meAgendaExec(t, w.agendaWorld, `UPDATE items SET assigned_user_id=? WHERE id=?`, id, it.ID)
			}
		}
	}
	agent := w.sched(t, w.owner, "Agent only", "2031-03-10T09:00", "agent")
	a := meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow)
	for _, group := range []string{"no_date", "priority_list"} {
		rows := a[group].([]any)
		if len(rows) != 2 {
			t.Errorf("%s includes someone else's work: %v", group, rows)
		}
		for _, raw := range rows {
			e := raw.(map[string]any)
			if !strings.HasPrefix(e["item_title"].(string), "own") || e["item_ref"] == agent.Ref || e["workspace"] != w.ws.Slug || e["owner_username"] != w.owner.Username {
				t.Errorf("wrong personal row: %v", e)
			}
		}
	}
}
