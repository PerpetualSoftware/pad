package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The deadline of the outstanding occurrence (deadlineOccAt plus one hour) is
// still ahead at this instant and has passed at lapseNow.
const lapseBeforeDeadline = "2026-10-26T09:30:00Z"

func lapseRenegotiateInput(scope string) models.CommitmentRenegotiateInput {
	in := models.CommitmentRenegotiateInput{Scope: scope, Reason: "Plans changed", Preview: true}
	if scope == "occurrence" {
		in.Deadline = ptr("2026-10-28T10:00:00Z")
	} else {
		in.DeadlineBehavior = ptr("stays_owed")
	}
	return in
}

// recordOutstandingMiss records the miss of the outstanding occurrence the way
// the evaluator does, leaving the occurrence itself outstanding.
func recordOutstandingMiss(t *testing.T, f *schedFixture, sc *models.ItemSchedule) {
	t.Helper()
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := f.s.pinScheduleTx(tx, f.ws.ID, sc.ItemID); err != nil {
		t.Fatal(err)
	}
	d, _, err := f.s.deadlineStateTx(tx, sc.ID)
	if err != nil || d == nil {
		t.Fatalf("deadline state: %+v %v", d, err)
	}
	if err := f.s.recordDeadlineMissTx(tx, *d); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestRenegotiatePreviewLapsesOnApply(t *testing.T) {
	for _, tc := range []struct {
		name, behavior, now  string
		closed, recordedMiss bool
		wantLapses, wantMiss bool
	}{
		{name: "lapses due", behavior: "lapses", now: lapseNow, wantLapses: true, wantMiss: true},
		{name: "stays owed due", behavior: "stays_owed", now: lapseNow, wantMiss: true},
		{name: "lapses ahead", behavior: "lapses", now: lapseBeforeDeadline},
		{name: "lapses due closed item", behavior: "lapses", now: lapseNow, closed: true},
		{name: "lapses due miss recorded", behavior: "lapses", now: lapseNow, recordedMiss: true, wantLapses: true},
	} {
		for _, scope := range []string{"occurrence", "series"} {
			t.Run(tc.name+"/"+scope, func(t *testing.T) {
				f := newSchedFixture(t)
				sc := lapseSchedule(t, f, true)
				f.exec(t, `UPDATE item_schedules SET deadline_behavior=? WHERE id=?`, tc.behavior, sc.ID)
				if tc.closed {
					f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, sc.ItemID)
				}
				if tc.recordedMiss {
					recordOutstandingMiss(t, f, sc)
				}
				in := lapseRenegotiateInput(scope)
				p, err := f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(tc.now), nil)
				if err != nil {
					t.Fatalf("preview: %v", err)
				}
				if p.Preview.LapsesOnApply != tc.wantLapses || p.Preview.MissRecordedFirst != tc.wantMiss {
					t.Fatalf("lapses_on_apply=%v miss_recorded_first=%v, want %v %v", p.Preview.LapsesOnApply, p.Preview.MissRecordedFirst, tc.wantLapses, tc.wantMiss)
				}
				in.Preview, in.Revision = false, p.Revision
				_, err = f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(tc.now), nil)
				lapsed := f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND state='lapsed'`, sc.ID)
				if tc.wantLapses {
					if !errors.Is(err, ErrCommitmentChanged) {
						t.Fatalf("apply = %v, want commitment changed", err)
					}
					assertLapsed(t, f, sc)
					return
				}
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				if lapsed != 0 {
					t.Fatalf("lapsed rows = %d, want none", lapsed)
				}
			})
		}
	}
}

// The lapse decision is part of the revision: with the miss already recorded,
// only the passing of the deadline separates these two previews.
func TestRenegotiateRevisionBindsLapseDecision(t *testing.T) {
	f := newSchedFixture(t)
	sc := lapseSchedule(t, f, true)
	recordOutstandingMiss(t, f, sc)
	in := lapseRenegotiateInput("occurrence")
	ahead, err := f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(lapseBeforeDeadline), nil)
	if err != nil {
		t.Fatal(err)
	}
	due, err := f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(lapseNow), nil)
	if err != nil {
		t.Fatal(err)
	}
	if ahead.Preview.MissRecordedFirst || due.Preview.MissRecordedFirst || ahead.Preview.LapsesOnApply || !due.Preview.LapsesOnApply {
		t.Fatalf("decisions ahead=%+v due=%+v", ahead.Preview, due.Preview)
	}
	if ahead.Revision == due.Revision {
		t.Fatal("revision ignores the lapse decision")
	}
	in.Preview, in.Revision = false, ahead.Revision
	if _, err := f.s.renegotiateCommitmentAt(f.ws.ID, sc.ItemID, "user", in, rt(lapseNow), nil); !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("stale apply = %v, want commitment changed", err)
	}
}
