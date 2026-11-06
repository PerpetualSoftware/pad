package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

type commitmentEntry struct {
	ID, Type, Key, Role, Before, After, Actor, Reason, Source, Root string
	Parent                                                          sql.NullString
}

func commitmentJournal(t *testing.T, s *Store, id string) []commitmentEntry {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT id,change_type,COALESCE(occurrence_key,''),COALESCE(role,''),COALESCE(before_json,''),COALESCE(after_json,''),actor,reason,reason_source,root_change_id,parent_change_id FROM schedule_changes WHERE schedule_id=? ORDER BY recorded_at,id`), id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []commitmentEntry
	for rows.Next() {
		var e commitmentEntry
		if err := rows.Scan(&e.ID, &e.Type, &e.Key, &e.Role, &e.Before, &e.After, &e.Actor, &e.Reason, &e.Source, &e.Root, &e.Parent); err != nil {
			t.Fatal(err)
		}
		if e.Source != "system" || e.Reason != "" || e.Root != e.ID || e.Parent.Valid {
			t.Fatalf("journal provenance: %+v", e)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func commitmentSnapshot(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "dtstart", "dtend", "roles", "next_at", "next_wall", "next_override_at"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("snapshot missing %s", key)
		}
	}
	return m
}

func commitmentActual(t *testing.T, in ResolveInput, at time.Time) ResolveInput {
	t.Helper()
	field := reflect.ValueOf(&in).Elem().FieldByName("ActualDoneAt")
	if !field.IsValid() {
		t.Fatal("ResolveInput.ActualDoneAt missing")
	}
	field.Set(reflect.ValueOf(&at))
	return in
}

func TestCommitmentJournal_CreateUpdateDeleteSameTransaction(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	entries := commitmentJournal(t, f.s, sc.ID)
	if len(entries) != 1 || entries[0].Type != "create" || entries[0].Before != "" {
		t.Fatalf("create journal: %+v", entries)
	}
	commitmentSnapshot(t, entries[0].After)
	for _, upd := range []models.ScheduleUpdate{{LeadMinutes: ptr(30)}, {Audience: ptr("human")}, {Kind: ptr("deadline_hard")}, {Dtstart: ptr("2026-10-01T09:00:00"), Rrule: ptr("")}, {Dtstart: ptr("2026-11-01T09:00:00"), Rrule: ptr("FREQ=DAILY")}} {
		f.update(t, upd)
	}
	entries = commitmentJournal(t, f.s, sc.ID)
	if len(entries) != 6 {
		t.Fatalf("updates journaled=%d", len(entries))
	}
	for _, e := range entries[1:] {
		if e.Type != "update" {
			t.Fatal(e.Type)
		}
		commitmentSnapshot(t, e.Before)
		commitmentSnapshot(t, e.After)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND deadline_days=0 AND deadline_secs=0 AND deadline_behavior='stays_owed'`, sc.ID); n != 1 {
		t.Fatal("update did not derive roles")
	}
	ok, err := f.s.DeleteSchedule(f.ws.ID, f.item.ID)
	if err != nil || !ok {
		t.Fatalf("delete: %v", err)
	}
	entries = commitmentJournal(t, f.s, sc.ID)
	if len(entries) != 7 || entries[6].Type != "delete" || entries[6].After != "" {
		t.Fatal("delete lost journal")
	}
	commitmentSnapshot(t, entries[6].Before)
}

