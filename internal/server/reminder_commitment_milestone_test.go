package server

import (
	"net/http"
	"strings"
	"testing"
)

// BUG-4612: `pad item unremind` (DELETE on the reminder alias) on a commitment
// with a forecast is refused like a gated one, and the forecast survives.
func TestReminderDeleteAliasRefusesForecastCommitment(t *testing.T) {
	srv := testServer(t)
	ws := createWSWithCollections(t, srv)
	item := createItem(t, srv, ws, "tasks", map[string]interface{}{"title": "Forecast milestone", "fields": `{"status":"open"}`})
	r := armViaAPI(t, srv, ws, item, futureInstant)
	if r.ScheduleID == nil {
		t.Fatalf("no commitment: %+v", r)
	}
	if err := srv.store.SetForecast(item.WorkspaceID, item.ID, "u1", "user", "2099-02-01", "estimate"); err != nil {
		t.Fatal(err)
	}
	rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+ws+"/reminders/"+r.ID, nil)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), `"code":"reminder_managed"`) {
		t.Fatalf("delete=%d %s", rr.Code, rr.Body.String())
	}
	sc, err := srv.store.GetSchedule(item.WorkspaceID, item.ID)
	if err != nil || sc == nil || sc.ID != *r.ScheduleID {
		t.Fatalf("refusal lost the commitment: %+v %v", sc, err)
	}
	var n int
	if err := srv.store.DB().QueryRow(`SELECT COUNT(*) FROM schedule_forecasts WHERE schedule_id=?`, sc.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("forecast rows=%d err=%v", n, err)
	}
}
