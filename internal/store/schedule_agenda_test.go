package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4104 D1: the agenda's expansion and its date-window clause. SQLite in
// make test, Postgres in make test-pg.

// agendaStates maps an entry's wall to its state, for a compact assertion.
func agendaStates(es []AgendaEntry) map[string]string {
	out := map[string]string{}
	for _, e := range es {
		out[e.Wall.String()] = e.State
	}
	return out
}

func (f *schedFixture) expand(t *testing.T, item *models.Item, view *time.Location, from, to, now string) []AgendaEntry {
	t.Helper()
	sc, err := f.s.GetSchedule(f.ws.ID, item.ID)
	if err != nil || sc == nil {
		t.Fatalf("GetSchedule: %v %v", sc, err)
	}
	es, _, unavailable, err := f.s.ExpandAgenda(sc, view, rt(from), rt(to), rt(now))
	if err != nil || unavailable != "" {
		t.Fatalf("ExpandAgenda: unavailable=%q err=%v", unavailable, err)
	}
	return es
}

func TestAgendaExpandMergesOccurrenceState(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-01T00:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "done", Actor: "user"}, "2026-10-01T10:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-02T09:00:00Z"), State: "skip", Actor: "user"}, "2026-10-02T10:00:00Z")

	es := f.expand(t, item, time.UTC, "2026-10-01T00:00:00Z", "2026-10-08T00:00:00Z", "2026-10-05T12:00:00Z")
	want := map[string]string{
		"2026-10-01T09:00:00": "done", "2026-10-02T09:00:00": "skipped", "2026-10-03T09:00:00": "outstanding",
		"2026-10-04T09:00:00": "missed", "2026-10-05T09:00:00": "missed",
		"2026-10-06T09:00:00": "scheduled", "2026-10-07T09:00:00": "scheduled",
	}
	if got := agendaStates(es); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("states = %v, want %v", got, want)
	}
	for _, e := range es {
		if e.State == "outstanding" && (!e.Overdue || e.MissedCount != 2 || !e.Stale) {
			t.Errorf("outstanding entry = %+v, want overdue, missed_count 2, stale", e)
		}
		if e.State != "outstanding" && (e.Overdue || e.MissedCount != 0) {
			t.Errorf("only the outstanding entry carries overdue/missed_count: %+v", e)
		}
	}

	// A row that is not there is never invented: the occurrence reads unrecorded.
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM schedule_occurrences WHERE state = 'skipped'`)); err != nil {
		t.Fatal(err)
	}
	es = f.expand(t, item, time.UTC, "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z", "2026-10-05T12:00:00Z")
	if got := agendaStates(es)["2026-10-02T09:00:00"]; got != "unrecorded" {
		t.Errorf("before the outstanding occurrence with no row = %q, want unrecorded", got)
	}
}

// A resolve writes the history row in the RECIPIENT's reading. The agenda reads a
// floating schedule in the caller's zone, where the same wall is another instant,
// so the row must be found by its wall (F3).
func TestAgendaFloatingMergesByWall(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	la := "America/Los_Angeles"
	u, err := f.s.CreateUser(models.UserCreate{Email: "la@example.com", Name: "LA", Password: "pw-123456789"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateUser(u.ID, models.UserUpdate{Timezone: &la}); err != nil {
		t.Fatal(err)
	}
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Floating: true, Rrule: "FREQ=DAILY"}, "2026-10-01T00:00:00Z")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE items SET assigned_user_id = ? WHERE id = ?`), u.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	laLoc, _ := time.LoadLocation(la)
	// 09:00 in Los Angeles on 2026-10-01 (PDT) is 16:00Z.
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T16:00:00Z"), State: "done", View: laLoc, Actor: "user"}, "2026-10-01T17:00:00Z")

	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	es := f.expand(t, item, tokyo, "2026-09-30T00:00:00Z", "2026-10-03T00:00:00Z", "2026-10-01T17:00:00Z")
	var found bool
	for _, e := range es {
		if e.Wall.String() == "2026-10-01T09:00:00" {
			found = true
			if e.State != "done" {
				t.Errorf("floating row resolved in LA, read in Tokyo: state = %q, want done", e.State)
			}
			if !e.At.Equal(rt("2026-10-01T00:00:00Z")) {
				t.Errorf("at = %s, want 09:00 Tokyo = 2026-10-01T00:00:00Z", e.At)
			}
		}
	}
	if !found {
		t.Fatalf("the 2026-10-01T09:00:00 wall is not on the agenda: %+v", es)
	}
}

