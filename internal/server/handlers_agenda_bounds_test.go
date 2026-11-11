package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4236: next_from and unexpanded on the workspace agenda. next_from
// promises that every instant before it was examined, so it is emitted only
// when every cut that fired is ordered by time.

func (w *agendaWorld) scheduleID(t *testing.T, itemID string) string {
	t.Helper()
	var id string
	if err := w.srv.store.DB().QueryRow(w.srv.store.D().Rebind(`SELECT id FROM item_schedules WHERE item_id = ?`), itemID).Scan(&id); err != nil {
		t.Fatalf("schedule id: %v", err)
	}
	return id
}

// The per-schedule cut (100) is a time-ordered cut: next_from is the first
// occurrence not shown. hourlyWindow starts 2031-03-10T00:00Z with the 100th
// (zero-based) occurrence at +100h.
func TestAgendaPerScheduleCutCarriesNextFrom(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG hourly", `{"status":"open"}`, hourly())
	m := w.ok(t, w.owner, hourlyWindow)
	if n := len(entriesOf(m)); n != 100 || m["truncated"] != true {
		t.Fatalf("%d entries, truncated=%v; want 100 and truncated", n, m["truncated"])
	}
	if got := m["next_from"]; got != "2031-03-14T04:00:00Z" {
		t.Errorf("next_from = %v; want the first occurrence not shown, 2031-03-14T04:00:00Z", got)
	}
	if _, has := m["unexpanded"]; has {
		t.Errorf("unexpanded present with nothing unexpanded: %v", m["unexpanded"])
	}
}

// An untruncated response carries neither next_from nor unexpanded.
func TestAgendaUntruncatedHasNoNextFrom(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG one", `{"status":"open"}`, oneShot())
	m := w.ok(t, w.owner, agendaWindow)
	if _, has := m["next_from"]; has || m["truncated"] == true {
		t.Errorf("truncated=%v next_from=%v; want neither", m["truncated"], m["next_from"])
	}
}

// The response cut (500) resumes at the first entry not shown. Six hourly
// schedules put six entries on each of the first 100 hours; the 501st entry in
// time order is on hour 83 (500 / 6), earlier than the per-schedule cuts at 100.
func TestAgendaResponseCutCarriesNextFrom(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	for i := 0; i < 6; i++ {
		w.item(t, "tasks", fmt.Sprintf("AG hourly %d", i), `{"status":"open"}`, hourly())
	}
	m := w.ok(t, w.owner, hourlyWindow)
	if n := len(entriesOf(m)); n != 500 || m["truncated"] != true {
		t.Fatalf("%d entries, truncated=%v; want 500 and truncated", n, m["truncated"])
	}
	if got := m["next_from"]; got != "2031-03-13T11:00:00Z" {
		t.Errorf("next_from = %v; want 2031-03-13T11:00:00Z (hour 83)", got)
	}
}

// A schedule the window cannot be expanded for (a scan cap: dtstart 30 years
// back on a minute-level rule) is named, never dropped; the others in the same
// response are complete; truncated is true and next_from is the window's end.
func TestAgendaNamesAScheduleItCouldNotExpand(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	w.item(t, "tasks", "AG normal", `{"status":"open"}`, oneShot())
	far := w.item(t, "tasks", "AG far back", `{"status":"open"}`,
		&models.ScheduleCreate{Kind: "check", Dtstart: "2001-03-01T00:00", Tz: "UTC", Rrule: "FREQ=DAILY;BYHOUR=3;BYMINUTE=0"})
	farID := w.scheduleID(t, far.ID)
	// Under the 15-minute floor, so only a stored row can be.
	if _, err := w.srv.store.DB().Exec(w.srv.store.D().Rebind(`UPDATE item_schedules SET rrule = ? WHERE id = ?`), "FREQ=MINUTELY;BYHOUR=3;BYMINUTE=0", farID); err != nil {
		t.Fatal(err)
	}
	code, m := w.get(t, w.owner, agendaWindow)
	if code != http.StatusOK {
		t.Fatalf("%d %v", code, m)
	}
	un, _ := m["unexpanded"].([]any)
	if len(un) != 1 {
		t.Fatalf("unexpanded = %v; want the one far-back schedule", m["unexpanded"])
	}
	u := un[0].(map[string]any)
	wantRef := fmt.Sprintf("P0-%d", *far.ItemNumber)
	if u["schedule_id"] != farID || u["item_ref"] != wantRef || u["item_title"] != "AG far back" || u["collection"] != "tasks" || u["reason"] != "scan_limit" {
		t.Errorf("unexpanded[0] = %v; want schedule_id %s, item_ref %s, title, collection tasks, reason scan_limit", u, farID, wantRef)
	}
	if m["truncated"] != true || m["next_from"] != m["to"] {
		t.Errorf("truncated=%v next_from=%v to=%v; want truncated and next_from = to", m["truncated"], m["next_from"], m["to"])
	}
	if un2, _ := m["unavailable"].([]any); len(un2) != 0 {
		t.Errorf("unavailable = %v; a scan cap is unexpanded, not unavailable", un2)
	}
	var normal int
	for _, e := range entriesOf(m) {
		if e["item_title"] == "AG normal" {
			normal++
		}
		if e["item_title"] == "AG far back" {
			t.Errorf("the unexpanded schedule has an entry: %v", e)
		}
	}
	if normal != 1 {
		t.Errorf("the healthy schedule has %d entries; want 1", normal)
	}
}

