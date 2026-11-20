package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const countsSchema = `{"fields":[{"key":"status","type":"select","options":["open","done","cancelled"],"default":"open","terminal_options":["done","cancelled"],"abandoned_options":["cancelled"]}]}`

// countsItem makes an item created at createdAt whose history is the given
// (instant, status) steps. Its own create row is replaced, so the history is
// exactly what the test wrote.
func countsItem(t *testing.T, f *schedFixture, collID, title, createdAt string, steps ...[2]string) *models.Item {
	t.Helper()
	it := createTestItem(t, f.s, f.ws.ID, collID, title, "")
	f.exec(t, `UPDATE items SET created_at = ? WHERE id = ?`, createdAt, it.ID)
	f.exec(t, `DELETE FROM status_transitions WHERE item_id = ?`, it.ID)
	prev := ""
	all := append([][2]string{{createdAt, "open"}}, steps...)
	for n, st := range all {
		f.exec(t, `INSERT INTO status_transitions (id,item_id,workspace_id,collection_id,field_key,from_status,to_status,created_at,seq) VALUES (?,?,?,?,'status',?,?,?,?)`,
			fmt.Sprintf("%s-%d", it.ID, n), it.ID, f.ws.ID, collID, prev, st[1], st[0], n)
		prev = st[1]
	}
	return it
}

func countsFixture(t *testing.T) *schedFixture {
	f := newSchedFixture(t)
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, countsSchema, f.item.CollectionID)
	// The fixture's own item would be one more open item in every series.
	f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2030-01-01T00:00:00Z", f.item.ID)
	return f
}

func countsGot(days []AgendaDayCount) string {
	out := ""
	for _, d := range days {
		out += fmt.Sprintf("%s:%d/%d/%d ", d.Day, d.Completed, d.Open, d.Burndown)
	}
	return out
}

// completed / open at the end of the day / burn-down of the items open at the
// window's start. New items never add to the burn-down line.
func TestAgendaCountsSeries(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-13T00:00:00Z")
	countsItem(t, f, c, "stays open", "2031-03-01T00:00:00Z")
	countsItem(t, f, c, "closes day one", "2031-03-05T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	countsItem(t, f, c, "done before", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "done"})
	countsItem(t, f, c, "abandoned day two", "2031-03-05T00:00:00Z", [2]string{"2031-03-11T10:00:00Z", "cancelled"})
	countsItem(t, f, c, "new and done", "2031-03-11T08:00:00Z", [2]string{"2031-03-11T20:00:00Z", "done"})
	countsItem(t, f, c, "new and open", "2031-03-12T01:00:00Z")
	countsItem(t, f, c, "reopened", "2031-03-05T00:00:00Z", [2]string{"2031-03-09T00:00:00Z", "done"}, [2]string{"2031-03-12T05:00:00Z", "open"})
	days, _, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 1000)
	if err != nil || truncated {
		t.Fatalf("counts: %v truncated=%v", err, truncated)
	}
	// Open at the start: stays open, closes day one, abandoned day two = 3.
	// Day 10: closes day one done. Day 11: abandoned (not completed) and
	// new and done. Day 12: reopened, which is not a completion and does not
	// join the burn-down (it was closed at the start).
	want := "2031-03-10:1/2/2 2031-03-11:1/1/1 2031-03-12:0/3/1 "
	if got := countsGot(days); got != want {
		t.Fatalf("series = %s, want %s", got, want)
	}
}

func TestAgendaCountsScopeAndDeleted(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	hidden := createTestCollection(t, f.s, f.ws.ID, "Hidden")
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, countsSchema, hidden.ID)
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "visible", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	countsItem(t, f, hidden.ID, "hidden", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	gone := countsItem(t, f, c, "deleted", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2031-03-10T11:00:00Z", gone.ID)
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{CollectionIDs: []string{c}}, from, to, time.UTC, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := countsGot(days), "2031-03-10:1/0/0 "; got != want {
		t.Fatalf("scoped = %s, want %s", got, want)
	}
	none, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{CollectionIDs: []string{}}, from, to, time.UTC, 1000)
	if err != nil || countsGot(none) != "2031-03-10:0/0/0 " {
		t.Fatalf("empty scope = %s %v", countsGot(none), err)
	}
}