func TestAgendaPinnedIgnoresViewerFloatingFollowsIt(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	pinned, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Tz: "Asia/Tokyo"}, "2026-10-01T00:00:00Z")
	floating, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true}, "2026-10-01T00:00:00Z")
	la, _ := time.LoadLocation("America/Los_Angeles")
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	at := func(item *models.Item, view *time.Location) string {
		es := f.expand(t, item, view, "2026-11-01T00:00:00Z", "2026-11-04T00:00:00Z", "2026-10-01T00:00:00Z")
		if len(es) != 1 {
			t.Fatalf("entries = %+v", es)
		}
		return es[0].At.UTC().Format(time.RFC3339)
	}
	if a, b := at(pinned, la), at(pinned, tokyo); a != "2026-11-02T00:00:00Z" || b != a {
		t.Errorf("pinned Tokyo 09:00: la %s tokyo %s, want 2026-11-02T00:00:00Z in both", a, b)
	}
	if a, b := at(floating, la), at(floating, tokyo); a != "2026-11-02T17:00:00Z" || b != "2026-11-02T00:00:00Z" {
		t.Errorf("floating 09:00: la %s tokyo %s, want 17:00Z and 00:00Z", a, b)
	}
}

func TestAgendaMovedOccurrenceAtGridWithEffectiveAt(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-01T00:00:00Z")
	until := rt("2026-10-04T15:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "move", Until: &until, Actor: "user"}, "2026-10-01T08:00:00Z")

	for name, w := range map[string][2]string{
		"grid window":      {"2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z"},
		"effective window": {"2026-10-04T00:00:00Z", "2026-10-05T00:00:00Z"},
	} {
		es := f.expand(t, item, time.UTC, w[0], w[1], "2026-10-01T08:00:00Z")
		var o *AgendaEntry
		for i := range es {
			if es[i].State == "outstanding" {
				o = &es[i]
			}
		}
		if o == nil {
			t.Errorf("%s: the moved occurrence is not listed: %+v", name, es)
			continue
		}
		if !o.At.Equal(rt("2026-10-01T09:00:00Z")) || o.EffectiveAt == nil || !o.EffectiveAt.Equal(until) {
			t.Errorf("%s: at %s effective %v, want grid 2026-10-01T09:00Z and effective %s", name, o.At, o.EffectiveAt, until)
		}
	}
	if es := f.expand(t, item, time.UTC, "2026-10-02T00:00:00Z", "2026-10-03T00:00:00Z", "2026-10-01T08:00:00Z"); len(es) != 1 || es[0].State != "scheduled" {
		t.Errorf("a window holding neither instant lists only the grid's own days: %+v", es)
	}
}

func TestAgendaSnoozeDoesNotMoveTheEntry(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-01T00:00:00Z")
	until := rt("2026-10-04T15:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "snooze", Until: &until, Actor: "user"}, "2026-10-01T10:00:00Z")
	es := f.expand(t, item, time.UTC, "2026-10-04T00:00:00Z", "2026-10-05T00:00:00Z", "2026-10-01T10:00:00Z")
	if len(es) != 1 || es[0].State != "scheduled" || es[0].EffectiveAt != nil {
		t.Errorf("a snooze must not place the outstanding entry in the snooze day, only the grid's own 10-04 occurrence: %+v", es)
	}
}

func TestAgendaAllDayAtIsLocalMidnight(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-05", Tz: "Asia/Tokyo"}, "2026-10-01T00:00:00Z")
	es := f.expand(t, item, time.UTC, "2026-11-01T00:00:00Z", "2026-11-10T00:00:00Z", "2026-10-01T00:00:00Z")
	if len(es) != 1 || !es[0].Wall.AllDay || !es[0].At.Equal(rt("2026-11-04T15:00:00Z")) {
		t.Fatalf("all-day Tokyo 2026-11-05 = %+v, want At local midnight 2026-11-04T15:00:00Z (not the 09:00 fire hour)", es)
	}
}

func TestAgendaDegradesPerSchedule(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	runaway, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T00:00", Tz: "UTC", Rrule: "FREQ=MINUTELY"}, "2026-10-01T00:00:00Z")
	sc, _ := f.s.GetSchedule(f.ws.ID, runaway.ID)
	es, truncated, unavailable, err := f.s.ExpandAgenda(sc, time.UTC, rt("2026-10-02T00:00:00Z"), rt("2026-12-30T00:00:00Z"), rt("2026-10-01T00:00:00Z"))
	if err != nil {
		t.Fatalf("a runaway rule is a per-schedule outcome, not an error: %v", err)
	}
	if unavailable == "" && !(truncated && len(es) > 0 && len(es) <= 500) {
		t.Errorf("runaway: truncated=%v entries=%d unavailable=%q, want truncated at <=500 or unavailable", truncated, len(es), unavailable)
	}
	broken, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, "2026-10-01T00:00:00Z")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE item_schedules SET error_code = 'cap_scan' WHERE item_id = ?`), broken.ID); err != nil {
		t.Fatal(err)
	}
	sc, _ = f.s.GetSchedule(f.ws.ID, broken.ID)
	if es, _, unavailable, err = f.s.ExpandAgenda(sc, time.UTC, rt("2026-10-01T00:00:00Z"), rt("2026-10-05T00:00:00Z"), rt("2026-10-01T00:00:00Z")); err != nil || unavailable != "cap_scan" || len(es) != 0 {
		t.Errorf("stored error_code: entries=%d unavailable=%q err=%v, want none and cap_scan", len(es), unavailable, err)
	}
}
