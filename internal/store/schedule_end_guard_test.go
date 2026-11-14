package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4537: a done or skip that would end the schedule of an open item is
// refused unless the caller consents. Runs on SQLite in make test and on
// Postgres in make test-pg.

// endGuardCounts is every table a refused resolve must leave untouched.
func (f *schedFixture) endGuardCounts(t *testing.T, item *models.Item, sc *models.ItemSchedule) [5]int {
	t.Helper()
	return [5]int{
		f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=?`, sc.ID),
		f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=?`, sc.ID),
		f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id=?`, item.ID),
		f.count(t, `SELECT COUNT(*) FROM activities WHERE document_id=?`, item.ID),
		f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=?`, sc.ID),
	}
}

func (f *schedFixture) outstanding(t *testing.T, item *models.Item) time.Time {
	t.Helper()
	sc := f.reload(t, item.ID)
	x, ok, err := OutstandingInstant(sc, nil)
	if err != nil || !ok {
		t.Fatalf("no outstanding occurrence: %v %+v", err, sc)
	}
	return x
}

func (f *schedFixture) closeItem(t *testing.T, item *models.Item, status string) {
	t.Helper()
	f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"`+status+`"}`, item.ID)
}

// lastOccurrenceCase sets up a schedule whose next done or skip has no next
// occurrence, and returns the instant to resolve at.
type lastOccurrenceCase struct {
	name    string
	in      models.ScheduleCreate
	created string
	// prepare drives the schedule to its last occurrence and returns the resolve clock.
	prepare func(t *testing.T, f *schedFixture, item *models.Item) string
	state   string
}

func lastOccurrenceCases() []lastOccurrenceCase {
	oneShot := models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}
	at := func(now string) func(*testing.T, *schedFixture, *models.Item) string {
		return func(*testing.T, *schedFixture, *models.Item) string { return now }
	}
	advance := func(now string) func(*testing.T, *schedFixture, *models.Item) string {
		return func(t *testing.T, f *schedFixture, item *models.Item) string {
			f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user"}, "2026-10-30T09:05:00Z")
			return now
		}
	}
	return []lastOccurrenceCase{
		{"one-shot done", oneShot, "2026-10-29T12:00:00Z", at("2026-10-30T09:05:00Z"), "done"},
		{"one-shot skip", oneShot, "2026-10-29T12:00:00Z", at("2026-10-30T09:05:00Z"), "skip"},
		{"COUNT=2 last", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;COUNT=2"}, "2026-10-29T12:00:00Z", advance("2026-10-31T09:05:00Z"), "done"},
		{"UNTIL last", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;UNTIL=20261031T090000Z"}, "2026-10-29T12:00:00Z", advance("2026-10-31T09:05:00Z"), "skip"},
		{"COUNT=3 resolved while two passed", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;COUNT=3"}, "2026-10-29T12:00:00Z", at("2026-11-02T09:05:00Z"), "done"},
		{"completion with UNTIL", models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: "FREQ=DAILY;UNTIL=20261031T080000Z", Anchor: "completion"}, "2026-10-29T12:00:00Z", at("2026-10-30T09:05:00Z"), "done"},
		{"past-moved one-shot", oneShot, "2026-10-29T12:00:00Z", func(t *testing.T, f *schedFixture, item *models.Item) string {
			until := rt("2026-10-30T09:30:00Z")
			f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "move", Until: &until, Actor: "user"}, "2026-10-30T10:00:00Z")
			return "2026-10-30T10:05:00Z"
		}, "done"},
	}
}

func TestResolveLastOccurrenceOpenItemRefused(t *testing.T) {
	t.Parallel()
	for _, c := range lastOccurrenceCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			u := f.user(t, "guard@example.com", "")
			item, sc := f.mk(t, c.in, c.created)
			now := c.prepare(t, f, item)
			before := f.reload(t, item.ID)
			counts := f.endGuardCounts(t, item, sc)
			_, err := f.resolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: c.state, Actor: "user", Comment: f.comment(item, u.ID, "finished")}, now)
			var end *ErrScheduleWouldEnd
			if !errors.As(err, &end) {
				t.Fatalf("resolve = %v, want ErrScheduleWouldEnd", err)
			}
			if after := f.reload(t, item.ID); !reflect.DeepEqual(after, before) {
				t.Fatalf("the schedule changed:\n%+v\n%+v", before, after)
			}
			if got := f.endGuardCounts(t, item, sc); got != counts {
				t.Fatalf("a refused resolve wrote: %v -> %v", counts, got)
			}
		})
	}
}

func TestResolveLastOccurrenceEndScheduleRecordsConsent(t *testing.T) {
	t.Parallel()
	for _, c := range lastOccurrenceCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			item, sc := f.mk(t, c.in, c.created)
			now := c.prepare(t, f, item)
			fired := f.reload(t, item.ID).ReminderID != nil
			if fired {
				f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, "2026-10-30T09:00:00Z", sc.ID)
			}
			res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: c.state, Actor: "user", EndSchedule: true}, now)
			if !res.EndedItemOpen || res.Schedule.EndedAt == nil || res.Schedule.EndedDismissedAt != nil {
				t.Fatalf("result = %+v", res)
			}
			var reason, source, beforeJSON string
			if err := f.s.db.QueryRow(f.s.q(`SELECT reason, reason_source, before_json FROM schedule_changes WHERE schedule_id=? AND change_type=? ORDER BY recorded_at DESC LIMIT 1`), sc.ID, c.state).Scan(&reason, &source, &beforeJSON); err != nil {
				t.Fatal(err)
			}
			if reason != "ended_item_open" || source != "system" {
				t.Fatalf("journal reason=%q source=%q", reason, source)
			}
			var snap scheduleDateSnapshot
			if err := json.Unmarshal([]byte(beforeJSON), &snap); err != nil {
				t.Fatal(err)
			}
			if fired && (snap.ReminderFiredAt == nil || *snap.ReminderFiredAt != "2026-10-30T09:00:00Z" || snap.ReminderRemindAt == nil) {
				t.Fatalf("before snapshot lost the deleted reminder: %s", beforeJSON)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=?`, sc.ID); n != 0 {
				t.Fatalf("managed rows = %d", n)
			}
		})
	}
}

