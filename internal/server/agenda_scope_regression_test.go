package server

import (
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"testing"
)

func TestAgendaNoDateScope(t *testing.T) {
	w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
	it, err := w.srv.store.CreateItem(w.ws.ID, w.colls["tasks"].ID, models.ItemCreate{Title: "Unscheduled", Fields: `{"status":"open"}`, AssignedUserID: &w.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, recipient string
		personal        bool
	}{{"workspace", "", false}, {"recipient", w.owner.ID, false}, {"me", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if tc.personal {
				m = meAgendaOK(t, w.agendaWorld, w.owner, agendaWindow)
			} else {
				m = w.ok(t, w.owner, agendaWindow+"&recipient="+tc.recipient)
			}
			rows, ok := m["no_date"].([]any)
			if !ok {
				t.Fatalf("no_date is not an array: %v", m)
			}
			if tc.name == "workspace" {
				if len(rows) != 0 || m["no_date_scope"] != "personal_only" {
					t.Fatalf("unfiltered no_date must be unlisted: %v", m)
				}
			} else if len(rows) != 1 || rows[0].(map[string]any)["item_ref"] != it.Ref || m["no_date_scope"] != nil {
				t.Fatalf("listed no_date: %v", m)
			}
		})
	}
	// An unfiltered scan must not spend even one candidate on unplaced work.
	rows, more, err := w.srv.store.ListAgendaWork(w.ws.ID, store.PendingReminderScope{}, "", "", 1, false)
	if err != nil || more || len(rows) != 0 {
		t.Fatalf("unfiltered candidates = %v, more=%v, err=%v", rows, more, err)
	}
}
