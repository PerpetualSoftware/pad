package server

import "testing"

func TestMeAgendaScanCapIsRequestWide(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	other := meAgendaWorkspace(t, w, "Other")
	w.srv.agendaScanLimit = 3
	for _, world := range []*agendaWorld{w, other} {
		for _, title := range []string{"One", "Two"} {
			world.item(t, "tasks", title, `{}`, oneShot())
		}
	}
	m := meAgendaOK(t, w, w.owner, agendaWindow)
	if len(entriesOf(m)) != 3 || m["truncated"] != true || m["next_from"] != nil {
		t.Fatalf("request scan cap = %v", m)
	}
	w.srv.agendaScanLimit = 2
	m = meAgendaOK(t, w, w.owner, agendaWindow)
	if len(entriesOf(m)) != 2 || m["truncated"] != true || m["next_from"] != nil {
		t.Fatalf("exhausted scan cap = %v", m)
	}
	w.srv.agendaScanLimit = 3
	w.item(t, "tasks", "Three", `{}`, oneShot())
	w.item(t, "tasks", "Four", `{}`, oneShot())
	m = w.ok(t, w.owner, agendaWindow)
	if len(entriesOf(m)) != 3 || m["truncated"] != true || m["next_from"] != nil {
		t.Fatalf("workspace scan cap = %v", m)
	}
}