func TestResolveLastOccurrenceTerminalItemUnguarded(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"done", "skip"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			f := newSchedFixture(t)
			item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
			f.closeItem(t, item, "done")
			res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: state, Actor: "user"}, "2026-10-30T09:05:00Z")
			if res.EndedItemOpen || res.Schedule.EndedAt == nil {
				t.Fatalf("result = %+v", res)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND reason='ended_item_open'`, sc.ID); n != 0 {
				t.Fatal("a closed item's ending was recorded as consent")
			}
		})
	}
	// A next occurrence exists: the flag changes nothing.
	f := newSchedFixture(t)
	item, _ := f.mk(t, dailyUTC, "2026-10-29T12:00:00Z")
	res := f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T09:05:00Z")
	if res.EndedItemOpen || res.Schedule.EndedAt != nil {
		t.Fatalf("recurring = %+v", res)
	}
}

// The guard reads the item under the pin: a close or reopen that commits
// before the resolve takes it decides the outcome, including one that commits
// while the resolve waits on the lock.
func TestResolveWouldEndRacesItemClose(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T, status string) (*schedFixture, *models.Item, time.Time) {
		f := newSchedFixture(t)
		item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
		f.closeItem(t, item, status)
		return f, item, f.outstanding(t, item)
	}
	in := func(x time.Time) ResolveInput { return ResolveInput{OccurrenceAt: x, State: "done", Actor: "user"} }
	const now = "2026-10-30T09:05:00Z"

	t.Run("close commits before the pin", func(t *testing.T) {
		f, item, x := setup(t, "open")
		restore := f.s.SetBeforeCommitmentPinHookForTesting(func() { f.closeItem(t, item, "done") })
		defer restore()
		res := f.mustResolve(t, item, in(x), now)
		if res.EndedItemOpen || res.Schedule.EndedAt == nil {
			t.Fatalf("result = %+v", res)
		}
	})
	t.Run("resolve before the close", func(t *testing.T) {
		f, item, x := setup(t, "open")
		var end *ErrScheduleWouldEnd
		if _, err := f.resolve(t, item, in(x), now); !errors.As(err, &end) {
			t.Fatalf("resolve = %v", err)
		}
		f.closeItem(t, item, "done")
		if res := f.mustResolve(t, item, in(x), now); res.Schedule.EndedAt == nil || res.EndedItemOpen {
			t.Fatalf("retry = %+v", res)
		}
	})
	waiting := func(t *testing.T, from, to string, wantRefused bool) {
		f, item, x := setup(t, from)
		var wg sync.WaitGroup
		var commitErr error
		restore := f.s.SetBeforeCommitmentPinHookForTesting(func() {
			// The status write holds the item row (Postgres) or the write lock
			// (SQLite) until it commits, so the resolve waits on its pin.
			tx, err := f.s.db.Begin()
			if err != nil {
				commitErr = err
				return
			}
			if _, err := tx.Exec(f.s.q(`UPDATE items SET fields=? WHERE id=?`), `{"status":"`+to+`"}`, item.ID); err != nil {
				tx.Rollback()
				commitErr = err
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				time.Sleep(300 * time.Millisecond)
				commitErr = tx.Commit()
			}()
		})
		defer restore()
		_, err := f.resolve(t, item, in(x), now)
		wg.Wait()
		if commitErr != nil {
			t.Fatal(commitErr)
		}
		var end *ErrScheduleWouldEnd
		if refused := errors.As(err, &end); refused != wantRefused || (!refused && err != nil) {
			t.Fatalf("resolve = %v, want refused=%v", err, wantRefused)
		}
	}
	t.Run("close commits while the resolve waits on the pin", func(t *testing.T) { waiting(t, "open", "done", false) })
	t.Run("reopen commits while the resolve waits on the pin", func(t *testing.T) { waiting(t, "done", "open", true) })
}

func TestLapseEndsOneShotUnguarded(t *testing.T) {
	t.Parallel()
	f, sc := ackFixture(t, "", "calendar")
	f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=1,deadline_behavior='lapses' WHERE id=?`, sc.ID)
	if _, err := f.s.EvaluateDeadlines(ackNow, 10); err != nil {
		t.Fatal(err)
	}
	got := f.reload(t, f.item.ID)
	if got.EndedAt == nil || got.EndedDismissedAt != nil {
		t.Fatalf("lapse = %+v", got)
	}
}