// 23:30 local lands on that local day, and a window that starts mid-day still
// buckets by the zone's days.
func TestAgendaCountsLocalDays(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	from, to := rt("2031-03-10T07:00:00Z"), rt("2031-03-13T07:00:00Z")
	countsItem(t, f, c, "late local", "2031-03-01T00:00:00Z", [2]string{"2031-03-11T06:30:00Z", "done"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, la, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := countsGot(days), "2031-03-10:1/0/0 2031-03-11:0/0/0 2031-03-12:0/0/0 "; got != want {
		t.Fatalf("series = %s, want %s", got, want)
	}
}

// A zone that skips a whole local date must still end the day loop.
func TestAgendaCountsSkippedLocalDate(t *testing.T) {
	f := countsFixture(t)
	apia, err := time.LoadLocation("Pacific/Apia") // skipped 2011-12-30
	if err != nil {
		t.Skip("no tzdata")
	}
	from, to := rt("2011-12-29T00:00:00Z"), rt("2012-01-02T00:00:00Z")
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, apia, 1000)
	if err != nil || len(days) == 0 || len(days) > 6 {
		t.Fatalf("days = %s %v", countsGot(days), err)
	}
}

func TestAgendaCountsCap(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	for i := 0; i < 3; i++ {
		countsItem(t, f, c, fmt.Sprintf("capped %d", i), "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	}
	days, _, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 2)
	if err != nil || !truncated || days != nil {
		t.Fatalf("cap: days=%v truncated=%v err=%v", days, truncated, err)
	}
}

// The cap covers creations and status changes together.
func TestAgendaCountsCapIsShared(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	for i := 0; i < 2; i++ {
		countsItem(t, f, c, fmt.Sprintf("shared %d", i), "2031-03-10T01:00:00Z", [2]string{"2031-03-10T10:00:00Z", "done"})
	}
	// 2 creations + 4 transitions (create rows are in the window too) = 6 rows.
	if _, rows, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 5); err != nil || !truncated || rows > 6 {
		t.Fatalf("cap 5: rows=%d truncated=%v err=%v", rows, truncated, err)
	}
	if _, _, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 6); err != nil || truncated {
		t.Fatalf("cap 6: truncated=%v err=%v", truncated, err)
	}
}

// A non-ASCII terminal value reads the same on SQLite and Postgres.
func TestAgendaCountsUnicodeTerminal(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, `{"fields":[{"key":"status","type":"select","options":["open","Готово"],"default":"open","terminal_options":["Готово"]}]}`, c)
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "closed before", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "Готово"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 100)
	if err != nil || countsGot(days) != "2031-03-10:0/0/0 " {
		t.Fatalf("unicode terminal: %s %v", countsGot(days), err)
	}
}

// A status that is a case variant of a terminal value (written before a
// schema edit recased the option) is terminal to the start-of-window query and
// to the replay alike, so the burn-down never counts an item the replay cannot
// close. DONE closed the item before the window, so the move to done inside it
// is neither a completion nor a change in open: 0 done, 0 open, burn-down 0.
func TestAgendaCountsCaseVariantAgrees(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "odd", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "DONE"}, [2]string{"2031-03-10T10:00:00Z", "done"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 100)
	if err != nil || countsGot(days) != "2031-03-10:0/0/0 " {
		t.Fatalf("case variant: %s %v", countsGot(days), err)
	}
}

// The schema now spells its terminal "done"; rows written while it was "Done"
// still close the item, before the window and inside it.
func TestAgendaCountsRecasedTerminal(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "closed before", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "Done"})
	countsItem(t, f, c, "closes in window", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "Done"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 100)
	// Open at the start: closes in window only. It completes on the day.
	if err != nil || countsGot(days) != "2031-03-10:1/0/0 " {
		t.Fatalf("recased terminal: %s %v", countsGot(days), err)
	}
}

// A recased abandoned value closes the item without counting a completion.
func TestAgendaCountsRecasedAbandoned(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "abandoned before", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "Cancelled"})
	countsItem(t, f, c, "abandoned in window", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "Cancelled"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 100)
	if err != nil || countsGot(days) != "2031-03-10:0/0/0 " {
		t.Fatalf("recased abandoned: %s %v", countsGot(days), err)
	}
}

// A non-ASCII terminal recased in the row folds in Go, not in SQL, so SQLite
// (which lowercases ASCII only) and Postgres read it closed alike.
func TestAgendaCountsRecasedUnicodeTerminal(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, `{"fields":[{"key":"status","type":"select","options":["open","Готово"],"default":"open","terminal_options":["Готово"]}]}`, c)
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
	countsItem(t, f, c, "closed before", "2031-03-01T00:00:00Z", [2]string{"2031-03-02T00:00:00Z", "ГОТОВО"})
	countsItem(t, f, c, "closes in window", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T10:00:00Z", "ГОТОВО"})
	days, _, _, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 100)
	if err != nil || countsGot(days) != "2031-03-10:1/0/0 " {
		t.Fatalf("recased unicode terminal: %s %v", countsGot(days), err)
	}
}

