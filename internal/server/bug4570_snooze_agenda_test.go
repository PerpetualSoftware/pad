package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

func TestBUG4570SnoozeReadSurfaces(t *testing.T) {
	day := time.Now().UTC().AddDate(0, 0, 1)
	start := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, 0, 0, time.UTC)
	w := newBootAgendaWorld(t, start.Add(3*time.Hour).Format(time.RFC3339))
	item := w.sched(t, w.owner, "Snoozed", start.Format("2006-01-02T15:04"), "both")
	sc, err := w.srv.store.UpdateSchedule(w.ws.ID, item.ID, models.ScheduleUpdate{LeadMinutes: ptr(30)})
	if err != nil {
		t.Fatal(err)
	}
	until := start.Add(30 * time.Hour)
	original, _ := time.Parse(time.RFC3339, *sc.NextAt)
	resolve := func(state string) {
		t.Helper()
		if _, err := w.srv.store.ResolveOccurrence(w.ws.ID, item.ID, store.ResolveInput{State: state, OccurrenceAt: original, Until: &until, Actor: "user"}); err != nil {
			t.Fatal(err)
		}
	}
	w.srv.ensureRouter()
	srv := httptest.NewServer(w.srv.router)
	defer srv.Close()
	get := func(path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/"+path, nil)
		req.AddCookie(&http.Cookie{Name: "pad_session", Value: w.tok})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %+v", path, resp.StatusCode, body)
		}
		return body
	}
	for _, state := range []string{"snooze", "move"} {
		resolve(state)
		t.Run(state, func(t *testing.T) {
			check := func(name string, row map[string]any) {
				t.Helper()
				value, exists := row["snoozed_until"]
				if state == "snooze" {
					if value != until.Format(time.RFC3339) {
						t.Errorf("%s snoozed_until = %v", name, value)
					}
				} else if exists {
					t.Errorf("%s moved row has snoozed_until: %v", name, value)
				}
			}
			agenda := get("me/agenda?from=" + start.Add(-9*time.Hour).Format(time.RFC3339) + "&to=" + start.Add(39*time.Hour).Format(time.RFC3339) + "&tz=UTC")
			for _, name := range []string{"entries", "priority_list"} {
				rows, _ := agenda[name].([]any)
				found := false
				for _, r := range rows {
					row := r.(map[string]any)
					if row["item_ref"] != item.Ref || row["type"] != "schedule" {
						continue
					}
					found = true
					check(name, row)
					if state == "snooze" && (row["at"] != original.Format(time.RFC3339) || row["overdue"] != true) {
						t.Errorf("%s changed due facts: %+v", name, row)
					}
				}
				if !found {
					t.Errorf("%s missing occurrence", name)
				}
			}
			check("schedule", get("workspaces/"+w.ws.Slug+"/items/"+item.Ref+"/commitment"))
			// A moved target is tomorrow; move the read clock so bootstrap lists it as due today.
			if state == "move" {
				w.srv.agendaClock = func() time.Time { return until }
			}
			bootstrap := get("workspaces/" + w.ws.Slug + "/agent/bootstrap")
			a, ok := bootstrap["agenda"].(map[string]any)
			if !ok {
				t.Fatalf("bootstrap missing agenda: %+v", bootstrap)
			}
			rows, _ := a["due_today"].([]any)
			if len(rows) != 1 {
				t.Fatalf("bootstrap due rows: %+v", a)
			}
			check("bootstrap", rows[0].(map[string]any))
			if state == "snooze" {
				// Past the re-fire instant the snooze has ended, so bootstrap omits it.
				w.srv.agendaClock = func() time.Time { return until.Add(time.Second) }
				ended := get("workspaces/" + w.ws.Slug + "/agent/bootstrap")["agenda"].(map[string]any)
				seen := 0
				for _, group := range []string{"due_today", "carried"} {
					rows, _ := ended[group].([]any)
					for _, r := range rows {
						row := r.(map[string]any)
						if row["ref"] != item.Ref {
							continue
						}
						seen++
						if v, ok := row["snoozed_until"]; ok {
							t.Errorf("ended snooze still reported in %s: %v", group, v)
						}
					}
				}
				if seen == 0 {
					t.Errorf("ended snooze: occurrence missing from bootstrap: %+v", ended)
				}
				w.srv.agendaClock = func() time.Time { return start.Add(3 * time.Hour) }
			}
		})
	}
}
