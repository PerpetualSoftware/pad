package store

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func reopenState(t *testing.T, s *Store) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, table := range []string{"items", "item_schedules", "item_reminders", "schedule_occurrences", "schedule_occurrence_overrides", "milestone_completion_evidence", "schedule_changes", "activities", "comments", "schedule_reopen_checkpoints", "schedule_links", "schedule_gates", "user_notifications", "event_outbox", "milestone_children", "schedule_forecasts", "schedule_forecast_events"} {
		query := table
		if table == "schedule_reopen_checkpoints" || table == "schedule_forecasts" {
			query = "(SELECT *,schedule_id AS id FROM " + table + ")"
		}
		out[table] = renegotiateRows(t, s, query, "")
	}
	return out
}

func reopenSnapshot(t *testing.T, s *Store, id string) string {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	v, err := s.scheduleSnapshotTx(tx, id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestReopenRestoresResolution(t *testing.T) {
	cases := []struct {
		name, state, rule, anchor, start, zone, now, until string
		floating, ack, linked                              bool
	}{
		{name: "done", state: "done"}, {name: "skip", state: "skip"},
		{name: "snooze", state: "snooze", until: "2026-10-30T15:00:00Z"},
		{name: "future move", state: "move", until: "2026-10-30T15:00:00Z"},
		{name: "past move", state: "move", until: "2026-10-29T15:00:00Z"},
		{name: "move route", state: "move", until: "2026-10-30T15:00:00Z", linked: true},
		{name: "past move route", state: "move", until: "2026-10-29T15:00:00Z", linked: true},
		{name: "completion anchor", state: "done", anchor: "completion"},
		{name: "one shot", state: "done", rule: "one"},
		{name: "count exhausted", state: "done", rule: "FREQ=DAILY;COUNT=1"},
		{name: "until exhausted", state: "skip", rule: "FREQ=DAILY;UNTIL=20261030T090000Z"},
		{name: "all day", state: "done", start: "2026-10-30"},
		{name: "floating", state: "done", floating: true},
		{name: "DST", state: "done", start: "2026-11-01T01:30", zone: "America/New_York", now: "2026-11-01T08:00:00Z"},
		{name: "ack before done", state: "done", ack: true},
		{name: "past move no next", state: "move", rule: "one", until: "2026-10-29T15:00:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			if tc.rule != "" {
				in.Rrule = tc.rule
			}
			if tc.rule == "one" {
				in.Rrule = ""
			}
			if tc.anchor != "" {
				in.Anchor = tc.anchor
			}
			if tc.start != "" {
				in.Dtstart = tc.start
			}
			if tc.zone != "" {
				in.Tz = tc.zone
			}
			if tc.floating {
				in.Floating = true
				in.Tz = ""
			}
			item, sc := f.mk(t, in, "2026-10-29T00:00:00Z")
			occurrence, _, err := OutstandingInstant(sc, time.UTC)
			if err != nil {
				t.Fatal(err)
			}
			now := "2026-10-30T10:00:00Z"
			if tc.now != "" {
				now = tc.now
			}
			if tc.ack {
				f.fire(t, "2026-10-30T09:00:00Z")
				f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "ack", Actor: "user"}, "2026-10-30T09:01:00Z")
			}
			before := reopenSnapshot(t, f.s, sc.ID)
			rid := *sc.ReminderID
			var id string
			if tc.linked {
				f.s.linkClock = func() time.Time { return rt(now) }
				_, err := f.s.MoveLinkedDate(f.ws.ID, models.LinkedMoveInput{ItemID: item.ID, Role: "target", To: tc.until, Actor: "user", Access: models.CommitmentAccess{Readable: map[string]bool{item.ID: true}, Editable: map[string]bool{item.ID: true}}})
				if err != nil {
					t.Fatal(err)
				}
				view, err := f.s.GetCommitment(f.ws.ID, item.ID)
				if err != nil {
					t.Fatal(err)
				}
				id = view.LastResolution.ResolutionID
			} else {
				r := ResolveInput{OccurrenceAt: occurrence, State: tc.state, Actor: "user", EndSchedule: true}
				if tc.until != "" {
					v := rt(tc.until)
					r.Until = &v
				}
				res := f.resolveForReopenTest(t, item, r, now)
				id = res.ResolutionID
				if tc.name == "past move no next" && res.Schedule.EndedAt != nil {
					t.Fatal("past move ended schedule")
				}
			}
			if id == "" {
				t.Fatal("missing exact resolution ID")
			}
			res, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: id, Actor: "user"}, rt(now).Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if got := reopenSnapshot(t, f.s, sc.ID); got != before {
				t.Fatalf("restored state differs\nbefore %s\nafter %s", before, got)
			}
			restored := f.reload(t, item.ID)
			if restored.EndedAt != nil || restored.ReminderID == nil || *restored.ReminderID != rid {
				t.Fatalf("not live with original reminder: %+v", restored)
			}
			if tc.linked {
				view, err := f.s.GetCommitment(f.ws.ID, item.ID)
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, c := range view.Journal {
					if c.ID == id {
						found = true
						if c.ReversedBy != res.ChangeID {
							t.Fatal("move reversal annotation missing")
						}
					}
				}
				if !found {
					t.Fatal("move history missing")
				}
			}
			if res.ResolutionID != id {
				t.Fatal("wrong identity")
			}
		})
	}
}