// More than 1000 schedules is a cut ordered by id, not by time: truncated, and
// no next_from, because no instant describes it.
func TestAgendaScheduleSetCutHasNoNextFrom(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	for i := 0; i < 1001; i++ {
		w.item(t, "tasks", fmt.Sprintf("AG s%04d", i), `{"status":"open"}`, oneShot())
	}
	m := w.ok(t, w.owner, agendaWindow)
	if m["truncated"] != true {
		t.Fatalf("1001 schedules: truncated=%v; want true", m["truncated"])
	}
	if got, has := m["next_from"]; has {
		t.Errorf("next_from = %v; a schedule-set cut has none", got)
	}
}

// next_from is a promise about instants before it. A cut that falls on a dated
// entry (selected by calendar day, not by instant) cannot make it, so the
// response is truncated with no next_from. Five hourly schedules fill exactly
// 500 entries in a 100-hour window; the dated item is the 501st.
func TestAgendaCutOnADatedEntryHasNoNextFrom(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	for i := 0; i < 5; i++ {
		w.item(t, "tasks", fmt.Sprintf("AG hourly %d", i), `{"status":"open"}`, hourly())
	}
	w.item(t, "tasks", "AG dated", `{"status":"open","due_date":"2031-03-14T03:30:00Z"}`, nil)
	m := w.ok(t, w.owner, "from=2031-03-10T00:00:00Z&to=2031-03-14T04:00:00Z")
	if m["truncated"] != true || len(entriesOf(m)) != 500 {
		t.Fatalf("truncated=%v entries=%d; want truncated at 500", m["truncated"], len(entriesOf(m)))
	}
	if got, has := m["next_from"]; has {
		t.Errorf("next_from = %v; a cut on a dated entry has none", got)
	}
}

// 50 schedules in one workspace agenda request (TASK-4236 acceptance): entries
// stay at the 500 bound, and truncated and next_from are set. Fifty hourly
// schedules put fifty entries on each hour, so the 501st entry in time order is
// on hour 10 (500 / 50) of the window, earlier than any per-schedule cut at 100.
func TestAgendaFiftySchedulesStayWithinTheBoundAndResume(t *testing.T) {
	t.Parallel()
	w := newAgendaWorld(t, "tasks")
	// Exercise the entry cap independently of elapsed test-runner time.
	// Deadline cuts are covered with the injectable clock in their own tests.
	w.srv.agendaClock = func() time.Time { return meAgendaNow }

	for i := 0; i < 50; i++ {
		w.item(t, "tasks", fmt.Sprintf("AG hourly %02d", i), `{"status":"open"}`, hourly())
	}
	m := w.ok(t, w.owner, hourlyWindow)
	if n := len(entriesOf(m)); n != 500 || m["truncated"] != true {
		t.Fatalf("%d entries, truncated=%v; want exactly 500 and truncated", n, m["truncated"])
	}
	if got := m["next_from"]; got != "2031-03-10T10:00:00Z" {
		t.Errorf("next_from = %v; want 2031-03-10T10:00:00Z (hour 10)", got)
	}
}
