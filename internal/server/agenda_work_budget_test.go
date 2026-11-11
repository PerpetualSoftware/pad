package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

func TestMeAgendaExactWorkBudgetWithEmptyWorkspace(t *testing.T) {
	for _, laterWork := range []bool{false, true} {
		name := "empty"
		if laterWork {
			name = "has candidate"
		}
		t.Run(name, func(t *testing.T) {
			w := newBootAgendaWorld(t, "2031-03-10T12:00:00Z")
			other := meAgendaWorkspace(t, w.agendaWorld, "Other")
			w.item(t, "tasks", "First", `{}`, nil)
			if laterWork {
				other.item(t, "tasks", "Later", `{}`, nil)
			}
			// Exercise the shared /me/agenda work read with a small exact budget.
			run := w.srv.newAgendaRead(meAgendaNow, meAgendaNow, usertz.Resolve(nil), "")
			run.workRemaining = 1
			if err := w.srv.appendAgendaWork(run, w.ws.ID, store.PendingReminderScope{}); err != nil {
				t.Fatal(err)
			}
			if run.workRemaining != 0 || len(run.response.NoDate) != 1 || !run.response.NoDateComplete {
				t.Fatalf("first workspace did not exactly consume budget: %+v", run)
			}
			if err := w.srv.appendAgendaWork(run, other.ws.ID, store.PendingReminderScope{}); err != nil {
				t.Fatal(err)
			}
			a := run.response
			if a.PriorityListComplete == laterWork || a.WaitingOnComplete == laterWork || a.NoDateComplete == laterWork {
				t.Fatalf("later work=%v completeness=%v/%v/%v", laterWork, a.PriorityListComplete, a.WaitingOnComplete, a.NoDateComplete)
			}
		})
	}
}