func TestReopenRestoresOnlyOwnedEffects(t *testing.T) {
	f := newSchedFixture(t)
	u := f.user(t, "reopen@example.com", "")
	item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
	until := rt("2026-10-30T12:00:00Z")
	f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "move", Until: &until, Actor: "user"}, "2026-10-30T08:00:00Z")
	f.fire(t, "2026-10-30T12:00:00Z")
	f.exec(t, `INSERT INTO schedule_occurrences(id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES(?,?,?, ?,'missed','',?)`, newID(), sc.ID, "2026-10-31T09:00:00Z", "2026-10-31T09:00:00", "2026-11-01T10:00:00Z")
	before := reopenSnapshot(t, f.s, sc.ID)
	occurrences := renegotiateRows(t, f.s, "schedule_occurrences", "")
	overrides := renegotiateRows(t, f.s, "schedule_occurrence_overrides", "")
	res := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user", Comment: f.comment(item, u.ID, "Keep this comment")}, "2026-11-02T10:00:00Z")
	if res.MissedRecorded == 0 {
		t.Fatal("fixture needs inserted misses")
	}
	f.s.mustExec(t, `UPDATE comments SET body='Edited' WHERE id=?`, res.Comment.ID)
	got, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user", ActorUserID: u.ID}, rt("2026-11-02T10:01:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got.MissedRemoved != res.MissedRecorded {
		t.Fatalf("misses removed %d want %d", got.MissedRemoved, res.MissedRecorded)
	}
	if reopenSnapshot(t, f.s, sc.ID) != before || renegotiateRows(t, f.s, "schedule_occurrences", "") != occurrences || renegotiateRows(t, f.s, "schedule_occurrence_overrides", "") != overrides {
		t.Fatal("lost preceding state")
	}
	if f.count(t, `SELECT COUNT(*) FROM comments WHERE id=? AND body='Edited'`, res.Comment.ID) != 1 {
		t.Fatal("comment not kept")
	}
}