func TestDismissEndedSchedule(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	if _, err := f.s.DismissEndedSchedule(f.ws.ID, f.item.ID); !errors.Is(err, ErrScheduleNotFound) {
		t.Fatalf("no schedule = %v", err)
	}
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	if _, err := f.s.DismissEndedSchedule(f.ws.ID, item.ID); !errors.Is(err, ErrScheduleNotEnded) {
		t.Fatalf("live = %v", err)
	}
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T09:05:00Z")
	changed, err := f.s.DismissEndedSchedule(f.ws.ID, item.ID)
	if err != nil || !changed {
		t.Fatalf("dismiss = %v %v", changed, err)
	}
	first := f.reload(t, item.ID)
	if first.EndedDismissedAt == nil {
		t.Fatal("not dismissed")
	}
	changed, err = f.s.DismissEndedSchedule(f.ws.ID, item.ID)
	if err != nil || changed {
		t.Fatalf("again = %v %v", changed, err)
	}
	if again := f.reload(t, item.ID); !reflect.DeepEqual(again, first) {
		t.Fatal("an idempotent dismissal wrote")
	}
}

func TestTimingRevivalClearsDismissal(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T09:05:00Z")
	if _, err := f.s.DismissEndedSchedule(f.ws.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	// An edit that leaves it ended keeps the dismissal.
	audience := "human"
	if _, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{Audience: &audience}, rt("2026-10-30T10:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if sc := f.reload(t, item.ID); sc.EndedAt == nil || sc.EndedDismissedAt == nil {
		t.Fatalf("kept = %+v", sc)
	}
	start := "2026-11-05T09:00"
	sc, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{Dtstart: &start}, rt("2026-10-30T10:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if sc.EndedAt != nil || sc.EndedDismissedAt != nil {
		t.Fatalf("revived = %+v", sc)
	}
}

func TestCarryMoveDismissesSourceEnd(t *testing.T) {
	t.Parallel()
	c := newSchedCopy(t)
	c.schedule(t, daily())
	c.do(t, true)
	a := c.sched(t, c.wsA.ID, c.src.ID)
	if a == nil || a.EndedAt == nil || a.EndedDismissedAt == nil || *a.EndedDismissedAt != *a.EndedAt {
		t.Fatalf("source schedule = %+v, want ended and dismissed", a)
	}
}

func TestScheduleEndDismissMigration(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	if _, err := f.s.db.Exec(f.s.q(`UPDATE item_schedules SET ended_dismissed_at=? WHERE id=?`), "2026-10-30T10:00:00Z", sc.ID); err == nil {
		t.Fatal("a live schedule took a dismissal")
	}
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T09:05:00Z")
	f.exec(t, `UPDATE item_schedules SET ended_dismissed_at=? WHERE id=?`, "2026-10-30T10:00:00Z", sc.ID)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE item_schedules SET ended_at=NULL, next_at=?, next_wall=? WHERE id=?`), "2026-11-01T09:00:00Z", "2026-11-01T09:00:00", sc.ID); err == nil {
		t.Fatal("a dismissal outlived its ending")
	}
	if _, ok := nulExcluded["item_schedules.ended_dismissed_at"]; !ok {
		t.Fatal("ended_dismissed_at is not classified for the NUL census")
	}
}

func TestScheduleEndDismissBundleRoundTrip(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T09:05:00Z")
	if _, err := f.s.DismissEndedSchedule(f.ws.ID, item.ID); err != nil {
		t.Fatal(err)
	}
	want := f.reload(t, item.ID).EndedDismissedAt
	bad := f.another(t, "bad dismissal")
	data := f.exportBundle(t)
	byTitle := map[string]string{}
	for _, it := range data.Items {
		byTitle[it.Title] = it.ID
	}
	x := validSchedExport(byTitle[bad.Title])
	x.ID, x.EndedDismissedAt = "bad-dismissal-schedule", "2026-10-30T10:00:00Z"
	data.Schedules = append(data.Schedules, x)
	dst, rep := f.importBundle(t, jsonRoundTrip(t, data), "Dismiss trip")
	got := f.reload2(t, dst.ID, f.itemByTitle(t, dst.ID, item.Title).ID)
	if got == nil || got.EndedAt == nil || got.EndedDismissedAt == nil || *got.EndedDismissedAt != *want {
		t.Fatalf("imported = %+v, want dismissed at %v", got, *want)
	}
	dropped := false
	for _, d := range rep.DroppedSchedules {
		dropped = dropped || strings.Contains(d.Reason, "ended_dismissed_at:combination")
	}
	if !dropped {
		t.Fatalf("a dismissal without an ending was imported: %+v", rep.DroppedSchedules)
	}
}

func (f *schedFixture) reload2(t *testing.T, wsID, itemID string) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.GetSchedule(wsID, itemID)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func TestScheduleEndImportDismissalPresence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, key, want string }{
		{"legacy missing", "", "2026-10-30T09:05:00Z"},
		{"explicit dismissal", `,"ended_dismissed_at":"2026-10-30T10:00:00Z"`, "2026-10-30T10:00:00Z"},
		{"explicit empty", `,"ended_dismissed_at":""`, ""},
		{"explicit null", `,"ended_dismissed_at":null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			data := f.exportBundle(t)
			var sc models.ScheduleExport
			raw := `{"id":"legacy","item_id":"` + f.item.ID + `","kind":"check","dtstart":"2026-10-30T09:00:00","tz":"UTC","anchor":"calendar","audience":"both","ended_at":"2026-10-30T09:05:00Z","created_at":"2026-10-29T12:00:00Z","updated_at":"2026-10-30T09:05:00Z"` + tc.key + `}`
			if err := json.Unmarshal([]byte(raw), &sc); err != nil {
				t.Fatal(err)
			}
			data.Schedules = []models.ScheduleExport{sc}
			dst, rep := f.importBundle(t, data, "Import dismissal")
			if len(rep.DroppedSchedules) != 0 {
				t.Fatalf("dropped: %+v", rep)
			}
			got := f.reload2(t, dst.ID, f.itemByTitle(t, dst.ID, f.item.Title).ID)
			if got == nil || derefStr(got.EndedDismissedAt) != tc.want {
				t.Fatalf("dismissal = %+v, want %q", got, tc.want)
			}
		})
	}
	// A current export must preserve an intentionally undismissed ending.
	f := newSchedFixture(t)
	item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user", EndSchedule: true}, "2026-10-30T09:05:00Z")
	dst, _ := f.importBundle(t, f.exportBundle(t), "Current ending")
	if got := f.reload2(t, dst.ID, f.itemByTitle(t, dst.ID, item.Title).ID); got == nil || got.EndedDismissedAt != nil {
		t.Fatalf("current ending changed: %+v", got)
	}
}
