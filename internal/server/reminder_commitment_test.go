package server

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

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