func TestReopenRefusalsAreAtomic(t *testing.T) {
	for _, name := range []string{"old ID", "later resolution", "reminder fire", "item seq", "close item too", "schedule edit", "edit back", "renegotiation", "dismissal", "unsupported ack", "link added", "gate added", "dependency removed", "deadline passed", "schedule replaced"} {
		t.Run(name, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			if name == "deadline passed" || name == "renegotiation" {
				f.s.mustExec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=7200,deadline_behavior='lapses' WHERE id=?`, sc.ID)
			}
			res := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
			id, code := res.ResolutionID, "reopen_conflict"
			switch name {
			case "old ID":
				id = newID()
			case "later resolution":
				f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*res.Schedule.NextAt), State: "skip", Actor: "user"}, "2026-10-30T10:01:00Z")
			case "reminder fire":
				f.fire(t, "2026-10-31T09:01:00Z")
			case "item seq":
				if _, err := f.s.UpdateItem(item.ID, models.ItemUpdate{Title: ptr("Changed title")}); err != nil {
					t.Fatal(err)
				}
			case "close item too":
				if _, err := f.s.UpdateItem(item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
					t.Fatal(err)
				}
			case "schedule edit", "edit back":
				if _, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{LeadMinutes: i(15)}, rt("2026-10-30T10:01:00Z")); err != nil {
					t.Fatal(err)
				}
				if name == "edit back" {
					if _, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{LeadMinutes: i(0)}, rt("2026-10-30T10:02:00Z")); err != nil {
						t.Fatal(err)
					}
				}
			case "renegotiation":
				if err := f.s.renegotiateDeadlineAt(f.ws.ID, item.ID, "user", rt("2026-10-31T18:00:00Z"), "New agreement", rt("2026-10-30T10:01:00Z")); err != nil {
					t.Fatal(err)
				}
			case "dismissal":
				f.s.mustExec(t, `UPDATE item_schedules SET next_at=NULL,next_wall=NULL,ended_at='2026-10-30T10:01:00Z',ended_dismissed_at='2026-10-30T10:01:00Z' WHERE id=?`, sc.ID)
			case "unsupported ack":
				f.fire(t, "2026-10-31T09:00:00Z")
				r := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*res.Schedule.NextAt), State: "ack", Actor: "user"}, "2026-10-31T09:01:00Z")
				id = r.ResolutionID
				code = "reopen_not_supported"
			case "link added", "dependency removed":
				_, other := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
				tx, err := f.s.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				err = f.s.insertLinkTx(tx, f.ws.ID, linkRow{ID: newID(), ParentSchedule: sc.ID, ParentRole: "target", ChildSchedule: other.ID, ChildRole: "target", Offset: models.ScheduleLinkOffset{Mode: "elapsed"}, CancelPolicy: "waiting_on", CreatedAt: "x", UpdatedAt: "x"})
				if err != nil {
					tx.Rollback()
					t.Fatal(err)
				}
				if err = tx.Commit(); err != nil {
					t.Fatal(err)
				}
				code = "reopen_dependency"
				if name == "dependency removed" {
					f.s.mustExec(t, `DELETE FROM schedule_links WHERE parent_schedule_id=?`, sc.ID)
				}
			case "gate added":
				other := f.another(t, "blocker")
				f.s.mustExec(t, `INSERT INTO schedule_gates(id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES(?,?,?,?,?)`, newID(), f.ws.ID, item.ID, other.ID, "2026-10-30T10:01:00Z")
				code = "reopen_dependency"
			case "deadline passed":
				code = "reopen_deadline_passed"
			case "schedule replaced":
				f.s.mustExec(t, `DELETE FROM item_schedules WHERE id=?`, sc.ID)
				_, err := f.s.createScheduleAt(f.ws.ID, item.ID, dailyUTC, rt("2026-10-29T00:00:00Z"))
				if err != nil {
					t.Fatal(err)
				}
				code = "reopen_unavailable"
			}
			before := reopenState(t, f.s)
			_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: id, Actor: "user"}, rt("2026-10-30T12:01:00Z"))
			var refusal *ReopenError
			if !errors.As(err, &refusal) || refusal.Code != code {
				t.Fatalf("error %v want %s", err, code)
			}
			if after := reopenState(t, f.s); !reflect.DeepEqual(after, before) {
				for table, v := range before {
					if after[table] != v {
						t.Errorf("refusal changed %s", table)
					}
				}
			}
		})
	}
}

func TestReopenHistoryAndConsumption(t *testing.T) {
	f := newSchedFixture(t)
	u := f.user(t, "original@example.com", "")
	v := f.user(t, "reverser@example.com", "")
	item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
	res := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user", ActorUserID: u.ID, Comment: f.comment(item, u.ID, "Resolution's own comment")}, "2026-10-30T10:00:00Z")
	got, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user", ActorUserID: v.ID}, rt("2026-10-30T10:01:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE id=? AND parent_change_id=? AND change_type='reopen'`, got.ChangeID, res.ResolutionID) != 1 {
		t.Fatal("missing lineage")
	}
	if f.count(t, `SELECT COUNT(*) FROM activities WHERE document_id=? AND action='schedule_reopened' AND user_id=?`, item.ID, v.ID) != 1 {
		t.Fatal("missing transactional actor activity")
	}
	before := reopenState(t, f.s)
	_, err = f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID}, rt("2026-10-30T10:02:00Z"))
	var refusal *ReopenError
	if !errors.As(err, &refusal) || refusal.Code != "reopen_conflict" {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, reopenState(t, f.s)) {
		t.Fatal("second reopen wrote")
	}
	res = f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user"}, "2026-10-30T10:03:00Z")
	if _, err = f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt("2026-10-30T10:04:00Z")); err != nil {
		t.Fatal(err)
	}
}

