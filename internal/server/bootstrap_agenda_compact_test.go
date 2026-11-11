package server

import (
	"encoding/json"
	"fmt"
	"github.com/PerpetualSoftware/pad/internal/models"
	"testing"
)

func TestBootstrapAgendaCompactGroups(t *testing.T) {
	for _, group := range []string{"priority_list", "waiting_on", "no_date"} {
		t.Run(group, func(t *testing.T) {
			w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
			blocker := w.item(t, "tasks", "Blocker", `{"status":"open"}`, nil)
			for n := 0; n < 15; n++ {
				it, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: fmt.Sprintf("Task %d", n), Fields: `{"status":"open"}`, AssignedUserID: &w.owner.ID})
				if err != nil {
					t.Fatal(err)
				}
				if group == "priority_list" {
					if _, err = w.srv.store.CreateSchedule(w.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2031-03-20T09:00", Tz: "UTC"}); err != nil {
						t.Fatal(err)
					}
				}
				if group == "waiting_on" {
					if _, err = w.srv.store.CreateScheduleGate(w.ws.ID, it.ID, blocker.ID, models.CommitmentAccess{Readable: map[string]bool{it.ID: true, blocker.ID: true}, Editable: map[string]bool{it.ID: true, blocker.ID: true}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			a := w.bootstrap(t, w.tok, nil).Agenda
			raw, _ := json.Marshal(a)
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			rows, _ := m[group].([]any)
			if len(rows) != 10 || m[group+"_more_count"] != float64(5) || m[group+"_complete"] != false {
				t.Fatalf("compact %s: %s", group, raw)
			}
		})
	}
}

func TestBootstrapAgendaEmptyAbsent(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	if a := w.bootstrap(t, w.tok, nil).Agenda; a != nil {
		t.Fatalf("empty agenda = %+v", a)
	}
}
