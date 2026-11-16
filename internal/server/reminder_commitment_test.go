package server

import (
	"encoding/json"
	"github.com/PerpetualSoftware/pad/internal/store"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestReminderCompatibilityRoutes(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Run once", "fields": `{"status":"open"}`})
	r := armViaAPI(t, srv, ws, item, pastInstant)
	if r.ScheduleID == nil || r.OccurrenceAt == nil {
		t.Fatalf("POST did not create commitment: %+v", r)
	}
	base := "/api/v1/workspaces/" + ws
	activityCount := func(action string) int {
		t.Helper()
		var n int
		if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM activities WHERE document_id=? AND action=?`, item.ID, action).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := activityCount("schedule_created"); n != 1 {
		t.Fatalf("creation activities=%d", n)
	}
	rr := doRequest(srv, "POST", base+"/items/"+item.ID+"/reminders", map[string]string{"remind_at": futureInstant})
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "schedule_exists") || !strings.Contains(rr.Body.String(), "pad remind move") {
		t.Fatalf("conflict=%d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "POST", base+"/reminders/"+r.ID+"/ack", nil)
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "reminder_not_fired") {
		t.Fatalf("armed ack=%d %s", rr.Code, rr.Body.String())
	}
	srv.runReminderTick()
	rr = doRequest(srv, "POST", base+"/items/"+item.ID+"/reminders/ack", nil)
	if rr.Code != 200 {
		t.Fatalf("ref ack=%d %s", rr.Code, rr.Body.String())
	}
	var ack models.Reminder
	parseJSON(t, rr, &ack)
	if ack.ID != r.ID || ack.AckedAt == nil {
		t.Fatal("ack lost identity")
	}
	rr = doRequest(srv, "POST", base+"/reminders/"+r.ID+"/ack", nil)
	if rr.Code != 200 {
		t.Fatalf("retry=%d %s", rr.Code, rr.Body.String())
	}
	if n := activityCount("schedule_acknowledged"); n != 1 {
		t.Fatalf("acknowledgement activities=%d", n)
	}
	rr = doRequest(srv, "POST", base+"/items/"+item.ID+"/reminders/ack", nil)
	if rr.Code != 404 {
		t.Fatalf("ref retry=%d", rr.Code)
	}
	rr = doRequest(srv, "PATCH", base+"/reminders/"+r.ID, map[string]string{"remind_at": pastInstant})
	if rr.Code != 200 {
		t.Fatalf("rearm=%d %s", rr.Code, rr.Body.String())
	}
	rr = doRequest(srv, "GET", base+"/items/"+item.ID+"/reminders", nil)
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), r.ID) {
		t.Fatal("list lost reminder")
	}
	rr = doRequest(srv, "DELETE", base+"/reminders/"+r.ID, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"deleted":true`) {
		t.Fatalf("delete=%d %s", rr.Code, rr.Body.String())
	}
	sc, err := srv.store.GetSchedule(item.WorkspaceID, item.ID)
	if err != nil || sc != nil {
		t.Fatalf("delete left commitment: %v %v", sc, err)
	}
}

func TestNativePastCommitmentPostStillRefused(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Native past"})
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/items/"+item.ID+"/commitment", map[string]any{"dtstart": "2020-01-01T00:00:00", "tz": "UTC", "roles": map[string]int{"target_days": 0, "target_secs": 0}})
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "exhausted") {
		t.Fatalf("native past=%d %s", rr.Code, rr.Body.String())
	}
}

func TestOneShotReminderNextReady(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Acknowledged work", "fields": `{"status":"in-progress","priority":"high"}`})
	other := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Ordinary work", "fields": `{"status":"in-progress","priority":"high"}`})
	r := armViaAPI(t, srv, ws, item, futureInstant)
	has := func(title string) bool {
		for _, v := range getDashboard(t, srv, ws).SuggestedNext {
			if v.ItemTitle == title {
				return true
			}
		}
		return false
	}
	if !has(item.Title) || !has(other.Title) {
		t.Fatal("fixture lacks ordinary suggestions")
	}
	base := "/api/v1/workspaces/" + ws + "/reminders/" + r.ID
	rr := doRequest(srv, "PATCH", base, map[string]string{"remind_at": pastInstant})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	srv.runReminderTick()
	if len(getDashboard(t, srv, ws).PendingReminders) != 1 {
		t.Fatal("no pending reminder")
	}
	rr = doRequest(srv, "POST", base+"/ack", nil)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if has(item.Title) || !has(other.Title) || len(getDashboard(t, srv, ws).PendingReminders) != 0 {
		t.Fatal("ack did not suppress only acknowledged work")
	}
	rr = doRequest(srv, "PATCH", base, map[string]string{"remind_at": futureInstant})
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if !has(item.Title) {
		t.Fatal("rearm did not clear suppression")
	}
	rr = doRequest(srv, "DELETE", base, nil)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	if !has(item.Title) {
		t.Fatal("delete retained suppression")
	}
}