func TestReopenConcurrentDoubleReopen(t *testing.T) {
	f := newSchedFixture(t)
	item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
	res := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt("2026-10-30T10:01:00Z"))
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	success, conflict := 0, 0
	for err := range errs {
		if err == nil {
			success++
		} else {
			var r *ReopenError
			if errors.As(err, &r) && r.Code == "reopen_conflict" {
				conflict++
			} else {
				t.Fatal(err)
			}
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}

// Hold the same pin as each writer, then make reopen wait before it can
// acquire child rows. The writer must still be able to take FOR UPDATE there.
func TestReopenPostgresSerializesWriters(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	for _, writer := range []string{"resolve", "fire", "edit", "delete", "relationship"} {
		t.Run(writer, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			_, other := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			r := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
			tx, err := f.s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if err = f.s.pinScheduleTx(tx, f.ws.ID, item.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: r.ResolutionID, Actor: "user"}, rt("2026-10-30T10:02:00Z"))
				done <- err
			}()
			waitForLockWait(t, f.s, "FOR NO KEY UPDATE OF", done)
			code := "reopen_conflict"
			switch writer {
			case "resolve":
				cur, e := f.s.getScheduleQ(tx, f.ws.ID, item.ID, false)
				if e != nil {
					t.Fatal(e)
				}
				before, e := f.s.scheduleSnapshotTx(tx, sc.ID)
				if e != nil {
					t.Fatal(e)
				}
				_, err = f.s.closeOccurrenceTx(tx, cur, ResolveInput{State: "skip", Actor: "user"}, nil, rt(*cur.NextAt), rt("2026-10-31T10:00:00Z"), rt("2026-10-31T10:00:00Z"), "recorded_now", "", before)
			case "fire":
				// Fire takes the reminder row for update only after the pin.
				_, err = tx.Exec(f.s.q(`SELECT id FROM item_reminders WHERE schedule_id=? FOR UPDATE`), sc.ID)
				if err == nil {
					_, err = tx.Exec(f.s.q(`UPDATE item_reminders SET fired_at=? WHERE schedule_id=?`), "2026-10-31T09:00:00Z", sc.ID)
				}
			case "edit":
				err = f.s.journalScheduleChangeTx(tx, sc.ID, "update", "", "", "", "", "user")
			case "delete":
				_, err = tx.Exec(f.s.q(`DELETE FROM item_schedules WHERE id=?`), sc.ID)
			case "relationship":
				err = f.s.insertLinkTx(tx, f.ws.ID, linkRow{ID: newID(), ParentSchedule: sc.ID, ParentRole: "target", ChildSchedule: other.ID, ChildRole: "target", Offset: models.ScheduleLinkOffset{Mode: "elapsed"}, CancelPolicy: "waiting_on", CreatedAt: "x", UpdatedAt: "x"})
				code = "reopen_dependency"
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("reopen lock deadlock")
			}
			if writer == "delete" {
				if !errors.Is(err, ErrScheduleNotFound) {
					t.Fatal(err)
				}
			} else {
				var refusal *ReopenError
				if !errors.As(err, &refusal) || refusal.Code != code {
					t.Fatalf("%v want %s", err, code)
				}
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='reopen'`, sc.ID) != 0 {
				t.Fatal("partial reopen history")
			}
		})
	}
	t.Run("double reopen", TestReopenConcurrentDoubleReopen)
}

func TestReopenDeadlineAndHistoryDone(t *testing.T) {
	for _, behavior := range []string{"stays_owed", "lapses"} {
		t.Run(behavior, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := lapseSchedule(t, f, false)
			f.exec(t, `UPDATE item_schedules SET deadline_behavior=? WHERE id=?`, behavior, sc.ID)
			f.eval(t, lapseNow, 0)
			if behavior == "lapses" {
				view, err := f.s.GetCommitment(f.ws.ID, sc.ItemID)
				if err != nil {
					t.Fatal(err)
				}
				if view.LastResolution == nil || view.LastResolution.ReopenCode != "reopen_not_supported" {
					t.Fatalf("lapse candidate %+v", view.LastResolution)
				}
				before := reopenState(t, f.s)
				_, err = f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: view.LastResolution.ResolutionID}, rt(lapseNow))
				var refusal *ReopenError
				if !errors.As(err, &refusal) || refusal.Code != "reopen_not_supported" || !reflect.DeepEqual(before, reopenState(t, f.s)) {
					t.Fatalf("lapse refused atomically: %v", err)
				}
			}
			item, err := f.s.GetItem(sc.ItemID)
			if err != nil {
				t.Fatal(err)
			}
			actual := rt("2026-10-26T08:00:00Z")
			result := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: "done", Actor: "user", ActualDoneAt: &actual, EndSchedule: true}, lapseNow)
			if behavior == "lapses" && (result.ScheduleLastResolution == nil || !result.ScheduleLastResolution.CanReopen || result.ScheduleLastResolution.State != "history_done") {
				t.Fatalf("history done availability %+v", result.ScheduleLastResolution)
			}
			_, err = f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: result.ResolutionID, Actor: "user"}, rt(lapseNow).Add(time.Minute))
			if err != nil {
				t.Fatal(err)
			}
			if behavior == "lapses" {
				assertHistoryLapsedAgain(t, f, sc.ID, deadlineOccAt)
			}
			if f.deadlineEvidence(t) != 1 {
				t.Fatal("genuine miss removed")
			}
			view, err := f.s.GetCommitment(f.ws.ID, sc.ItemID)
			if err != nil {
				t.Fatal(err)
			}
			imported, _ := f.importBundle(t, f.exportBundle(t), "Imported reversal")
			var importedItem string
			if err := f.s.db.QueryRow(f.s.q(`SELECT item_id FROM item_schedules WHERE workspace_id=?`), imported.ID).Scan(&importedItem); err != nil {
				t.Fatal(err)
			}
			importedView, err := f.s.GetCommitment(imported.ID, importedItem)
			if err != nil {
				t.Fatal(err)
			}
			if importedView.LastResolution.CanReopen {
				t.Fatal("imported checkpoint")
			}
			for _, candidate := range []*models.Commitment{view, importedView} {
				corrected := false
				for _, row := range candidate.Journal {
					if row.ChangeType == "done" && row.Role != nil && *row.Role == "deadline" {
						corrected = true
						if row.ReversedBy == "" {
							t.Fatal("correction still presented as current")
						}
					}
				}
				if !corrected {
					t.Fatal("correction history disappeared")
				}
			}
		})
	}
}

func TestReopenMilestoneEvidence(t *testing.T) {
	m := newMsFix(t)
	a := m.child(t, m.declared, "Shipped")
	m.sel(t, a)
	m.setStatus(t, a, "shipped")
	tx, err := m.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	key := OccurrenceKey(m.sc, *m.sc.NextAt, *m.sc.NextWall)
	if err = m.s.snapshotMilestoneEvidenceTx(tx, m.ws.ID, m.sc.ID, key); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}

	b := m.child(t, m.declared, "Also shipped")
	m.sel(t, b)
	m.setStatus(t, b, "shipped")
	r := m.resolveForReopenTest(t, m.ms, ResolveInput{OccurrenceAt: rt(*m.sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	if m.count(t, `SELECT COUNT(*) FROM milestone_completion_evidence WHERE schedule_id=?`, m.sc.ID) != 2 {
		t.Fatal("no evidence captured")
	}
	before := reopenState(t, m.s)
	_, err = m.s.reopenOccurrenceAt(m.ws.ID, m.ms.ID, ReopenInput{ResolutionID: r.ResolutionID, Actor: "user"}, rt("2026-10-30T10:01:00Z"))
	var refusal *ReopenError
	if !errors.As(err, &refusal) || refusal.Code != "reopen_dependency" {
		t.Fatalf("milestone: %v", err)
	}
	if !reflect.DeepEqual(before, reopenState(t, m.s)) {
		t.Fatal("milestone refusal changed evidence")
	}
	view, err := m.s.GetCommitmentFor(m.ws.ID, m.ms.ID, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), a.ID) || strings.Contains(string(raw), b.ID) {
		t.Fatal("reopen audit exposed a hidden milestone child")
	}
}

func TestReopenGraphExclusionAndNoops(t *testing.T) {
	for _, kind := range []string{"incoming", "outgoing", "blocked", "blocker", "milestone"} {
		t.Run(kind, func(t *testing.T) {
			f := newSchedFixture(t)
			item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			other, osc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
			if kind == "incoming" || kind == "outgoing" {
				parent, child := sc.ID, osc.ID
				if kind == "incoming" {
					parent, child = child, parent
				}
				f.exec(t, `INSERT INTO schedule_links(id,workspace_id,parent_schedule_id,parent_role,child_schedule_id,child_role,offset_mode,cancel_policy,created_at,updated_at) VALUES(?,?,?,'target',?,'target','elapsed','waiting_on','x','x')`, newID(), f.ws.ID, parent, child)
			} else if kind == "milestone" {
				f.exec(t, `INSERT INTO milestone_children(id,workspace_id,schedule_id,child_item_id,kind,added_at) VALUES(?,?,?,?,'child','x')`, newID(), f.ws.ID, sc.ID, other.ID)
			} else {
				blocked, blocker := item.ID, other.ID
				if kind == "blocker" {
					blocked, blocker = blocker, blocked
				}
				f.exec(t, `INSERT INTO schedule_gates(id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES(?,?,?,?,?)`, newID(), f.ws.ID, blocked, blocker, "x")
			}
			input := ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}
			if kind == "outgoing" {
				input.State = "move"
				until := rt("2026-10-30T15:00:00Z")
				input.Until = &until
				input.LinkAccess = models.CommitmentAccess{Readable: map[string]bool{item.ID: true, other.ID: true}, Editable: map[string]bool{item.ID: true, other.ID: true}}
			}
			r := f.resolveForReopenTest(t, item, input, "2026-10-30T10:00:00Z")
			if r.ScheduleLastResolution == nil || r.ScheduleLastResolution.ReopenCode != "reopen_dependency" {
				t.Fatalf("missing dependency availability: %+v", r.ScheduleLastResolution)
			}
			f.exec(t, `DELETE FROM schedule_links WHERE workspace_id=?`, f.ws.ID)
			f.exec(t, `DELETE FROM schedule_gates WHERE workspace_id=?`, f.ws.ID)
			f.exec(t, `UPDATE milestone_children SET removed_at='x' WHERE schedule_id=?`, sc.ID)
			before := reopenState(t, f.s)
			_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: r.ResolutionID}, rt("2026-10-30T10:01:00Z"))
			var refusal *ReopenError
			if !errors.As(err, &refusal) || refusal.Code != "reopen_dependency" {
				t.Fatalf("%s: %v", kind, err)
			}
			if !reflect.DeepEqual(before, reopenState(t, f.s)) {
				t.Fatal("dependency refusal wrote")
			}
		})
	}
	t.Run("no-op does not invalidate", func(t *testing.T) {
		f := newSchedFixture(t)
		item, sc := f.mk(t, dailyUTC, "2026-10-29T00:00:00Z")
		r := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
		if err := f.s.ClearForecast(f.ws.ID, item.ID, "", "user"); err != nil {
			t.Fatal(err)
		}
		if err := f.s.RemoveMilestoneChild(f.ws.ID, item.ID, f.item.ID, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: r.ResolutionID, Actor: "user"}, rt("2026-10-30T10:01:00Z")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestReopenDateDeadlineAndRecipient(t *testing.T) {
	for _, at := range []string{"2026-10-30T23:59:59Z", "2026-10-31T00:00:00Z"} {
		t.Run(at, func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			in.Dtstart = "2026-10-30"
			item, sc := f.mk(t, in, "2026-10-29T00:00:00Z")
			f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=0,deadline_behavior='lapses' WHERE id=?`, sc.ID)
			r := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T12:00:00Z")
			before := reopenState(t, f.s)
			_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: r.ResolutionID, Actor: "user"}, rt(at))
			if at == "2026-10-30T23:59:59Z" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var refusal *ReopenError
				if !errors.As(err, &refusal) || refusal.Code != "reopen_deadline_passed" || !reflect.DeepEqual(before, reopenState(t, f.s)) {
					t.Fatalf("midnight refusal: %v", err)
				}
			}
		})
	}
	t.Run("recipient zone changed", func(t *testing.T) {
		f := newSchedFixture(t)
		u := f.user(t, "reopen-zone@example.com", "UTC")
		in := dailyUTC
		in.Floating = true
		in.Tz = ""
		item, sc := f.mk(t, in, "2026-10-29T00:00:00Z")
		f.exec(t, `UPDATE items SET assigned_user_id=? WHERE id=?`, u.ID, item.ID)
		r := f.resolveForReopenTest(t, item, ResolveInput{OccurrenceAt: rt("2026-10-30T09:00:00Z"), State: "done", Actor: "user", View: time.UTC}, "2026-10-30T10:00:00Z")
		f.exec(t, `UPDATE users SET timezone='America/New_York' WHERE id=?`, u.ID)
		before := reopenState(t, f.s)
		_, err := f.s.reopenOccurrenceAt(f.ws.ID, item.ID, ReopenInput{ResolutionID: r.ResolutionID}, rt("2026-10-30T10:01:00Z"))
		var refusal *ReopenError
		if !errors.As(err, &refusal) || refusal.Code != "reopen_conflict" || !reflect.DeepEqual(before, reopenState(t, f.s)) {
			t.Fatalf("zone refusal: %v schedule=%s", err, sc.ID)
		}
	})
}

