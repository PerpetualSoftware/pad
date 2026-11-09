package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The resolve-move refusal loosened, and the two skip rules (TASK-4353, ahead
// of 5b). A native row with a deadline can have its target moved; the deadline
// instant, evaluator and recorded miss do not follow it. A skip never erases a
// miss that has happened, and drops a promise still ahead only with authority.

const (
	weeklyMon = "2026-10-26T09:00:00Z" // the outstanding occurrence
	tueNoon   = "2026-10-27T10:00:00Z"
	wedNoon   = "2026-10-28T10:00:00Z"
	thuNoon   = "2026-10-29T10:00:00Z"
)

// weeklyDeadline is a native weekly row whose occurrence is Monday 09:00 UTC and
// whose deadline is two days later (Wednesday 09:00).
func (f *schedFixture) weeklyDeadline(t *testing.T, behavior string) *models.ItemSchedule {
	t.Helper()
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-26T09:00", Tz: "UTC", Rrule: "FREQ=WEEKLY"})
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior=?, next_at=?, next_wall=? WHERE id=?`,
		behavior, weeklyMon, "2026-10-26T09:00:00", sc.ID)
	return f.reload(t, sc.ItemID)
}

func (f *schedFixture) missEvidence(t *testing.T) []string {
	t.Helper()
	rows, err := f.s.db.Query(`SELECT COALESCE(occurrence_key,'')||' '||COALESCE(after_json,'') FROM schedule_changes WHERE change_type='missed_recorded' AND role='deadline' ORDER BY recorded_at,id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func skipInput(sc *models.ItemSchedule, may bool) ResolveInput {
	return ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "skip", Actor: "user", MaySkipBeforeDeadline: may}
}

func TestMoveOnNativeDeadlineRowLeavesTheDeadlineAlone(t *testing.T) {
	t.Parallel()
	for name, target := range map[string]string{"before the deadline": "2026-10-27T18:00:00Z", "after the deadline": "2026-10-30T12:00:00Z"} {
		t.Run(name, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.weeklyDeadline(t, "stays_owed")
			res := f.mustResolve(t, f.item, moveInput(sc, target), tueNoon)
			if want := strings.HasPrefix(name, "after"); res.TargetAfterDeadline != want {
				t.Fatalf("TargetAfterDeadline=%v, want %v", res.TargetAfterDeadline, want)
			}
			if got := f.reload(t, sc.ItemID); *got.NextAt != weeklyMon {
				t.Fatalf("next_at=%s moved", *got.NextAt)
			}
			if got := f.eval(t, "2026-10-28T08:59:59Z", 0); got.Missed != 0 {
				t.Fatalf("before the deadline: %+v", got)
			}
			if got := f.eval(t, "2026-10-28T09:00:01Z", 0); got.Missed != 1 {
				t.Fatalf("at the deadline: %+v", got)
			}
			ev := f.missEvidence(t)
			if len(ev) != 1 || !strings.Contains(ev[0], "2026-10-28T09:00:00Z") {
				t.Fatalf("evidence=%v, want one miss at the Wednesday deadline", ev)
			}
		})
	}
}