func TestReminderPostReusesEndedSimpleCommitment(t *testing.T) {
	for _, direction := range []string{"ungated", "blocked", "blocker"} {
		t.Run(direction, func(t *testing.T) {
			srv := testServer(t)
			ws := createWSWithCollections(t, srv)
			item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Ended reminder", "fields": `{"status":"open"}`})
			r := armViaAPI(t, srv, ws, item, futureInstant)
			if _, err := srv.store.DB().Exec(`DELETE FROM item_reminders WHERE id=?`, r.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := srv.store.DB().Exec(`UPDATE item_schedules SET ended_at=?,next_at=NULL,next_wall=NULL WHERE id=?`, pastInstant, *r.ScheduleID); err != nil {
				t.Fatal(err)
			}
			if direction != "ungated" {
				other := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Other", "fields": `{"status":"open"}`})
				armViaAPI(t, srv, ws, other, futureInstant)
				blocked, blocker := item.ID, other.ID
				if direction == "blocker" {
					blocked, blocker = blocker, blocked
				}
				access := models.CommitmentAccess{Readable: map[string]bool{item.ID: true, other.ID: true}, Editable: map[string]bool{item.ID: true, other.ID: true}}
				if _, err := srv.store.CreateScheduleGate(item.WorkspaceID, blocked, blocker, access); err != nil {
					t.Fatal(err)
				}
			}
			base := "/api/v1/workspaces/" + ws + "/items/" + item.ID
			rr := doRequest(srv, "POST", base+"/commitment", map[string]any{"dtstart": "2099-01-01T00:00:00", "tz": "UTC", "roles": map[string]int{"target_days": 0, "target_secs": 0}})
			if rr.Code != 409 || !strings.Contains(rr.Body.String(), "schedule_exists") {
				t.Fatalf("native=%d %s", rr.Code, rr.Body.String())
			}
			got := armViaAPI(t, srv, ws, item, futureInstant)
			if got.ScheduleID == nil || *got.ScheduleID != *r.ScheduleID || got.ID == r.ID {
				t.Fatalf("reuse=%+v", got)
			}
			if direction != "ungated" {
				alarmPath := "/api/v1/workspaces/" + ws + "/reminders/" + got.ID
				for _, op := range []struct{ method, suffix string }{{"PATCH", ""}, {"POST", "/ack"}, {"DELETE", ""}} {
					rr := doRequest(srv, op.method, alarmPath+op.suffix, map[string]string{"remind_at": futureInstant})
					if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"code":"reminder_managed"`) {
						t.Fatalf("%s %s: %d %s", op.method, op.suffix, rr.Code, rr.Body.String())
					}
				}
				return
			}
			rr = doRequest(srv, "DELETE", base+"/commitment", nil)
			if rr.Code != 200 {
				t.Fatalf("delete=%d %s", rr.Code, rr.Body.String())
			}
			fresh := armViaAPI(t, srv, ws, item, futureInstant)
			if *fresh.ScheduleID == *r.ScheduleID {
				t.Fatal("fresh create reused deleted schedule")
			}
		})
	}
}

func TestReminderCreateActivity(t *testing.T) {
	for _, reuse := range []bool{false, true} {
		name, wantAction := "fresh", "schedule_created"
		var wantFields []string
		if reuse {
			name, wantAction, wantFields = "reuse", "schedule_updated", []string{"dtstart"}
		}
		t.Run(name, func(t *testing.T) {
			srv := testServer(t)
			ws := createWSWithCollections(t, srv)
			item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Reminder activity"})
			var scheduleID string
			if reuse {
				r, err := srv.store.CreateReminder(item.WorkspaceID, item.ID, futureInstant)
				if err != nil {
					t.Fatal(err)
				}
				scheduleID = *r.ScheduleID
				if _, err := srv.store.DB().Exec(`DELETE FROM item_reminders WHERE id=?`, r.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := srv.store.DB().Exec(`UPDATE item_schedules SET ended_at=?,next_at=NULL,next_wall=NULL WHERE id=?`, pastInstant, scheduleID); err != nil {
					t.Fatal(err)
				}
			}
			r := armViaAPI(t, srv, ws, item, futureInstant)
			if r.ID == "" || r.ScheduleID == nil || r.RemindAt != futureInstant || (reuse && *r.ScheduleID != scheduleID) {
				t.Fatalf("flat Reminder response: %+v", r)
			}
			var count int
			if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM activities WHERE document_id=? AND action IN ('schedule_created','schedule_updated')`, item.ID).Scan(&count); err != nil || count != 1 {
				t.Fatalf("schedule activities=%d, err=%v", count, err)
			}
			var action, metadata string
			if err := srv.store.DB().QueryRow(`SELECT action,metadata FROM activities WHERE document_id=? AND action IN ('schedule_created','schedule_updated')`, item.ID).Scan(&action, &metadata); err != nil {
				t.Fatal(err)
			}
			var meta struct {
				Kind   string   `json:"kind"`
				Fields []string `json:"fields"`
			}
			if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
				t.Fatal(err)
			}
			if action != wantAction || meta.Kind != "check" || !reflect.DeepEqual(meta.Fields, wantFields) {
				t.Fatalf("activity=%s %s, want %s fields=%v", action, metadata, wantAction, wantFields)
			}
		})
	}
}

func TestLegacyAcknowledgedReminderImportStaysDismissed(t *testing.T) {
	for _, kind := range []string{"acked", "unreadable"} {
		t.Run(kind, func(t *testing.T) {
			srv := testServer(t)
			ws := createWSWithCollections(t, srv)
			item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Dismissed reminder", "fields": `{"status":"open"}`})
			data, err := srv.store.ExportWorkspace(ws)
			if err != nil {
				t.Fatal(err)
			}
			data.Reminders = []models.ReminderExport{{ItemID: item.ID, RemindAt: pastInstant, FiredAt: pastInstant, AckedAt: pastInstant, CreatedAt: pastInstant, UpdatedAt: pastInstant}}
			if kind == "unreadable" {
				data.Reminders[0].RemindAt = "unreadable"
			}
			imported, report, err := srv.store.ImportWorkspaceWithReport(data, "Dismissed import", "", "test")
			if err != nil || len(report.DroppedSchedules) != 0 {
				t.Fatalf("import: %+v %v", report, err)
			}
			itemID := report.ItemIDs[item.ID]
			base := "/api/v1/workspaces/" + imported.Slug + "/items/" + itemID
			rr := doRequest(srv, "GET", base+"/reminders", nil)
			var listed struct {
				Reminders []models.Reminder `json:"reminders"`
			}
			parseJSON(t, rr, &listed)
			if rr.Code != http.StatusOK || len(listed.Reminders) != 0 {
				t.Fatalf("dismissed reminder listed: %d %s", rr.Code, rr.Body.String())
			}
			sc, err := srv.store.GetSchedule(imported.ID, itemID)
			if err != nil || sc == nil || sc.EndedAt == nil || sc.ReminderID != nil {
				t.Fatalf("carrier: %+v %v", sc, err)
			}

			from := time.Now().UTC().Truncate(24 * time.Hour)
			to := from.Add(24 * time.Hour)
			run := &agendaRead{now: time.Now(), deadline: time.Now().Add(time.Minute), loc: time.UTC, workRemaining: 100, listNoDate: true}
			srv.appendAgendaWork(run, imported.ID, store.PendingReminderScope{})
			if len(run.response.PriorityList) != 0 || len(run.response.WaitingOn) != 0 || len(run.response.NoDate) != 1 || run.response.NoDate[0].Reason != "unplaced" {
				t.Errorf("carrier agenda work: %+v", run.response)
			}
			for _, path := range []string{"/agenda", "/collections/tasks/calendar"} {
				rr := doRequest(srv, "GET", "/api/v1/workspaces/"+imported.Slug+path+"?date_field=due_date&from="+from.Format(time.RFC3339)+"&to="+to.Format(time.RFC3339), nil)
				var result map[string]any
				parseJSON(t, rr, &result)
				if rr.Code != http.StatusOK || len(entriesOf(result)) != 0 {
					t.Errorf("carrier %s: %d %s", path, rr.Code, rr.Body.String())
				}
			}
			rr = doRequest(srv, "POST", base+"/reminders", map[string]string{"remind_at": pastInstant})
			var rearmed models.Reminder
			parseJSON(t, rr, &rearmed)
			if rr.Code != http.StatusCreated || rearmed.ScheduleID == nil || *rearmed.ScheduleID != sc.ID {
				t.Fatalf("rearm: %d %s", rr.Code, rr.Body.String())
			}
		})
	}
}