// Test callers obtain eligibility from a separate commitment read, as clients do.
type resolvedForReopenTest struct {
	*ResolveResult
	*models.ScheduleLastResolution
}

func (f *schedFixture) resolveForReopenTest(t *testing.T, item *models.Item, in ResolveInput, now string) *resolvedForReopenTest {
	t.Helper()
	res := f.mustResolve(t, item, in, now)
	view, err := f.s.GetCommitment(f.ws.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &resolvedForReopenTest{res, view.LastResolution}
}

// assertHistoryLapsedAgain proves a reopened late Done put the history row back
// exactly as the deadline evaluator left it.
func assertHistoryLapsedAgain(t *testing.T, f *schedFixture, scheduleID, at string) {
	t.Helper()
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=? AND state='lapsed' AND actor='system' AND comment_id IS NULL AND actual_done_at IS NULL AND actual_source IS NULL`, scheduleID, at); n != 1 {
		t.Fatalf("lapsed rows at %s = %d, want 1", at, n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='done'`, scheduleID); n != 0 {
		t.Fatalf("done rows = %d, want 0", n)
	}
}

func (f *schedFixture) lateDoneForReopenTest(t *testing.T, sc *models.ItemSchedule, in ResolveInput, now string) *resolvedForReopenTest {
	t.Helper()
	item, err := f.s.GetItem(sc.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	if in.State == "" {
		in.State, in.Actor = "done", "user"
	}
	return f.resolveForReopenTest(t, item, in, now)
}

func TestReopenHistoryDoneRecurring(t *testing.T) {
	f := newSchedFixture(t)
	u := f.user(t, "late-done@example.com", "")
	sc := lapseSchedule(t, f, true)
	f.eval(t, lapseNow, 0)
	lapsedRow := renegotiateRows(t, f.s, "schedule_occurrences", "")
	schedule := renegotiateRows(t, f.s, "item_schedules", "")
	reminders := renegotiateRows(t, f.s, "item_reminders", "")
	item, err := f.s.GetItem(sc.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	actual := rt("2026-10-26T09:30:00Z")
	res := f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt), State: "done", Actor: "user", ActorUserID: u.ID, ActualDoneAt: &actual, Comment: f.comment(item, u.ID, "Done late")}, "2026-10-26T13:00:00Z")
	if res.ScheduleLastResolution == nil || !res.ScheduleLastResolution.CanReopen || res.ScheduleLastResolution.State != "history_done" {
		t.Fatalf("availability %+v", res.ScheduleLastResolution)
	}
	if want := OccurrenceKey(sc, deadlineOccAt, "2026-10-26T09:00:00"); res.ScheduleLastResolution.OccurrenceKey != want {
		t.Fatalf("key %s want %s", res.ScheduleLastResolution.OccurrenceKey, want)
	}
	got, err := f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user", ActorUserID: u.ID}, rt("2026-10-26T13:01:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if got.PreviousState != "history_done" || got.MissedRemoved != 0 || got.CommentID == "" {
		t.Fatalf("result %+v", got)
	}
	assertHistoryLapsedAgain(t, f, sc.ID, deadlineOccAt)
	if after := renegotiateRows(t, f.s, "schedule_occurrences", ""); after != lapsedRow {
		t.Fatal("history row not restored byte for byte")
	}
	if renegotiateRows(t, f.s, "item_schedules", "") != schedule {
		t.Fatal("reopen touched the schedule row")
	}
	if renegotiateRows(t, f.s, "item_reminders", "") != reminders {
		t.Fatal("reopen touched the managed reminder")
	}
	if f.deadlineEvidence(t) != 1 {
		t.Fatal("miss evidence removed")
	}
	if f.count(t, `SELECT COUNT(*) FROM comments WHERE id=? AND deleted_at IS NULL`, got.CommentID) != 1 {
		t.Fatal("comment removed")
	}
	view, err := f.s.GetCommitment(f.ws.ID, sc.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	corrected := false
	for _, row := range view.Journal {
		if row.ChangeType == "done" && row.Role != nil && *row.Role == "deadline" {
			corrected = row.ReversedBy == got.ChangeID
		}
	}
	if !corrected {
		t.Fatal("on-time correction not kept and marked reversed")
	}
	if view.LastResolution == nil || view.LastResolution.CanReopen || view.LastResolution.ReopenCode != "reopen_conflict" {
		t.Fatalf("consumed checkpoint %+v", view.LastResolution)
	}
	// The restored row is lapsed again, so the late Done can be recorded anew.
	f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt)}, "2026-10-26T13:02:00Z")
}

