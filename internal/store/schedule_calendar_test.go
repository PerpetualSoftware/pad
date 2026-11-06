package store

import (
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4239: ListSchedulesForCalendar, the agenda's schedule read with finished
// items' history added. SQLite in make test, Postgres in make test-pg.

// customTerminalSchema closes an item on "shipped", so the terminal expression
// has more than one collection clause (and more than one set of arguments).
const customTerminalSchema = `{"fields":[` +
	`{"key":"status","type":"select","options":["open","shipped"],"default":"open","terminal_options":["shipped"]}]}`

type calWorld struct {
	*schedFixture
	custom *models.Collection
}

func newCalWorld(t *testing.T) *calWorld {
	t.Helper()
	f := newSchedFixture(t)
	c, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Releases", Schema: customTerminalSchema})
	if err != nil {
		t.Fatal(err)
	}
	return &calWorld{schedFixture: f, custom: c}
}

var dailyFromOct = models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}

// sched makes an item with a daily schedule in coll (the fixture's collection
// when nil), with the given status, and records its 1 October occurrence as
// done when recorded is set.
func (w *calWorld) sched(t *testing.T, coll *models.Collection, status string, recorded bool) *models.Item {
	t.Helper()
	cid := w.item.CollectionID
	if coll != nil {
		cid = coll.ID
	}
	item, err := w.s.CreateItem(w.ws.ID, cid, models.ItemCreate{Title: "S", Fields: `{"status":"open"}`})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.createScheduleAt(w.ws.ID, item.ID, dailyFromOct, rt("2026-10-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if recorded {
		w.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-01T10:00:00Z")
	}
	if status != "open" {
		f := `{"status":"` + status + `"}`
		if _, err := w.s.UpdateItem(item.ID, models.ItemUpdate{Fields: &f}); err != nil {
			t.Fatal(err)
		}
	}
	return item
}

func schedItemIDs(as []AgendaSchedule) []string {
	var out []string
	for _, a := range as {
		out = append(out, a.Schedule.ItemID)
	}
	sort.Strings(out)
	return out
}

func sameSchedItems(t *testing.T, what string, got []AgendaSchedule, want ...*models.Item) {
	t.Helper()
	var ids []string
	for _, it := range want {
		ids = append(ids, it.ID)
	}
	sort.Strings(ids)
	if strings.Join(schedItemIDs(got), "|") != strings.Join(ids, "|") {
		t.Errorf("%s = %d schedules, want %d", what, len(got), len(ids))
	}
}

// A finished item qualifies only through a history row near the window, with
// the terminal expression bound once per use: two collections give it two
// clauses with different values, so a misordered argument list reads wrong.
func TestListSchedulesForCalendarTerminalNeedsAnOccurrenceNearTheWindow(t *testing.T) {
	t.Parallel()
	w := newCalWorld(t)
	open := w.sched(t, nil, "open", false)
	openCustom := w.sched(t, w.custom, "open", false)
	done := w.sched(t, nil, "done", true)
	shipped := w.sched(t, w.custom, "shipped", true)
	doneNoRow := w.sched(t, nil, "done", false)
	shippedNoRow := w.sched(t, w.custom, "shipped", false)
	_ = doneNoRow
	_ = shippedNoRow
	all := PendingReminderScope{}

	// Around the recorded occurrence: the open ones and the two with a row.
	got, truncated, err := w.s.ListSchedulesForCalendar(w.ws.ID, all, rt("2026-10-01T00:00:00Z"), rt("2026-10-08T00:00:00Z"), 100)
	if err != nil || truncated {
		t.Fatalf("calendar: truncated=%v err=%v", truncated, err)
	}
	sameSchedItems(t, "calendar near the history", got, open, openCustom, done, shipped)
	terminal := map[string]bool{}
	for _, a := range got {
		terminal[a.Schedule.ItemID] = a.Terminal
		if a.ItemTitle == "" || a.ItemRef == "" {
			t.Errorf("a row lost its item facts: %+v", a)
		}
	}
	if terminal[open.ID] || terminal[openCustom.ID] || !terminal[done.ID] || !terminal[shipped.ID] {
		t.Errorf("Terminal flags = %v", terminal)
	}

	// The agenda's read is unchanged: open items only, no flag.
	ag, _, err := w.s.ListSchedulesForAgenda(w.ws.ID, all, rt("2026-10-01T00:00:00Z"), rt("2026-10-08T00:00:00Z"), 100)
	if err != nil {
		t.Fatal(err)
	}
	sameSchedItems(t, "agenda", ag, open, openCustom)
	for _, a := range ag {
		if a.Terminal {
			t.Errorf("the agenda's rows never carry Terminal: %+v", a)
		}
	}

	// Far from any history row: finished items take no place; open ones stay.
	got, _, err = w.s.ListSchedulesForCalendar(w.ws.ID, all, rt("2027-03-01T00:00:00Z"), rt("2027-03-08T00:00:00Z"), 100)
	if err != nil {
		t.Fatal(err)
	}
	sameSchedItems(t, "calendar far from the history", got, open, openCustom)
}

// Finished schedules that never ended must not push an open one past the scan
// bound: with the bound at 1, the open schedule is read and nothing is cut.
func TestListSchedulesForCalendarFinishedItemsDoNotFillTheScan(t *testing.T) {
	t.Parallel()
	w := newCalWorld(t)
	open := w.sched(t, nil, "open", false)
	for i := 0; i < 6; i++ {
		w.sched(t, nil, "done", true)
	}
	got, truncated, err := w.s.ListSchedulesForCalendar(w.ws.ID, PendingReminderScope{}, rt("2027-03-01T00:00:00Z"), rt("2027-03-08T00:00:00Z"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("six finished schedules with no history in the window filled a scan of one")
	}
	sameSchedItems(t, "scan of one", got, open)
}

// The terminal branch sits inside the same parentheses as the open filter, so
// nothing to its left can be skipped: a finished item with a history row in the
// window is still absent when it is out of scope, deleted, or in a deleted
// collection, for a collection-scoped caller (a member) and an item-granted one.
func TestListSchedulesForCalendarTerminalBranchStaysInScope(t *testing.T) {
	t.Parallel()
	w := newCalWorld(t)
	mine := w.sched(t, nil, "done", true)
	other := w.sched(t, w.custom, "shipped", true) // another collection
	deleted := w.sched(t, nil, "done", true)
	if err := w.s.DeleteItem(deleted.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := w.s.CreateCollection(w.ws.ID, models.CollectionCreate{Name: "Gone", Schema: customTerminalSchema})
	if err != nil {
		t.Fatal(err)
	}
	inGone := w.sched(t, gone, "shipped", true)
	if err := w.s.DeleteCollection(gone.ID, ""); err != nil {
		t.Fatal(err)
	}
	foreign := newCalWorld(t) // another workspace, same shape
	foreign.sched(t, nil, "done", true)

	from, to := rt("2026-10-01T00:00:00Z"), rt("2026-10-08T00:00:00Z")
	member := PendingReminderScope{CollectionIDs: []string{w.item.CollectionID}}
	got, _, err := w.s.ListSchedulesForCalendar(w.ws.ID, member, from, to, 100)
	if err != nil {
		t.Fatal(err)
	}
	sameSchedItems(t, "member of one collection", got, mine)

	guest := PendingReminderScope{CollectionIDs: []string{}, ItemIDs: []string{mine.ID}}
	got, _, err = w.s.ListSchedulesForCalendar(w.ws.ID, guest, from, to, 100)
	if err != nil {
		t.Fatal(err)
	}
	sameSchedItems(t, "guest with one item grant", got, mine)

	// A guest granted only what is out of reach reads nothing.
	guest = PendingReminderScope{CollectionIDs: []string{}, ItemIDs: []string{deleted.ID, inGone.ID}}
	got, _, err = w.s.ListSchedulesForCalendar(w.ws.ID, guest, from, to, 100)
	if err != nil {
		t.Fatal(err)
	}
	sameSchedItems(t, "guest granted a deleted item and a deleted collection's", got)
	_ = other
}