func TestCommitmentJournal_WriteFailureRollsBackEachMutation(t *testing.T) {
	for _, op := range []string{"create", "update", "delete", "move", "snooze", "done", "skip"} {
		t.Run(op, func(t *testing.T) {
			f := newSchedFixture(t)
			if f.s.dialect.Driver() != DriverSQLite {
				t.Skip("SQLite trigger fault injection")
			}
			sc := f.create(t, dailyUTC)
			before := commitmentOldRows(t, f.s)
			n := len(commitmentJournal(t, f.s, sc.ID))
			f.exec(t, `CREATE TRIGGER commitment_refuse BEFORE INSERT ON schedule_changes BEGIN SELECT RAISE(ABORT,'journal refused'); END`)
			var err error
			switch op {
			case "create":
				item := f.another(t, "Other")
				_, err = f.s.createScheduleAt(f.ws.ID, item.ID, dailyUTC, schedNow)
			case "update":
				_, err = f.s.updateScheduleAt(f.ws.ID, f.item.ID, models.ScheduleUpdate{LeadMinutes: ptr(30)}, schedNow)
			case "delete":
				_, err = f.s.DeleteSchedule(f.ws.ID, f.item.ID)
			default:
				until := rt("2026-11-01T09:00:00Z")
				in := ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: op, Actor: "user"}
				if op == "move" || op == "snooze" {
					in.Until = &until
				}
				_, err = f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, in, rt("2026-10-30T10:00:00Z"))
			}
			if err == nil {
				t.Fatal("mutation succeeded without journal")
			}
			if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) {
				t.Fatal("write escaped rollback")
			}
			if len(commitmentJournal(t, f.s, sc.ID)) != n {
				t.Fatal("journal escaped rollback")
			}
		})
	}
}

func TestCommitmentJournal_CommitFailureLeavesNoJournal(t *testing.T) {
	f := newSchedFixture(t)
	if f.s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite deferred FK fault injection")
	}
	f.exec(t, `CREATE TABLE commitment_parent (id TEXT PRIMARY KEY)`)
	f.exec(t, `CREATE TABLE commitment_deferred (id TEXT REFERENCES commitment_parent(id) DEFERRABLE INITIALLY DEFERRED)`)
	f.exec(t, `CREATE TRIGGER commitment_commit_fail AFTER INSERT ON schedule_changes BEGIN INSERT INTO commitment_deferred VALUES ('missing'); END`)
	_, err := f.s.createScheduleAt(f.ws.ID, f.item.ID, dailyUTC, schedNow)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("expected deferred commit failure: %v", err)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes`) != 0 || f.count(t, `SELECT COUNT(*) FROM item_schedules`) != 0 {
		t.Fatal("failed commit retained rows")
	}
}

func TestResolve_RepeatTargetMovesThenDoneRetainsEveryMove(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	original := rt(*sc.NextAt)
	var previous any
	for i := 0; i < 3; i++ {
		until := rt("2026-11-01T09:00:00Z").Add(time.Duration(i) * 24 * time.Hour)
		f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: original, State: "move", Until: &until, Actor: "user"}, "2026-10-30T10:00:00Z")
		entries := commitmentJournal(t, f.s, sc.ID)
		e := entries[len(entries)-1]
		if e.Type != "move" || e.Role != "target" || e.Key != original.Format(time.RFC3339)+"|"+*sc.NextWall {
			t.Fatalf("move identity: %+v", e)
		}
		before, after := commitmentSnapshot(t, e.Before), commitmentSnapshot(t, e.After)
		if before["next_override_at"] != previous || after["next_override_at"] != NormalizeInstant(until) {
			t.Fatal("move snapshot did not retain prior target")
		}
		previous = NormalizeInstant(until)
	}
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: original, State: "done", Actor: "user"}, "2026-11-03T10:00:00Z")
	entries := commitmentJournal(t, f.s, sc.ID)
	moves, done := 0, 0
	for _, e := range entries {
		if e.Type == "move" {
			moves++
		}
		if e.Type == "done" {
			done++
			if commitmentSnapshot(t, e.Before)["next_override_at"] != previous {
				t.Fatal("done lost final move")
			}
		}
	}
	if moves != 3 || done != 1 {
		t.Fatalf("moves=%d done=%d", moves, done)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND role='target' AND value_at=?`, sc.ID, previous) != 1 {
		t.Fatal("override missing after done")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='missed_recorded'`, sc.ID); n != 4 {
		t.Fatalf("missed journal count=%d", n)
	}
}

func TestResolve_LateReportedDoneAdvancesFromActual(t *testing.T) {
	for _, tc := range []struct {
		name, rule, actual, want string
		floating                 bool
	}{{"interval", "FREQ=DAILY", "2026-10-30T12:00:00Z", "2026-10-31T09:00:00Z", false}, {"elapsed", "FREQ=HOURLY", "2026-10-30T12:13:00Z", "2026-10-30T13:13:00Z", false}, {"floating", "FREQ=DAILY", "2026-10-30T12:00:00Z", "2026-10-31T09:00:00Z", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			in.Anchor, in.Rrule, in.Floating = "completion", tc.rule, tc.floating
			if in.Floating {
				in.Tz = ""
			}
			sc := f.create(t, in)
			at, ok, err := OutstandingInstant(sc, time.UTC)
			if err != nil || !ok {
				t.Fatal(err)
			}
			input := commitmentActual(t, ResolveInput{OccurrenceAt: at, State: "done", Actor: "user"}, rt(tc.actual))
			res, err := f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, input, rt("2026-11-03T12:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			next, ok, err := OutstandingInstant(res.Schedule, time.UTC)
			if err != nil || !ok || NormalizeInstant(next) != tc.want {
				t.Fatalf("next=%v ok=%v err=%v want=%s", next, ok, err, tc.want)
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='done' AND actual_done_at=? AND actual_source='reported' AND resolved_at='2026-11-03T12:00:00Z'`, sc.ID, tc.actual) != 1 {
				t.Fatal("actual provenance")
			}
		})
	}
}

