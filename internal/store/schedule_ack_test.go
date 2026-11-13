package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const ackAt = "2026-10-30T09:00:00Z"
const ackNow = "2026-10-30T10:00:00Z"

func ackFixture(t *testing.T, rule, anchor string) (*schedFixture, *models.ItemSchedule) {
	t.Helper()
	f := newSchedFixture(t)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC", Rrule: rule, Anchor: anchor})
	f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, ackAt, sc.ID)
	return f, sc
}
func ackInput() ResolveInput {
	return ResolveInput{OccurrenceAt: rt(ackAt), State: "ack", Actor: "user"}
}
func ackJournal(t *testing.T, f *schedFixture, sc *models.ItemSchedule) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='ack'`, sc.ID)
}
func TestResolveOccurrenceAckKeepsOneShotOutstanding(t *testing.T) {
	t.Parallel()
	f, sc := ackFixture(t, "", "calendar")
	before, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := f.mustResolve(t, f.item, ackInput(), ackNow)
	if res.Schedule.EndedAt != nil || !reflect.DeepEqual(res.Schedule.NextAt, sc.NextAt) || !reflect.DeepEqual(res.Schedule.NextWall, sc.NextWall) || res.MissedRecorded != 0 || f.occTotal(t, sc.ID) != 0 {
		t.Fatalf("ack advanced: %+v", res)
	}
	r := f.managed(t, res.Schedule)
	if r.AckedAt == nil || *r.AckedAt != ackNow || r.FiredAt == nil || *r.FiredAt != ackAt {
		t.Fatalf("delivery=%+v", r)
	}
	after, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Roles, after.Roles) || len(after.Journal) != 1 || after.Journal[0].ChangeType != "ack" || ackJournal(t, f, sc) != 1 {
		t.Fatalf("commitment=%+v", after)
	}
}
func TestResolveOccurrenceAckTransitions(t *testing.T) {
	t.Parallel()
	for _, anchor := range []string{"calendar", "completion"} {
		for _, rule := range []string{"", "FREQ=DAILY"} {
			t.Run(anchor+rule, func(t *testing.T) {
				f, sc := ackFixture(t, rule, anchor)
				f.exec(t, `UPDATE item_reminders SET fired_at=NULL WHERE schedule_id=?`, sc.ID)
				if _, err := f.resolve(t, f.item, ackInput(), ackNow); err == nil || !strings.Contains(err.Error(), "has not fired") {
					t.Fatalf("unfired=%v", err)
				}
				f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, ackAt, sc.ID)
				f.mustResolve(t, f.item, ackInput(), ackNow)
				f.mustResolve(t, f.item, ackInput(), "2026-10-30T10:01:00Z")
				if r := f.managed(t, sc); r.AckedAt == nil || *r.AckedAt != ackNow || ackJournal(t, f, sc) != 1 {
					t.Fatalf("duplicate=%+v", r)
				}
				until := rt("2026-10-31T12:00:00Z")
				f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(ackAt), State: "snooze", Until: &until, Actor: "user"}, ackNow)
				if r := f.managed(t, sc); r.AckedAt != nil || r.FiredAt != nil {
					t.Fatalf("snooze=%+v", r)
				}
				if _, err := f.resolve(t, f.item, ackInput(), ackNow); err == nil || !strings.Contains(err.Error(), "has not fired") {
					t.Fatalf("snoozed=%v", err)
				}
				f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, ackNow, sc.ID)
				f.mustResolve(t, f.item, ackInput(), ackNow)
				f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(ackAt), State: "done", Actor: "user"}, ackNow)
				if _, err := f.resolve(t, f.item, ackInput(), ackNow); isStale(err) == nil {
					t.Fatalf("resolved=%v", err)
				}
			})
		}
	}
	for _, state := range []string{"skip", "move"} {
		t.Run(state, func(t *testing.T) {
			f, sc := ackFixture(t, "FREQ=DAILY", "calendar")
			f.mustResolve(t, f.item, ackInput(), ackNow)
			in := ResolveInput{State: state, OccurrenceAt: rt(ackAt), Actor: "user"}
			if state == "move" {
				in.Until = ptr(rt("2026-10-31T12:00:00Z"))
			}
			res := f.mustResolve(t, f.item, in, ackNow)
			if r := f.managed(t, res.Schedule); r.AckedAt != nil || r.FiredAt != nil {
				t.Fatalf("%s=%+v", state, r)
			}
			if ackJournal(t, f, sc) != 1 {
				t.Fatal("lost journal")
			}
		})
	}
}
func TestResolveOccurrenceAckFloatingIdentity(t *testing.T) {
	t.Parallel()
	for _, wall := range []string{"2026-11-01T01:30", "2026-11-01"} {
		t.Run(wall, func(t *testing.T) {
			f := newSchedFixture(t)
			u := f.user(t, "ack-zone@example.com", "America/New_York")
			f.assign(t, f.item.ID, u.ID)
			f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: wall, Floating: true})
			if _, err := f.s.ResolveOccurrence(f.ws.ID, f.item.ID, ResolveInput{State: "ack", OccurrenceAt: rt("2026-11-01T05:30:00Z"), Actor: "user"}); err == nil {
				t.Fatal("early ack accepted")
			}
			fired := f.fire(t, "2026-11-01T16:00:00Z")
			if len(fired) != 1 {
				t.Fatalf("fired=%v", fired)
			}
			sc, _ := f.s.GetSchedule(f.ws.ID, f.item.ID)
			at := *sc.NextAt
			f.exec(t, `UPDATE users SET timezone='Asia/Tokyo' WHERE id=?`, u.ID)
			in := ResolveInput{State: "ack", OccurrenceAt: rt(at), View: time.FixedZone("other", -7*3600), Actor: "user"}
			res := f.mustResolve(t, f.item, in, "2026-11-01T16:01:00Z")
			if *res.Schedule.NextAt != at || ackJournal(t, f, sc) != 1 {
				t.Fatal("identity changed")
			}
		})
	}
}
func TestResolveOccurrenceAckConcurrentWriters(t *testing.T) {
	for _, other := range []string{"ack", "done", "snooze", "lapse"} {
		t.Run(other, func(t *testing.T) {
			f, sc := ackFixture(t, "", "calendar")
			if other == "lapse" {
				f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=1,deadline_behavior='lapses' WHERE id=?`, sc.ID)
			}
			start := make(chan struct{})
			var wg sync.WaitGroup
			errs := make([]error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-start
					in := ackInput()
					if i == 1 {
						in.State = other
						if other == "snooze" {
							in.Until = ptr(rt("2026-10-31T12:00:00Z"))
						}
						if other == "lapse" {
							_, errs[i] = f.s.EvaluateDeadlines(ackNow, 10)
							return
						}
					}
					_, errs[i] = f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, in, rt(ackNow))
				}(i)
			}
			close(start)
			wg.Wait()
			for _, err := range errs {
				if err != nil && isStale(err) == nil && !strings.Contains(err.Error(), "has not fired") {
					t.Fatal(err)
				}
			}
			n := ackJournal(t, f, sc)
			if n > 1 || other == "ack" && (n != 1 || errs[0] != nil || errs[1] != nil) {
				t.Fatalf("journals=%d errors=%v", n, errs)
			}
		})
	}
}
func TestAcknowledgedAgendaRemainsOwed(t *testing.T) {
	t.Parallel()
	f, _ := ackFixture(t, "FREQ=DAILY", "calendar")
	f.mustResolve(t, f.item, ackInput(), ackNow)
	sc, _ := f.s.GetSchedule(f.ws.ID, f.item.ID)
	e, err := f.s.AgendaOutstanding(sc, time.UTC, rt("2026-11-03T12:00:00Z"))
	if err != nil || e == nil || e.State != "acknowledged" || !e.Overdue {
		t.Fatalf("outstanding=%+v err=%v", e, err)
	}
	entries, _, _, err := f.s.ExpandAgenda(sc, time.UTC, rt("2026-10-30T00:00:00Z"), rt("2026-11-04T00:00:00Z"), rt("2026-11-03T12:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.State == "acknowledged" {
			found = true
			if !e.Stale || e.MissedCount < 2 {
				t.Fatalf("carried=%+v", e)
			}
		}
	}
	if !found {
		t.Fatal("acknowledged seed lost")
	}
}
func TestAcknowledgedByProjectsActor(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "ack-actor@example.com", "UTC")
	var ids []string
	for _, actor := range []string{"user", "agent"} {
		it, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
		f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, ackAt, sc.ID)
		in := ackInput()
		in.Actor = actor
		in.ActorUserID = u.ID
		// The agent name travels beside the journal actor kind, rather than replacing it.
		if actor == "agent" {
			in.AgentName = "rook"
		}
		f.mustResolve(t, it, in, ackNow)
		ids = append(ids, it.ID)
	}
	check := func(wantName string) {
		t.Helper()
		q := &agendaQueryCounter{Queryer: f.s.db}
		schedules, _, err := f.s.agendaWorkDetailsQ(q, f.ws.ID, ids)
		if q.queries > 4 || q.singles != 0 {
			t.Fatalf("ack attribution is not batched: %+v", q)
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			e, err := f.s.AgendaOutstanding(schedules[id], time.UTC, rt(ackNow))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(e)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			by, _ := m["AcknowledgedBy"].(map[string]any)
			if by == nil || by["user_id"] != u.ID || by["name"] != wantName {
				t.Fatalf("actor=%s", b)
			}
			if by["actor"] == "agent" && by["agent_name"] != "rook" {
				t.Fatalf("agent=%v", by)
			}
		}
	}
	check(u.Name)
	if err := f.s.DeleteAccountAtomic(u.ID); err != nil {
		t.Fatal(err)
	}
	check("")
}
func TestAcknowledgedDeadlineStillAccountsAndLapses(t *testing.T) {
	t.Parallel()
	for _, behavior := range []string{"stays_owed", "lapses"} {
		t.Run(behavior, func(t *testing.T) {
			f, sc := ackFixture(t, "", "calendar")
			f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=3600,deadline_behavior=? WHERE id=?`, behavior, sc.ID)
			f.mustResolve(t, f.item, ackInput(), "2026-10-30T09:30:00Z")
			got := f.eval(t, "2026-10-30T10:00:01Z", 10)
			if got.Missed != 1 || f.deadlineEvidence(t) != 1 {
				t.Fatalf("miss=%+v", got)
			}
			sc, _ = f.s.GetSchedule(f.ws.ID, f.item.ID)
			if (sc.EndedAt != nil) != (behavior == "lapses") {
				t.Fatalf("schedule=%+v", sc)
			}
		})
	}
}
func TestAcknowledgedLinkedPastMoveDoesNotResurrectAlert(t *testing.T) {
	t.Parallel()
	for _, resolveDoor := range []bool{false, true} {
		t.Run(map[bool]string{false: "graph", true: "resolve"}[resolveDoor], func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.link(t, 1, 0, "target", "target", dayBefore)
			sc := f.scheds[0]
			before := *sc.NextAt
			f.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, "2030-03-04T09:00:00Z", sc.ID)
			f.mustResolve(t, f.items[0], ResolveInput{State: "ack", OccurrenceAt: rt(before), Actor: "user"}, "2030-03-04T10:00:00Z")
			f.atClock(t, "2030-03-20T12:00:00Z")
			if resolveDoor {
				until := rt("2030-03-21T09:00:00Z")
				f.mustResolve(t, f.items[1], ResolveInput{State: "move", OccurrenceAt: rt(*f.scheds[1].NextAt), Until: &until, Actor: "user"}, "2030-03-20T12:00:00Z")
			} else {
				if _, err := f.move(1, "target", "2030-03-21T09:00:00Z", nil); err != nil {
					t.Fatal(err)
				}
			}
			after := f.reload(t, f.items[0].ID)
			if f.reminderRows(t, 0) != 0 || *after.NextAt != before || after.NextOverrideAt == nil {
				t.Fatalf("past move=%+v", after)
			}
		})
	}
}
func TestScheduleAckBundleRoundTrip(t *testing.T) {
	t.Parallel()
	f, sc := ackFixture(t, "", "calendar")
	f.mustResolve(t, f.item, ackInput(), ackNow)
	bundle := f.exportBundle(t)
	ws, rep := f.importBundle(t, bundle, "Ack import")
	if len(rep.DroppedSchedules) > 0 {
		t.Fatalf("report=%+v", rep)
	}
	it := f.itemByTitle(t, ws.ID, f.item.Title)
	imported, err := f.s.GetSchedule(ws.ID, it.ID)
	if err != nil || imported == nil {
		t.Fatalf("schedule=%v %v", imported, err)
	}
	r, err := f.s.GetReminder(ws.ID, *imported.ReminderID)
	if err != nil {
		t.Fatal(err)
	}
	if r.AckedAt == nil || *r.AckedAt != ackNow {
		t.Fatalf("import=%+v", r)
	}
	if ackJournal(t, f, sc) != 1 || ackJournal(t, f, imported) != 1 {
		t.Fatal("ack history lost")
	}
}

func TestScheduleAckBundleValidationAndFreshCopy(t *testing.T) {
	t.Parallel()
	f, sc := ackFixture(t, "FREQ=DAILY", "calendar")
	f.mustResolve(t, f.item, ackInput(), ackNow)
	bundle := f.exportBundle(t)
	for _, tc := range []struct {
		name, mark     string
		unfired, ended bool
	}{{name: "malformed", mark: "yesterday"}, {name: "unfired", mark: ackNow, unfired: true}, {name: "ended", mark: ackNow, ended: true}} {
		b := jsonRoundTrip(t, bundle)
		for i := range b.Schedules {
			b.Schedules[i].Reminder.AckedAt = tc.mark
			if tc.unfired {
				b.Schedules[i].Reminder.FiredAt = ""
			}
			if tc.ended {
				b.Schedules[i].EndedAt = ackNow
				b.Schedules[i].NextAt = ""
				b.Schedules[i].NextWall = ""
			}
		}
		_, rep := f.importBundle(t, b, "Invalid ack "+tc.name)
		if len(rep.DroppedSchedules) == 0 || !strings.HasPrefix(rep.DroppedSchedules[0].Reason, "acked_at:") {
			t.Fatalf("%s report=%+v", tc.name, rep)
		}
	}
	old := jsonRoundTrip(t, bundle)
	for i := range old.Schedules {
		old.Schedules[i].Reminder.AckedAt = ""
	}
	ws, rep := f.importBundle(t, old, "Old ack bundle")
	if len(rep.DroppedSchedules) != 0 {
		t.Fatal(rep)
	}
	it := f.itemByTitle(t, ws.ID, f.item.Title)
	got, err := f.s.GetSchedule(ws.ID, it.ID)
	if err != nil || got.ReminderAckedAt != nil {
		t.Fatalf("old mark=%+v err=%v", got, err)
	}
	if ackJournal(t, f, sc) != 1 {
		t.Fatal("source changed")
	}
	c := newSchedCopy(t)
	source := c.schedule(t, daily())
	c.forcePast(t, source)
	c.exec(t, `UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`, "2026-10-28T09:00:00Z", source.ID)
	if _, err := c.s.resolveOccurrenceAt(c.wsA.ID, c.src.ID, ResolveInput{State: "ack", OccurrenceAt: rt("2026-10-28T09:00:00Z"), Actor: "user"}, rt("2026-10-28T10:00:00Z")); err != nil {
		t.Fatal(err)
	}
	copied := c.do(t, false)
	fresh := c.sched(t, c.wsB.ID, copied.Item.ID)
	if copied.Schedule.ChangesDropped < 2 {
		t.Fatalf("copy did not report dropped ack history: %+v", copied.Schedule)
	}
	if fresh == nil || fresh.ReminderAckedAt != nil || fresh.ReminderFired {
		t.Fatalf("copy inherited ack=%+v", fresh)
	}
}