// countsSchedule gives it a UTC check schedule created at createdAt. rrule ""
// is a one-shot.
func countsSchedule(t *testing.T, f *schedFixture, it *models.Item, dtstart, rrule, createdAt string) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.createScheduleAt(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: dtstart, Tz: "UTC", Rrule: rrule}, rt(createdAt))
	if err != nil {
		t.Fatalf("createScheduleAt: %v", err)
	}
	// A reported completion may not precede the schedule's creation.
	f.exec(t, `UPDATE item_schedules SET created_at=? WHERE id=?`, createdAt, sc.ID)
	return f.reload(t, it.ID)
}

func countsDone(t *testing.T, f *schedFixture, it *models.Item, in ResolveInput, now string) *ResolveResult {
	t.Helper()
	if in.State == "" {
		in.State, in.Actor, in.View = "done", "user", time.UTC
	}
	res, err := f.s.resolveOccurrenceAt(f.ws.ID, it.ID, in, rt(now))
	if err != nil {
		t.Fatalf("resolve done at %s: %v", now, err)
	}
	return res
}

func countsSeries(t *testing.T, f *schedFixture, scope PendingReminderScope, from, to string, loc *time.Location) string {
	t.Helper()
	days, _, truncated, err := f.s.AgendaCounts(f.ws.ID, scope, rt(from), rt(to), loc, 1000)
	if err != nil || truncated {
		t.Fatalf("counts: %v truncated=%v", err, truncated)
	}
	return countsGot(days)
}

// An on-time Done of a repeating occurrence completes the item that day
// without closing it: Open and Burndown do not move.
func TestAgendaCountsOccurrenceDone(t *testing.T) {
	f := countsFixture(t)
	it := countsItem(t, f, f.item.CollectionID, "daily", "2031-03-01T00:00:00Z")
	countsSchedule(t, f, it, "2031-03-10T09:00", "FREQ=DAILY", "2031-03-10T00:00:00Z")
	countsDone(t, f, it, ResolveInput{OccurrenceAt: rt("2031-03-10T09:00:00Z")}, "2031-03-10T10:00:00Z")
	if got, want := countsSeries(t, f, PendingReminderScope{}, "2031-03-10T00:00:00Z", "2031-03-12T00:00:00Z", time.UTC), "2031-03-10:1/1/1 2031-03-11:0/1/1 "; got != want {
		t.Fatalf("series = %s, want %s", got, want)
	}
	// The occurrence is the window's only row, and it counts toward the cap.
	from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-12T00:00:00Z")
	if _, rows, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 0); err != nil || !truncated || rows != 1 {
		t.Fatalf("cap 0: rows=%d truncated=%v err=%v", rows, truncated, err)
	}
	if _, rows, truncated, err := f.s.AgendaCounts(f.ws.ID, PendingReminderScope{}, from, to, time.UTC, 1); err != nil || truncated || rows != 1 {
		t.Fatalf("cap 1: rows=%d truncated=%v err=%v", rows, truncated, err)
	}
}

// A late Done of a lapsed occurrence buckets on the reported completion, not
// on the day it was recorded, and its Undo (back to lapsed) removes the count.
func TestAgendaCountsOccurrenceLateDoneAndUndo(t *testing.T) {
	f := countsFixture(t)
	it := countsItem(t, f, f.item.CollectionID, "lapsing", "2026-09-01T00:00:00Z")
	sc := countsSchedule(t, f, it, "2026-10-01T09:00", "FREQ=DAILY", "2026-10-01T00:00:00Z")
	f.nativeDeadline(t, sc, "lapses", 3600)
	f.eval(t, lapseNow, 0)
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='lapsed'`, sc.ID); n != 1 {
		t.Fatalf("lapsed rows = %d, want 1", n)
	}
	actual := rt("2026-10-26T09:30:00Z")
	res := f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: "done", Actor: "user", ActualDoneAt: &actual}, "2026-10-27T13:00:00Z")
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='done' AND actual_done_at=? AND resolved_at=?`, sc.ID, "2026-10-26T09:30:00Z", "2026-10-27T13:00:00Z"); n != 1 {
		t.Fatalf("late done rows = %d, want 1", n)
	}
	from, to := "2026-10-26T00:00:00Z", "2026-10-28T00:00:00Z"
	if got, want := countsSeries(t, f, PendingReminderScope{}, from, to, time.UTC), "2026-10-26:1/1/1 2026-10-27:0/1/1 "; got != want {
		t.Fatalf("late done = %s, want %s", got, want)
	}
	if res.ScheduleLastResolution == nil || !res.ScheduleLastResolution.CanReopen {
		t.Fatalf("late done not reopenable: %+v", res.ScheduleLastResolution)
	}
	if _, err := f.s.reopenOccurrenceAt(f.ws.ID, it.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt("2026-10-27T13:01:00Z")); err != nil {
		t.Fatalf("undo: %v", err)
	}
	assertHistoryLapsedAgain(t, f, sc.ID, deadlineOccAt)
	if got, want := countsSeries(t, f, PendingReminderScope{}, from, to, time.UTC), "2026-10-26:0/1/1 2026-10-27:0/1/1 "; got != want {
		t.Fatalf("after undo = %s, want %s", got, want)
	}
}

