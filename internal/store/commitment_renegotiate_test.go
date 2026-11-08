package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Capture complete rows, including timestamps and IDs, to detect any write.
func renegotiateRows(t *testing.T, s *Store, table, where string, args ...any) string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT * FROM `+table+` `+where+` ORDER BY id`), args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := [][]any{}
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for j := range values {
			dest[j] = &values[j]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		out = append(out, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func renegotiateState(t *testing.T, s *Store) []string {
	t.Helper()
	out := []string{}
	for _, table := range []string{"item_schedules", "schedule_occurrences", "schedule_occurrence_overrides", "schedule_changes", "item_reminders", "event_outbox"} {
		out = append(out, renegotiateRows(t, s, table, ""))
	}
	return out
}
func seriesRequest() models.CommitmentRenegotiateInput {
	return models.CommitmentRenegotiateInput{Scope: "series", Reason: " client changed plans ", Preview: true}
}
func applyRenegotiation(t *testing.T, f *schedFixture, in models.CommitmentRenegotiateInput, now time.Time) *models.CommitmentRenegotiation {
	t.Helper()
	in.Preview = true
	before := renegotiateState(t, f.s)
	p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
		t.Fatal("preview wrote rows")
	}
	in.Preview = false
	in.Revision = p.Revision
	a, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, now)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, a) {
		t.Fatalf("preview/apply differ: %+v / %+v", p, a)
	}
	return a
}
func assertRenegotiationJournal(t *testing.T, f *schedFixture, id, typ string, beforeN int) {
	t.Helper()
	rows := f.journal(t, id)
	n := 0
	for _, r := range rows[beforeN:] {
		for _, raw := range []*string{r.BeforeJSON, r.AfterJSON} {
			if raw != nil && (strings.Contains(*raw, "roles_origin") || strings.Contains(*raw, "legacy_backfill")) {
				t.Fatalf("journal marker: %+v", r)
			}
		}
		if r.ChangeType != typ || str(r.Role) != "deadline" || r.Actor != "acting-user" || r.ReasonSource != "user" || r.Reason != "client changed plans" {
			continue
		}
		n++
	}
	if n != 1 {
		t.Fatalf("renegotiation journal rows %d: %+v", n, rows)
	}
	c, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range c.Journal {
		if r.ChangeType == typ && r.Actor == "acting-user" {
			found = true
		}
	}
	if !found {
		t.Fatal("renegotiation absent from GET journal")
	}
}
func TestRenegotiateSeriesCasesAndPastUntouched(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"offset", "lapses to stays_owed", "stays_owed to lapses", "removal", "one-off removal"} {
		t.Run(name, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.nativeCommit(t)
			if name == "one-off removal" {
				f.exec(t, `UPDATE item_schedules SET rrule=NULL WHERE id=?`, sc.ID)
			}
			if name == "lapses to stays_owed" {
				f.exec(t, `UPDATE item_schedules SET deadline_behavior='lapses' WHERE id=?`, sc.ID)
			}
			pastKey := "2026-10-25T09:00:00Z|2026-10-25T09:00:00"
			f.exec(t, `INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES (?,?,?,'deadline',?,?,?)`, newID(), sc.ID, pastKey, "2026-10-25T15:00:00Z", now(), now())
			f.exec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,'done','user',?)`, newID(), sc.ID, "2026-10-25T09:00:00Z", "2026-10-25T09:00:00", now())
			history := renegotiateRows(t, f.s, "schedule_occurrences", "")
			overrides := renegotiateRows(t, f.s, "schedule_occurrence_overrides", "WHERE occurrence_key<?", deadlineKey)
			prior := f.journal(t, sc.ID)
			priorIDs := make([]any, len(prior))
			for j, row := range prior {
				priorIDs[j] = row.ID
			}
			priorWhere := "WHERE id IN (" + placeholders(len(priorIDs)) + ")"
			priorBytes := renegotiateRows(t, f.s, "schedule_changes", priorWhere, priorIDs...)
			beforeN := len(prior)
			in := seriesRequest()
			switch name {
			case "offset":
				in.DeadlineOffset = &models.CommitmentDeadlineOffset{DeadlineDays: i(2), DeadlineSecs: i(-3600)}
			case "lapses to stays_owed":
				in.DeadlineBehavior = ptr("stays_owed")
			case "stays_owed to lapses":
				in.DeadlineBehavior = ptr("lapses")
			default:
				in.RemoveDeadline = true
			}
			reminder := renegotiateRows(t, f.s, "item_reminders", "")
			a := applyRenegotiation(t, f, in, rt("2026-10-26T09:30:00Z"))
			if a.Preview.AppliesFrom != deadlineKey || a.Preview.OccurrenceKey != deadlineKey {
				t.Fatalf("applies from %+v", a)
			}
			if history != renegotiateRows(t, f.s, "schedule_occurrences", "") || overrides != renegotiateRows(t, f.s, "schedule_occurrence_overrides", "WHERE occurrence_key<?", deadlineKey) {
				t.Fatal("past rows changed")
			}
			got := f.journal(t, sc.ID)
			if priorBytes != renegotiateRows(t, f.s, "schedule_changes", priorWhere, priorIDs...) {
				t.Fatal("prior journal bytes changed")
			}
			if reminder != renegotiateRows(t, f.s, "item_reminders", "") {
				t.Fatal("role change altered reminder timing")
			}
			assertRenegotiationJournal(t, f, sc.ID, "update", beforeN)
			row := got[len(got)-1]
			var b, after map[string]any
			if json.Unmarshal([]byte(str(row.BeforeJSON)), &b) != nil || json.Unmarshal([]byte(str(row.AfterJSON)), &after) != nil || len(b) != 3 || len(after) != 3 {
				t.Fatalf("snapshots %+v", row)
			}
			c, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if c.Roles.Target == nil || *c.Roles.Target != deadlineOccAt {
				t.Fatal("target changed")
			}
			if in.RemoveDeadline {
				if c.Roles.Deadline != nil || c.Roles.DeadlineBehavior != nil || after["deadline_days"] != nil || after["deadline_secs"] != nil || after["deadline_behavior"] != nil {
					t.Fatalf("deadline remains %+v %v", c, after)
				}
				if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE role='deadline' AND occurrence_key=?`, deadlineKey) != 0 {
					t.Fatal("deadline overrides remain")
				}
				if ok, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, false); err != nil || !ok {
					t.Fatalf("deadline-less delete %v %v", ok, err)
				}
			} else if name == "offset" {
				if str(c.Roles.Deadline) != "2026-10-28T08:00:00Z" {
					t.Fatalf("offset %+v", c)
				}
				f.force(t, sc, "2026-10-27T09:00:00", "2026-10-27T09:00:00Z", "2026-10-27T09:00:00Z")
				c, err = f.s.GetCommitment(f.ws.ID, f.item.ID)
				if err != nil || str(c.Roles.Deadline) != "2026-10-29T08:00:00Z" {
					t.Fatalf("later offset %+v %v", c, err)
				}
			} else if str(c.Roles.DeadlineBehavior) != *in.DeadlineBehavior || after["deadline_behavior"] != *in.DeadlineBehavior {
				t.Fatalf("behavior %+v %v", c, after)
			}
		})
	}
}
func TestRenegotiateSeriesKeepsExplicitOverrideAndRemovalClearsIt(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	in := models.CommitmentRenegotiateInput{Reason: " client changed plans ", Deadline: ptr("2026-10-27T14:00:00Z")}
	n := len(f.journal(t, sc.ID))
	applyRenegotiation(t, f, in, renegNow)
	assertRenegotiationJournal(t, f, sc.ID, "move", n)
	in = seriesRequest()
	in.DeadlineOffset = &models.CommitmentDeadlineOffset{DeadlineDays: i(3), DeadlineSecs: i(0)}
	p := applyRenegotiation(t, f, in, renegNow)
	if !p.Preview.KeepsDeadlineOverride || str(p.Preview.NewDeadline) != "2026-10-27T14:00:00Z" {
		t.Fatalf("override preview %+v", p)
	}
	in = seriesRequest()
	in.RemoveDeadline = true
	p = applyRenegotiation(t, f, in, renegNow)
	if !p.Preview.ClearsDeadlineOverrides || p.Preview.KeepsDeadlineOverride || p.Preview.NewDeadline != nil || f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE occurrence_key=?`, deadlineKey) != 0 {
		t.Fatal("removal kept override")
	}
}
func TestRenegotiateMissFirstAndTickDedupe(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"occurrence", "series removal", "series future offset", "series behavior"} {
		t.Run(scope, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.nativeCommit(t)
			in := seriesRequest()
			switch scope {
			case "occurrence":
				in.Scope = "occurrence"
				in.Deadline = ptr("2026-10-27T14:00:00Z")
			case "series removal":
				in.RemoveDeadline = true
			case "series future offset":
				in.DeadlineOffset = &models.CommitmentDeadlineOffset{DeadlineDays: i(2), DeadlineSecs: i(0)}
			default:
				in.DeadlineBehavior = ptr("lapses")
			}
			n := len(f.journal(t, sc.ID))
			a := applyRenegotiation(t, f, in, rt("2026-10-26T10:00:01Z"))
			if !a.Preview.MissRecordedFirst || str(a.Preview.MissOccurrenceKey) != deadlineKey || f.deadlineEvidence(t) != 1 {
				t.Fatalf("old miss lost %+v", a)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='missed_recorded' AND role='deadline' AND occurrence_key=?`, sc.ID, deadlineKey); n != 1 {
				t.Fatalf("after renegotiation: old deadline miss rows = %d, want 1", n)
			}
			if got := f.eval(t, "2026-10-26T10:00:02Z", 0); got.Missed != 0 || f.deadlineEvidence(t) != 1 {
				t.Fatalf("tick duplicate %+v", got)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='missed_recorded' AND role='deadline' AND occurrence_key=?`, sc.ID, deadlineKey); n != 1 {
				t.Fatalf("after evaluator tick: old deadline miss rows = %d, want 1", n)
			}
			rows := f.journal(t, sc.ID)
			if rows[n].ChangeType != "missed_recorded" || rows[n+1].ReasonSource != "user" {
				t.Fatal("miss was not first")
			}
			typ := "update"
			if scope == "occurrence" {
				typ = "move"
			}
			assertRenegotiationJournal(t, f, sc.ID, typ, n)
		})
	}
}
func TestRenegotiateSkipsTerminalMiss(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	f.nativeCommit(t)
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{FieldsPatch: map[string]any{"status": "done"}}); err != nil {
		t.Fatal(err)
	}
	in := seriesRequest()
	in.RemoveDeadline = true
	a := applyRenegotiation(t, f, in, rt("2026-10-26T11:00:00Z"))
	if a.Preview.MissRecordedFirst || f.deadlineEvidence(t) != 0 {
		t.Fatal("terminal item recorded a miss")
	}
}
func TestRenegotiateAtomicAfterMissAndChange(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"occurrence", "series"} {
		t.Run(scope, func(t *testing.T) {
			f := newSchedFixture(t)
			if f.s.dialect.Driver() != DriverSQLite {
				t.Skip("SQLite fault injection")
			}
			f.nativeCommit(t)
			in := seriesRequest()
			in.Scope = scope
			if scope == "series" {
				in.RemoveDeadline = true
			} else {
				in.Deadline = ptr("2026-10-27T14:00:00Z")
			}
			p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, rt("2026-10-26T11:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			in.Preview = false
			in.Revision = p.Revision
			f.exec(t, `CREATE TRIGGER renegotiate_fail BEFORE INSERT ON schedule_changes WHEN NEW.reason_source='user' BEGIN SELECT RAISE(ABORT,'renegotiation journal refused'); END`)
			before := renegotiateState(t, f.s)
			_, err = f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, rt("2026-10-26T11:00:00Z"))
			if err == nil {
				t.Fatal("injected error ignored")
			}
			if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
				t.Fatal("miss or change escaped rollback")
			}
		})
	}
}
func TestRenegotiateStaleRevisionAndRequired(t *testing.T) {
	t.Parallel()
	for _, change := range []string{"roles", "resolve", "override"} {
		t.Run(change, func(t *testing.T) {
			f := newSchedFixture(t)
			f.nativeCommit(t)
			in := seriesRequest()
			in.RemoveDeadline = true
			p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, renegNow)
			if err != nil {
				t.Fatal(err)
			}
			in.Preview = false
			_, err = f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, renegNow)
			var fe *ScheduleFieldError
			if !errors.As(err, &fe) || fe.Field != "revision" || fe.Code != "required" {
				t.Fatalf("missing revision %v", err)
			}
			switch change {
			case "roles":
				roles := nativeRoles()
				roles.Outcome = "different"
				_, err = f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, roles)
			case "resolve":
				_, err = f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, ResolveInput{State: "done", OccurrenceAt: rt(deadlineOccAt), Actor: "user", View: time.UTC}, renegNow)
			default:
				err = f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-28T14:00:00Z"), "explicit", renegNow)
			}
			if err != nil {
				t.Fatal(err)
			}
			in.Revision = p.Revision
			before := renegotiateState(t, f.s)
			_, err = f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "acting-user", in, renegNow)
			if !errors.Is(err, ErrCommitmentChanged) {
				t.Fatalf("stale revision %v", err)
			}
			if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
				t.Fatal("stale apply wrote")
			}
		})
	}
}
func TestRenegotiateSeriesConvertedRowLaterMiss(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.commitFixture(t, "check")
	if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.item.ID, nativeRoles()); err != nil {
		t.Fatal(err)
	}
	in := seriesRequest()
	in.DeadlineOffset = &models.CommitmentDeadlineOffset{DeadlineDays: i(40), DeadlineSecs: i(0)}
	n := len(f.journal(t, sc.ID))
	applyRenegotiation(t, f, in, renegNow)
	assertRenegotiationJournal(t, f, sc.ID, "update", n)
	c, err := f.s.GetCommitment(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.eval(t, rt(*c.Roles.Deadline).Add(time.Second).Format(time.RFC3339), 0); got.Missed != 1 {
		t.Fatalf("conversion marker suppressed later miss %+v", got)
	}
}