func TestReopenHistoryDoneFloatingViewIdentity(t *testing.T) {
	for _, allDay := range []bool{false, true} {
		t.Run(strconv.FormatBool(allDay), func(t *testing.T) {
			f := newSchedFixture(t)
			u := f.user(t, "reopen-floating@example.com", "America/Los_Angeles")
			f.assign(t, f.item.ID, u.ID)
			start, storedAt, viewerAt, now := "2026-10-26T09:00", "2026-10-26T16:00:00Z", "2026-10-26T00:00:00Z", "2026-10-26T20:00:00Z"
			if allDay {
				start, storedAt, viewerAt, now = "2026-10-26", "2026-10-26T07:00:00Z", "2026-10-25T15:00:00Z", "2026-10-27T08:00:00Z"
			}
			sc, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "check", Dtstart: start, Floating: true, Rrule: "FREQ=DAILY"}, rt("2026-10-01T00:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			f.exec(t, `UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=0,deadline_behavior='lapses' WHERE id=?`, sc.ID)
			f.eval(t, now, 0)
			lapsed := renegotiateRows(t, f.s, "schedule_occurrences", "")
			view, err := time.LoadLocation("Asia/Tokyo")
			if err != nil {
				t.Fatal(err)
			}
			res := f.lateDoneForReopenTest(t, sc, ResolveInput{State: "done", Actor: "user", View: view, OccurrenceAt: rt(viewerAt)}, now)
			if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_at=? AND state='done'`, sc.ID, storedAt) != 1 {
				t.Fatal("late Done did not land on the stored history row")
			}
			if _, err := f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt(now).Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			assertHistoryLapsedAgain(t, f, sc.ID, storedAt)
			if renegotiateRows(t, f.s, "schedule_occurrences", "") != lapsed {
				t.Fatal("floating history row not restored")
			}
		})
	}
}

func TestReopenHistoryDoneInvalidation(t *testing.T) {
	for _, name := range []string{"other lapsed done", "current done", "reminder fire", "item seq", "item close", "closed before done", "gate added", "journal write", "later lapse"} {
		t.Run(name, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := lapseSchedule(t, f, true)
			f.eval(t, lapseNow, 0)
			if name == "other lapsed done" {
				f.eval(t, "2026-10-27T12:00:00Z", 0)
			}
			if name == "closed before done" {
				if _, err := f.s.UpdateItem(sc.ItemID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
					t.Fatal(err)
				}
			}
			doneAt := "2026-10-26T13:00:00Z"
			if name == "other lapsed done" {
				doneAt = "2026-10-27T12:30:00Z"
			}
			res := f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt)}, doneAt)
			reopenAt, code := rt("2026-10-27T13:30:00Z"), "reopen_conflict"
			switch name {
			case "other lapsed done":
				f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt("2026-10-27T09:00:00Z")}, "2026-10-27T13:00:00Z")
			case "current done":
				f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt("2026-10-27T09:00:00Z")}, "2026-10-26T14:00:00Z")
				reopenAt = rt("2026-10-26T14:01:00Z")
			case "reminder fire":
				if len(f.fire(t, "2026-10-27T09:01:00Z")) != 1 {
					t.Fatal("reminder did not fire")
				}
			case "item seq":
				if _, err := f.s.UpdateItem(sc.ItemID, models.ItemUpdate{Title: ptr("Changed title")}); err != nil {
					t.Fatal(err)
				}
			case "item close":
				if _, err := f.s.UpdateItem(sc.ItemID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
					t.Fatal(err)
				}
			case "closed before done":
				code = ""
			case "gate added":
				other := f.another(t, "blocker")
				f.exec(t, `INSERT INTO schedule_gates(id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES(?,?,?,?,?)`, newID(), f.ws.ID, sc.ItemID, other.ID, "2026-10-26T13:01:00Z")
				code = "reopen_dependency"
			case "journal write":
				if _, err := f.s.updateScheduleAt(f.ws.ID, sc.ItemID, models.ScheduleUpdate{LeadMinutes: i(15)}, rt("2026-10-26T13:01:00Z")); err != nil {
					t.Fatal(err)
				}
			case "later lapse":
				f.eval(t, "2026-10-27T12:00:00Z", 0)
			}
			before := reopenState(t, f.s)
			_, err := f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, reopenAt)
			if code == "" {
				if err != nil {
					t.Fatalf("closed item reopen: %v", err)
				}
				assertHistoryLapsedAgain(t, f, sc.ID, deadlineOccAt)
				return
			}
			var refusal *ReopenError
			if !errors.As(err, &refusal) || refusal.Code != code {
				t.Fatalf("error %v want %s", err, code)
			}
			if !reflect.DeepEqual(before, reopenState(t, f.s)) {
				t.Fatal("refusal wrote")
			}
		})
	}
}

func TestReopenHistoryDoneGraphBlocked(t *testing.T) {
	for _, kind := range []string{"gate", "milestone"} {
		t.Run(kind, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := lapseSchedule(t, f, true)
			f.eval(t, lapseNow, 0)
			other := f.another(t, "partner")
			if kind == "gate" {
				f.exec(t, `INSERT INTO schedule_gates(id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES(?,?,?,?,?)`, newID(), f.ws.ID, other.ID, sc.ItemID, "x")
			} else {
				f.exec(t, `INSERT INTO milestone_children(id,workspace_id,schedule_id,child_item_id,kind,added_at) VALUES(?,?,?,?,'child','x')`, newID(), f.ws.ID, sc.ID, other.ID)
			}
			res := f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt)}, "2026-10-26T13:00:00Z")
			if res.ScheduleLastResolution == nil || res.ScheduleLastResolution.ReopenCode != "reopen_dependency" {
				t.Fatalf("availability %+v", res.ScheduleLastResolution)
			}
			f.exec(t, `DELETE FROM schedule_gates WHERE workspace_id=?`, f.ws.ID)
			f.exec(t, `UPDATE milestone_children SET removed_at='x' WHERE schedule_id=?`, sc.ID)
			before := reopenState(t, f.s)
			_, err := f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt("2026-10-26T13:01:00Z"))
			var refusal *ReopenError
			if !errors.As(err, &refusal) || refusal.Code != "reopen_dependency" || !reflect.DeepEqual(before, reopenState(t, f.s)) {
				t.Fatalf("dependency refusal: %v", err)
			}
		})
	}
}

// The forward late Done does not consult error_code (it never touches the
// current occurrence), so neither does its inverse.
func TestReopenHistoryDoneIgnoresScheduleError(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	f.eval(t, lapseNow, 0)
	f.exec(t, `UPDATE item_schedules SET error_code='unsupported' WHERE id=?`, sc.ID)
	res := f.lateDoneForReopenTest(t, sc, ResolveInput{OccurrenceAt: rt(deadlineOccAt)}, "2026-10-26T13:00:00Z")
	if res.ScheduleLastResolution == nil || !res.ScheduleLastResolution.CanReopen {
		t.Fatalf("availability %+v", res.ScheduleLastResolution)
	}
	if _, err := f.s.reopenOccurrenceAt(f.ws.ID, sc.ItemID, ReopenInput{ResolutionID: res.ResolutionID, Actor: "user"}, rt("2026-10-26T13:01:00Z")); err != nil {
		t.Fatal(err)
	}
	assertHistoryLapsedAgain(t, f, sc.ID, deadlineOccAt)
	if got := f.reload(t, sc.ItemID); got.ErrorCode == nil || *got.ErrorCode != "unsupported" {
		t.Fatal("reopen rewrote the schedule error")
	}
}