func TestResolve_DoneRecordsRecordedNowAndRejectsFutureActual(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	nowT := rt("2026-10-30T10:00:00Z")
	input := ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}
	_, err := f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, commitmentActual(t, input, nowT.Add(time.Hour)), nowT)
	var field *ScheduleFieldError
	if !errors.As(err, &field) || field.Field != "actual_done_at" {
		t.Fatalf("future actual: %v", err)
	}
	if len(commitmentJournal(t, f.s, sc.ID)) != 1 {
		t.Fatal("future actual wrote journal")
	}
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, input, nowT); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND actual_done_at=? AND actual_source='recorded_now'`, sc.ID, NormalizeInstant(nowT)) != 1 {
		t.Fatal("recorded_now missing")
	}
	sc, err = f.s.GetSchedule(f.ws.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user"}, "2026-10-31T10:00:00Z")
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='skipped' AND actual_done_at IS NULL AND actual_source IS NULL`, sc.ID) != 1 {
		t.Fatal("skip has actuals")
	}
}

func TestSnoozeIsJournaledButWritesNoOverride(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	until := rt("2026-11-01T09:00:00Z")
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "snooze", Until: &until, Actor: "user"}, "2026-10-30T10:00:00Z")
	entries := commitmentJournal(t, f.s, sc.ID)
	if len(entries) != 2 || entries[1].Type != "snooze" {
		t.Fatal("missing snooze")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=?`, sc.ID) != 0 {
		t.Fatal("snooze wrote role override")
	}
	if commitmentSnapshot(t, entries[1].After)["next_override_at"] != NormalizeInstant(until) {
		t.Fatal("snooze semantics changed")
	}
}

func TestFireAndReanchorDoNotJournal(t *testing.T) {
	for _, floating := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned", true: "floating"}[floating], func(t *testing.T) {
			f := newSchedFixture(t)
			in := dailyUTC
			in.Floating = floating
			if floating {
				in.Tz = ""
			}
			sc := f.create(t, in)
			f.force(t, sc, "2026-10-31T09:00:00", "2026-10-31T09:00:00Z", "2026-10-31T09:00:00Z")
			entries := commitmentJournal(t, f.s, sc.ID)
			f.exec(t, `UPDATE item_schedules SET target_days=-2,target_secs=300,earliest_days=-3,earliest_secs=30,deadline_days=1,deadline_secs=900,deadline_behavior='stays_owed',occupied_days=0,occupied_secs=1800,outcome='attend',roles_origin='native' WHERE id=?`, sc.ID)
			roles := func() []any {
				values := make([]any, 11)
				pointers := make([]any, len(values))
				for i := range values {
					pointers[i] = &values[i]
				}
				if err := f.s.db.QueryRow(f.s.q(`SELECT target_days,target_secs,earliest_days,earliest_secs,deadline_days,deadline_secs,deadline_behavior,occupied_days,occupied_secs,outcome,roles_origin FROM item_schedules WHERE id=?`), sc.ID).Scan(pointers...); err != nil {
					t.Fatal(err)
				}
				return values
			}
			before := roles()
			if _, err := f.s.FireDueReminders("2026-11-02T10:00:00Z", 0); err != nil {
				t.Fatal(err)
			}
			after := commitmentJournal(t, f.s, sc.ID)
			if !reflect.DeepEqual(entries, after) {
				t.Fatal("fire journaled")
			}
			if !reflect.DeepEqual(before, roles()) {
				t.Fatal("roles changed")
			}
			cur, err := f.s.GetSchedule(f.ws.ID, f.item.ID)
			if err != nil || cur.AnchorWall == nil {
				t.Fatalf("reanchor not exercised: %v", err)
			}
		})
	}
}

func TestCarryAndImport_BackfillRolesAndJournal(t *testing.T) {
	for _, move := range []bool{false, true} {
		t.Run(map[bool]string{false: "copy", true: "move"}[move], func(t *testing.T) {
			c := newSchedCopy(t)
			in := daily()
			in.Kind = "deadline_hard"
			source := c.scheduleFuture(t, in)
			result := c.do(t, move)
			dst := c.sched(t, c.wsB.ID, result.Item.ID)
			entries := commitmentJournal(t, c.s, dst.ID)
			if len(entries) != 1 || entries[0].Type != "carry" {
				t.Fatal("carry journal")
			}
			if c.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND deadline_days=0 AND deadline_behavior='stays_owed'`, dst.ID) != 1 {
				t.Fatal("carry roles")
			}
			if move {
				es := commitmentJournal(t, c.s, source.ID)
				if len(es) != 2 || es[1].Type != "end" {
					t.Fatal("source end journal")
				}
			}
		})
	}
	f := newSchedFixture(t)
	in := dailyUTC
	in.Kind, in.Dtend = "window", "2026-10-30T11:00"
	sc := f.create(t, in)
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	bundle := f.exportBundle(t)
	if bundle.Version != 1 {
		t.Fatal("bundle version changed")
	}
	ws, _ := f.importBundle(t, bundle, "Imported dates")
	item := f.itemByTitle(t, ws.ID, f.item.Title)
	dst, err := f.s.GetSchedule(ws.ID, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	entries := commitmentJournal(t, f.s, dst.ID)
	if len(entries) != 1 || entries[0].Type != "import" {
		t.Fatal("import journal")
	}
	if f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND occupied_days=0 AND occupied_secs=7200 AND roles_origin='legacy_backfill'`, dst.ID) != 1 {
		t.Fatal("import roles")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND actual_source='legacy_unknown' AND actual_done_at IS NULL`, dst.ID) != 1 {
		t.Fatal("import fabricated actuals")
	}
}