func TestRenegotiateRemovalPreservesResolvedMovedDeadlineOverride(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.nativeCommit(t)
	// Seed an earlier wall whose moved instant sorts after the next instance.
	f.force(t, sc, "2026-10-25T09:00:00", "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z")
	if err := f.s.renegotiateDeadlineAt(f.ws.ID, f.item.ID, "user", rt("2026-10-29T14:00:00Z"), "more time", renegNow); err != nil {
		t.Fatal(err)
	}
	moved := f.reload(t, f.item.ID)
	movedKey := OccurrenceKey(moved, *moved.NextAt, *moved.NextWall)
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*moved.NextAt), State: "done", Actor: "user", View: time.UTC}, "2026-10-26T08:30:00Z")
	f.force(t, sc, "2026-10-26T09:00:00", deadlineOccAt, deadlineOccAt)
	outstanding := f.reload(t, f.item.ID)
	key := OccurrenceKey(outstanding, *outstanding.NextAt, *outstanding.NextWall)
	if movedKey <= key {
		t.Fatalf("fixture must sort resolved moved key after outstanding: %s / %s", movedKey, key)
	}
	before := renegotiateRows(t, f.s, "schedule_occurrence_overrides", "WHERE occurrence_key=? AND role='deadline'", movedKey)
	if before == "[]" {
		t.Fatal("missing resolved deadline override")
	}
	in := seriesRequest()
	in.RemoveDeadline = true
	p := applyRenegotiation(t, f, in, renegNow)
	if before != renegotiateRows(t, f.s, "schedule_occurrence_overrides", "WHERE occurrence_key=? AND role='deadline'", movedKey) {
		t.Error("resolved moved deadline override changed")
	}
	if p.Preview.ClearsDeadlineOverrides {
		t.Error("preview reports clearing an absent outstanding override")
	}
}