// An item closed and its one-shot resolved Done on the same day is one
// completion. Closed one day and resolved Done the next, it completes on both.
func TestAgendaCountsOccurrenceDedupesWithClose(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	same := countsItem(t, f, c, "same day", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T11:00:00Z", "done"})
	countsSchedule(t, f, same, "2031-03-10T09:00", "", "2031-03-10T00:00:00Z")
	countsDone(t, f, same, ResolveInput{State: "done", Actor: "user", View: time.UTC, OccurrenceAt: rt("2031-03-10T09:00:00Z"), EndSchedule: true}, "2031-03-10T10:00:00Z")
	next := countsItem(t, f, c, "next day", "2031-03-01T00:00:00Z", [2]string{"2031-03-10T11:00:00Z", "done"})
	countsSchedule(t, f, next, "2031-03-11T09:00", "", "2031-03-10T00:00:00Z")
	countsDone(t, f, next, ResolveInput{State: "done", Actor: "user", View: time.UTC, OccurrenceAt: rt("2031-03-11T09:00:00Z"), EndSchedule: true}, "2031-03-11T10:00:00Z")
	if got, want := countsSeries(t, f, PendingReminderScope{}, "2031-03-10T00:00:00Z", "2031-03-12T00:00:00Z", time.UTC), "2031-03-10:2/0/0 2031-03-11:1/0/0 "; got != want {
		t.Fatalf("series = %s, want %s", got, want)
	}
}

// A done occurrence counts only for an item the caller can see: one outside
// the scope and one deleted count nothing.
func TestAgendaCountsOccurrenceScope(t *testing.T) {
	f := countsFixture(t)
	c := f.item.CollectionID
	hidden := createTestCollection(t, f.s, f.ws.ID, "Hidden")
	f.exec(t, `UPDATE collections SET schema = ? WHERE id = ?`, countsSchema, hidden.ID)
	for _, x := range []struct{ coll, title string }{{c, "visible"}, {hidden.ID, "hidden"}, {c, "deleted"}} {
		it := countsItem(t, f, x.coll, x.title, "2031-03-01T00:00:00Z")
		countsSchedule(t, f, it, "2031-03-10T09:00", "FREQ=DAILY", "2031-03-10T00:00:00Z")
		countsDone(t, f, it, ResolveInput{OccurrenceAt: rt("2031-03-10T09:00:00Z")}, "2031-03-10T10:00:00Z")
		if x.title == "deleted" {
			f.exec(t, `UPDATE items SET deleted_at = ? WHERE id = ?`, "2031-03-10T11:00:00Z", it.ID)
		}
	}
	if got, want := countsSeries(t, f, PendingReminderScope{CollectionIDs: []string{c}}, "2031-03-10T00:00:00Z", "2031-03-11T00:00:00Z", time.UTC), "2031-03-10:1/1/1 "; got != want {
		t.Fatalf("scoped = %s, want %s", got, want)
	}
}

// A reported completion at 23:30 in Los Angeles, recorded after local
// midnight, lands on the earlier local day; in UTC both are the later day.
func TestAgendaCountsOccurrenceLocalDay(t *testing.T) {
	f := countsFixture(t)
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	it := countsItem(t, f, f.item.CollectionID, "late local", "2031-03-01T00:00:00Z")
	countsSchedule(t, f, it, "2031-03-11T06:00", "FREQ=DAILY", "2031-03-10T00:00:00Z")
	actual := rt("2031-03-11T06:30:00Z") // 2031-03-10 23:30 PDT
	countsDone(t, f, it, ResolveInput{State: "done", Actor: "user", View: time.UTC, OccurrenceAt: rt("2031-03-11T06:00:00Z"), ActualDoneAt: &actual}, "2031-03-11T08:00:00Z")
	if got, want := countsSeries(t, f, PendingReminderScope{}, "2031-03-10T07:00:00Z", "2031-03-12T07:00:00Z", la), "2031-03-10:1/1/1 2031-03-11:0/1/1 "; got != want {
		t.Fatalf("local = %s, want %s", got, want)
	}
	if got, want := countsSeries(t, f, PendingReminderScope{}, "2031-03-10T00:00:00Z", "2031-03-12T00:00:00Z", time.UTC), "2031-03-10:0/1/1 2031-03-11:1/1/1 "; got != want {
		t.Fatalf("utc = %s, want %s", got, want)
	}
}