func TestScheduleChanges_AppendOnlyBySource(t *testing.T) {
	pattern := regexp.MustCompile(`(?is)\b(?:UPDATE\s+|DELETE\s+FROM\s+)schedule_changes\b`)
	err := filepath.WalkDir("../..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == ".cache" || entry.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if pattern.Match(b) {
			t.Errorf("journal mutation in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommitmentJournal_MissedOnlyWhenInserted(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	w, _ := schedule.ParseWall("2026-10-31T09:00:00")
	occ := schedule.Occurrence{At: rt("2026-10-31T09:00:00Z"), Wall: w}
	for i, want := range []int{1, 0} {
		n, err := f.s.insertMissedTx(tx, sc.ID, occ, "2026-11-03T00:00:00Z")
		if err != nil || n != want {
			t.Fatalf("insert %d n=%d: %v", i, n, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	entries := commitmentJournal(t, f.s, sc.ID)
	if len(entries) != 2 || entries[1].Type != "missed_recorded" || entries[1].Actor != "system" {
		t.Fatal("duplicate missed journal")
	}
}

func TestOccurrenceKey_PinnedAllDayAndFallBack(t *testing.T) {
	sc := &models.ItemSchedule{}
	for _, tc := range []struct{ at, wall, want string }{
		{"2026-11-01T05:30:00Z", "2026-11-01T01:30:00", "2026-11-01T05:30:00Z|2026-11-01T01:30:00"},
		{"2026-11-01T06:30:00Z", "2026-11-01T01:30:00", "2026-11-01T06:30:00Z|2026-11-01T01:30:00"},
		{"2026-11-01T04:00:00Z", "2026-11-01", "2026-11-01"},
	} {
		if got := OccurrenceKey(sc, tc.at, tc.wall); got != tc.want {
			t.Fatalf("key=%s want=%s", got, tc.want)
		}
	}
}

func TestOccurrenceKey_FloatingStableBeforeAndAfterFire(t *testing.T) {
	f := newSchedFixture(t)
	in := dailyUTC
	in.Floating, in.Tz = true, ""
	sc := f.create(t, in)
	before := OccurrenceKey(sc, *sc.NextAt, *sc.NextWall)
	if _, err := f.s.FireDueReminders("2026-10-30T10:00:00Z", 0); err != nil {
		t.Fatal(err)
	}
	cur := f.reload(t, f.item.ID)
	if *cur.NextAt == *sc.NextAt || !cur.ReminderFired {
		t.Fatal("floating fire did not refine instant")
	}
	if after := OccurrenceKey(cur, *cur.NextAt, *cur.NextWall); after != before || after != *sc.NextWall {
		t.Fatalf("floating key changed %s to %s", before, after)
	}
}

func TestResolve_AllDayDoneActualMatchesImmutableWall(t *testing.T) {
	f := newSchedFixture(t)
	in := dailyUTC
	in.Dtstart = "2026-10-30"
	sc := f.create(t, in)
	f.exec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,actor,resolved_at) VALUES (?,?,?,?,'moved','user',?)`, newID(), sc.ID, "2026-10-30T08:00:00Z", *sc.NextWall, "2026-10-29T12:00:00Z")
	f.mustResolve(t, f.item, ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}, "2026-10-30T10:00:00Z")
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND occurrence_wall=? AND actual_done_at='2026-10-30T10:00:00Z' AND actual_source='recorded_now'`, sc.ID, *sc.NextWall) != 1 {
		t.Fatal("all-day completion lost actual provenance")
	}
}

func TestResolve_ActualWithinTheCurrentSecondIsNotFuture(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	nowT := rt("2026-10-30T10:00:00Z").Add(600 * time.Millisecond)
	input := ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}
	reported := rt("2026-10-30T10:00:00Z").Add(400 * time.Millisecond)
	input.ActualDoneAt = &reported
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.item.ID, input, nowT); err != nil {
		t.Fatalf("an actual inside the current second was refused: %v", err)
	}
}

func TestUpdateScheduleNoOpWritesNoJournalRow(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	before := len(commitmentJournal(t, f.s, sc.ID))
	same := sc.Audience
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.item.ID, models.ScheduleUpdate{Audience: &same}); err != nil {
		t.Fatal(err)
	}
	if got := len(commitmentJournal(t, f.s, sc.ID)); got != before {
		t.Fatalf("a no-op update journaled: %d rows, want %d", got, before)
	}
	other := "human"
	if sc.Audience == "human" {
		other = "agent"
	}
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.item.ID, models.ScheduleUpdate{Audience: &other}); err != nil {
		t.Fatal(err)
	}
	if got := len(commitmentJournal(t, f.s, sc.ID)); got != before+1 {
		t.Fatalf("a real update did not journal: %d rows, want %d", got, before+1)
	}
}

func TestScheduleSnapshotCoversAnchorEndAndError(t *testing.T) {
	f := newSchedFixture(t)
	sc := f.create(t, dailyUTC)
	snap := func() string {
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		out, err := f.s.scheduleSnapshotTx(tx, sc.ID)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := snap()
	for _, set := range []string{`anchor_wall='2026-10-29T09:00:00'`, `error_code='cap_scan'`, `ended_at='2026-10-30T00:00:00Z', next_at=NULL`} {
		f.exec(t, `UPDATE item_schedules SET `+set+` WHERE id=?`, sc.ID)
		if snap() == base {
			t.Fatalf("snapshot ignores %s", set)
		}
		f.exec(t, `UPDATE item_schedules SET anchor_wall=NULL, error_code=NULL, ended_at=NULL WHERE id=?`, sc.ID)
		base = snap()
	}
}