func TestRenegotiateSeriesRefusesNewPastDeadline(t *testing.T) {
	t.Parallel()
	for _, secs := range []int{-1, 0} {
		t.Run((time.Duration(secs) * time.Second).String(), func(t *testing.T) {
			f := newSchedFixture(t)
			f.nativeCommit(t)
			in := seriesRequest()
			in.DeadlineOffset = &models.CommitmentDeadlineOffset{DeadlineDays: i(0), DeadlineSecs: i(secs)}
			before := renegotiateState(t, f.s)
			for _, preview := range []bool{true, false} {
				in.Preview = preview
				if !preview {
					// Obtain the same revision with an allowed plan; requests are not hashed.
					valid := seriesRequest()
					valid.DeadlineBehavior = ptr("lapses")
					p, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", valid, rt(deadlineOccAt))
					if err != nil {
						t.Fatal(err)
					}
					in.Revision = p.Revision
				}
				_, err := f.s.renegotiateCommitmentAt(f.ws.ID, f.item.ID, "user", in, rt(deadlineOccAt))
				var fe *ScheduleFieldError
				if !errors.As(err, &fe) || fe.Field != "deadline_offset" || fe.Code != "range" || fe.Message != "the new deadline is already past" {
					t.Errorf("preview=%v: got %v", preview, err)
				}
				if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
					t.Fatal("refused offset wrote rows")
				}
			}
		})
	}
}
