package cli

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReminderFlatCommitmentResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/v1/workspaces/ws/items/TASK-1/reminders" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"alarm","schedule_id":"schedule","occurrence_at":"2099-01-01T00:00:00Z","remind_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	c := NewClientFromURL(srv.URL)
	c.SetHTTPClient(srv.Client())
	r, err := c.CreateItemReminder("ws", "TASK-1", "2099-01-01T00:00:00Z")
	if err != nil || r.ID != "alarm" || r.ScheduleID == nil || *r.ScheduleID != "schedule" || r.OccurrenceAt == nil {
		t.Fatalf("response=%+v %v", r, err)
	}
}