func TestMoveKeepsADeadlineOverride(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.weeklyDeadline(t, "stays_owed")
	f.deadlineOverride(t, sc, "2026-10-29T09:00:00Z")
	f.mustResolve(t, f.item, moveInput(sc, "2026-10-28T18:00:00Z"), tueNoon)
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE schedule_id=? AND role='deadline' AND value_at=?`, sc.ID, "2026-10-29T09:00:00Z"); n != 1 {
		t.Fatalf("deadline override rows=%d after the move, want 1", n)
	}
	if got := f.eval(t, "2026-10-28T09:00:01Z", 0); got.Missed != 0 {
		t.Fatalf("the override was lost, the default deadline applied: %+v", got)
	}
	if got := f.eval(t, "2026-10-29T09:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("at the overridden deadline: %+v", got)
	}
}

func TestMoveAfterARecordedMissKeepsTheMiss(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.weeklyDeadline(t, "stays_owed")
	if got := f.eval(t, "2026-10-28T09:30:00Z", 0); got.Missed != 1 {
		t.Fatalf("first: %+v", got)
	}
	before := f.missEvidence(t)
	f.mustResolve(t, f.item, moveInput(sc, "2026-10-31T12:00:00Z"), wedNoon)
	if got := f.eval(t, "2026-10-29T12:00:00Z", 0); got.Missed != 0 {
		t.Fatalf("re-recorded: %+v", got)
	}
	if !reflect.DeepEqual(before, f.missEvidence(t)) {
		t.Fatalf("evidence changed: %v -> %v", before, f.missEvidence(t))
	}
}

func TestMoveIntoThePastOnADeadlineRowIsRefused(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.weeklyDeadline(t, "stays_owed")
	before := commitmentOldRows(t, f.s)
	_, err := f.resolve(t, f.item, moveInput(sc, "2026-10-27T09:00:00Z"), tueNoon)
	var fe *ScheduleFieldError
	if !errors.As(err, &fe) || fe.Field != "until" || fe.Code != "range" {
		t.Fatalf("past move error=%v", err)
	}
	if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) {
		t.Fatal("refused move wrote changes")
	}
	if got := f.eval(t, "2026-10-28T09:00:01Z", 0); got.Missed != 1 {
		t.Fatalf("Wednesday deadline was dropped: %+v", got)
	}
}

func TestMoveOnALegacyHardDeadlineNamesTheConversion(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := moveSchedule(t, f, "deadline_hard", 0, "")
	_, err := f.resolve(t, f.item, moveInput(sc, "2026-10-31T10:00:00Z"), moveNow)
	var fe *ScheduleFieldError
	const want = "a legacy hard deadline cannot be moved; PATCH .../commitment naming a role converts the row to native roles, after which the target can be moved"
	if !errors.As(err, &fe) || fe.Field != "state" || fe.Code != "combination" || fe.Message != want {
		t.Fatalf("legacy move error=%v", err)
	}
	if strings.Contains(fe.Message, "renegotiate") {
		t.Fatal("legacy message points at renegotiate, which refuses legacy rows")
	}
}

func TestSkipAfterTheDeadlineRecordsTheMissFirst(t *testing.T) {
	t.Parallel()
	for _, behavior := range []string{"stays_owed", "lapses"} {
		t.Run(behavior, func(t *testing.T) {
			f := newSchedFixture(t)
			f.s.db.SetMaxOpenConns(1)
			sc := f.weeklyDeadline(t, behavior)
			// Thursday, before any tick: no authority is needed, the deadline passed.
			f.mustResolve(t, f.item, skipInput(sc, false), thuNoon)
			ev := f.missEvidence(t)
			if len(ev) != 1 || !strings.Contains(ev[0], "2026-10-28T09:00:00Z") {
				t.Fatalf("evidence=%v, want the Wednesday miss", ev)
			}
			if got := f.reload(t, sc.ItemID); *got.NextAt == weeklyMon {
				t.Fatal("skip did not advance")
			}
			if got := f.eval(t, "2026-10-29T10:01:00Z", 0); got.Missed != 0 || len(f.missEvidence(t)) != 1 {
				t.Fatalf("tick re-recorded: %+v", got)
			}
			var miss, skip = -1, -1
			for i, e := range moveEntries(t, f, sc.ID) {
				switch e.ChangeType {
				case "missed_recorded":
					miss = i
				case "skip":
					skip = i
				}
			}
			if miss < 0 || skip < 0 || miss > skip {
				t.Fatalf("journal order miss=%d skip=%d", miss, skip)
			}
		})
	}
}

func TestSkipAfterAnAlreadyRecordedMissAddsNoMissAndNeedsNoAuthority(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	sc := f.weeklyDeadline(t, "stays_owed")
	if got := f.eval(t, "2026-10-28T09:30:00Z", 0); got.Missed != 1 {
		t.Fatalf("tick: %+v", got)
	}
	f.mustResolve(t, f.item, skipInput(sc, false), thuNoon)
	if n := len(f.missEvidence(t)); n != 1 {
		t.Fatalf("evidence rows=%d, want the one miss to stay", n)
	}
}

func TestSkipUsesTheEvaluatorsBoundaryIncludingAnOverride(t *testing.T) {
	t.Parallel()
	t.Run("a later override keeps the deadline ahead", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "stays_owed")
		f.deadlineOverride(t, sc, "2026-10-29T09:00:00Z")
		before := commitmentOldRows(t, f.s)
		_, err := f.resolve(t, f.item, skipInput(sc, false), wedNoon) // the default Wednesday 09:00 has passed
		if !errors.Is(err, ErrSkipDeadlineAuthority) {
			t.Fatalf("err=%v, want ErrSkipDeadlineAuthority", err)
		}
		if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) || len(f.missEvidence(t)) != 0 {
			t.Fatal("refused skip wrote")
		}
	})
	t.Run("an earlier override has already passed", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "stays_owed")
		f.deadlineOverride(t, sc, "2026-10-27T09:30:00Z")
		f.mustResolve(t, f.item, skipInput(sc, false), tueNoon)
		ev := f.missEvidence(t)
		if len(ev) != 1 || !strings.Contains(ev[0], "2026-10-27T09:30:00Z") {
			t.Fatalf("evidence=%v, want the overridden deadline", ev)
		}
	})
}

func TestSkipBeforeTheDeadlineNeedsAuthority(t *testing.T) {
	t.Parallel()
	t.Run("refused without it, nothing written", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "stays_owed")
		before := commitmentOldRows(t, f.s)
		entries := len(moveEntries(t, f, sc.ID))
		_, err := f.resolve(t, f.item, skipInput(sc, false), tueNoon)
		if !errors.Is(err, ErrSkipDeadlineAuthority) {
			t.Fatalf("err=%v", err)
		}
		if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) || len(moveEntries(t, f, sc.ID)) != entries {
			t.Fatal("refused skip wrote")
		}
	})
	t.Run("allowed with it and journalled under the actor", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "lapses")
		in := skipInput(sc, true)
		in.Actor = "agent"
		f.mustResolve(t, f.item, in, tueNoon)
		var found bool
		for _, e := range moveEntries(t, f, sc.ID) {
			if e.ChangeType == "skip" && e.Actor == "agent" {
				found = true
			}
		}
		if !found {
			t.Fatalf("no skip journal row under the actor: %+v", moveEntries(t, f, sc.ID))
		}
		if len(f.missEvidence(t)) != 0 {
			t.Fatal("a skip before the deadline recorded a miss")
		}
	})
	t.Run("a changed assignee is commitment_changed and writes nothing", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "stays_owed")
		before := commitmentOldRows(t, f.s)
		in := skipInput(sc, true)
		in.ExpectedAssignee = ptr("someone-else")
		_, err := f.resolve(t, f.item, in, tueNoon)
		if !errors.Is(err, ErrCommitmentChanged) {
			t.Fatalf("err=%v", err)
		}
		if !reflect.DeepEqual(before, commitmentOldRows(t, f.s)) {
			t.Fatal("refused skip wrote")
		}
	})
	t.Run("a stale occurrence is stale, not an authority error", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "stays_owed")
		in := skipInput(sc, false)
		in.OccurrenceAt = rt("2026-11-02T09:00:00Z")
		if _, err := f.resolve(t, f.item, in, tueNoon); isStale(err) == nil {
			t.Fatalf("err=%v, want stale", err)
		}
	})
	t.Run("done is not affected", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := f.weeklyDeadline(t, "stays_owed")
		in := ResolveInput{OccurrenceAt: rt(*sc.NextAt), State: "done", Actor: "user"}
		f.mustResolve(t, f.item, in, tueNoon)
	})
}

func TestSkipOfAClosedItemRecordsNoMissAndLegacyIsUnchanged(t *testing.T) {
	t.Parallel()
	t.Run("closed item", func(t *testing.T) {
		f := newSchedFixture(t)
		f.s.db.SetMaxOpenConns(1)
		sc := f.weeklyDeadline(t, "stays_owed")
		f.exec(t, `UPDATE items SET fields = ? WHERE id = ?`, `{"status":"done"}`, f.item.ID)
		f.mustResolve(t, f.item, skipInput(sc, false), thuNoon)
		if n := len(f.missEvidence(t)); n != 0 {
			t.Fatalf("closed item recorded %d misses", n)
		}
	})
	t.Run("legacy row", func(t *testing.T) {
		f := newSchedFixture(t)
		sc := moveSchedule(t, f, "deadline_hard", 0, "FREQ=DAILY")
		f.mustResolve(t, f.item, skipInput(sc, false), moveNow)
		if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='missed_recorded' AND role='deadline'`); n != 0 {
			t.Fatalf("legacy skip recorded %d deadline misses", n)
		}
	})
}
