package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/schedule"
)

// Include all resolution evidence, not just the target and managed reminder.
func deadlineWriteSnapshot(t *testing.T, f *schedFixture) []any {
	t.Helper()
	out := []any{commitmentOldRows(t, f.s)}
	for _, table := range []string{"comments", "activities", "schedule_changes", "schedule_occurrence_overrides", "schedule_reopen_checkpoints"} {
		out = append(out, f.count(t, "SELECT COUNT(*) FROM "+table))
	}
	return out
}

func TestResolvePastDeadlineConfirmation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target, override string
		confirm, crossing      bool
	}{
		{"refuse", "2026-10-30T12:00:00Z", "", false, true},
		{"confirm", "2026-10-30T12:00:00Z", "", true, true},
		{"before", "2026-10-28T08:00:00Z", "", false, false},
		{"equal", "2026-10-28T09:00:00Z", "", false, false},
		{"later override", "2026-10-28T18:00:00Z", "2026-10-29T09:00:00Z", false, false},
		{"earlier override", "2026-10-28T08:00:00Z", "2026-10-27T18:00:00Z", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			sc := f.weeklyDeadline(t, "stays_owed")
			deadline := "2026-10-28T09:00:00Z"
			if tc.override != "" {
				f.deadlineOverride(t, sc, tc.override)
				deadline = tc.override
			}
			before := deadlineWriteSnapshot(t, f)
			in := moveInput(sc, tc.target)
			in.ConfirmPastDeadline = tc.confirm
			u := f.user(t, "deadline-move@example.com", "UTC")
			in.Comment = f.comment(f.item, u.ID, "move evidence")
			res, err := f.resolve(t, f.item, in, tueNoon)
			if tc.crossing && !tc.confirm {
				var past *ErrTargetAfterDeadline
				if !errors.As(err, &past) || !past.Deadline.Equal(rt(deadline)) {
					t.Fatalf("refusal=%v, deadline=%s", err, deadline)
				}
				if !reflect.DeepEqual(before, deadlineWriteSnapshot(t, f)) {
					t.Fatal("refusal persisted resolution evidence")
				}
			} else if err != nil || res.TargetAfterDeadline != tc.crossing {
				t.Fatalf("result=%+v err=%v", res, err)
			} else if got := f.reload(t, sc.ItemID); got.NextOverrideAt == nil || *got.NextOverrideAt != tc.target {
				t.Fatalf("target not saved: %+v", got.NextOverrideAt)
			}
		})
	}
}

func TestLinkedMovePastDeadlineConfirmation(t *testing.T) {
	t.Parallel()
	for _, linked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unlinked", true: "linked"}[linked], func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.exec(t, `UPDATE item_schedules SET deadline_days=2,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, f.scheds[0].ID)
			if linked {
				f.link(t, 0, 1, "target", "target", oneDay)
			}
			in := models.LinkedMoveInput{ItemID: f.items[0].ID, Role: "target", To: "2030-03-08T09:00:00Z", Access: f.allEdit(), Preview: true}
			before := deadlineWriteSnapshot(t, f.schedFixture)
			preview, err := f.s.MoveLinkedDate(f.ws.ID, in)
			if err != nil || preview.PastDeadline != "2030-03-06T09:00:00Z" {
				t.Fatalf("preview=%+v err=%v", preview, err)
			}
			in.Preview = false
			_, err = f.s.MoveLinkedDate(f.ws.ID, in)
			var field *ScheduleFieldError
			if !errors.As(err, &field) || field.Field != "revision" || field.Code != "required" {
				t.Fatalf("revision: %v", err)
			}
			in.Revision = preview.Revision
			_, err = f.s.MoveLinkedDate(f.ws.ID, in)
			var past *ErrTargetAfterDeadline
			if !errors.As(err, &past) || !past.Deadline.Equal(rt(preview.PastDeadline)) {
				t.Fatalf("confirmation: %v", err)
			}
			if !reflect.DeepEqual(before, deadlineWriteSnapshot(t, f.schedFixture)) {
				t.Fatal("refused move wrote")
			}
			in.ConfirmPastDeadline = true
			applied, err := f.s.MoveLinkedDate(f.ws.ID, in)
			if err != nil || applied.PastDeadline != preview.PastDeadline {
				t.Fatalf("apply=%+v err=%v", applied, err)
			}
			if got := f.role(t, 0, "target"); got != in.To {
				t.Fatalf("target=%s, want %s", got, in.To)
			}
			// Even keeping an already late target requires fresh confirmation.
			in.Preview, in.Revision, in.ConfirmPastDeadline = true, "", false
			preview, err = f.s.MoveLinkedDate(f.ws.ID, in)
			if err != nil {
				t.Fatal(err)
			}
			in.Preview, in.Revision = false, preview.Revision
			if _, err = f.s.MoveLinkedDate(f.ws.ID, in); !errors.As(err, &past) {
				t.Fatalf("repeat: %v", err)
			}
			in.To, in.Revision = "2030-03-06T09:00:00Z", ""
			if _, err = f.s.MoveLinkedDate(f.ws.ID, in); err != nil {
				t.Fatalf("cheap equal: %v", err)
			}
		})
	}
}

func TestPastDeadlineAndDependentBoundsAreIndependent(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	for _, sc := range f.scheds {
		f.exec(t, `UPDATE item_schedules SET deadline_days=2,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
	}
	f.link(t, 0, 1, "target", "target", oneDay)
	in := models.LinkedMoveInput{ItemID: f.items[0].ID, Role: "target", To: "2030-03-08T09:00:00Z", Access: f.allEdit(), Preview: true}
	preview, err := f.s.MoveLinkedDate(f.ws.ID, in)
	if err != nil || len(preview.Violations) != 1 || preview.PastDeadline == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	// Resolve must still direct this move to preview before asking for confirmation.
	ri := moveInput(f.scheds[0], in.To)
	ri.LinkAccess = f.allEdit()
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ri, rt("2030-03-04T08:00:00Z")); !errors.Is(err, ErrCommitmentPreviewRequired) {
		t.Fatalf("resolve precedence: %v", err)
	}
	in.Preview, in.Revision = false, preview.Revision
	if _, err := f.s.MoveLinkedDate(f.ws.ID, in); !errors.Is(err, ErrLinkViolationAck) {
		t.Fatalf("ack precedence: %v", err)
	}
	in.AcknowledgeViolations = true
	var past *ErrTargetAfterDeadline
	if _, err := f.s.MoveLinkedDate(f.ws.ID, in); !errors.As(err, &past) {
		t.Fatalf("confirmation: %v", err)
	}
	in.ConfirmPastDeadline = true
	if _, err := f.s.MoveLinkedDate(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
	// A dependent-only crossing requires only the existing acknowledgement.
	f.exec(t, `UPDATE item_schedules SET deadline_days=10 WHERE id=?`, f.scheds[0].ID)
	in.To, in.Preview, in.Revision, in.ConfirmPastDeadline = "2030-03-09T09:00:00Z", true, "", false
	preview, err = f.s.MoveLinkedDate(f.ws.ID, in)
	if err != nil || preview.PastDeadline != "" || len(preview.Violations) != 1 {
		t.Fatalf("dependent preview=%+v err=%v", preview, err)
	}
	in.Preview, in.Revision = false, preview.Revision
	if _, err := f.s.MoveLinkedDate(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
}

func TestRootPastDeadlineViolationOrientations(t *testing.T) {
	deadline, target := rt("2030-03-06T09:00:00Z"), rt("2030-03-08T09:00:00Z")
	for _, v := range []schedule.Violation{
		{Node: schedule.Node{Schedule: "root"}, Role: schedule.RoleTarget, Bound: schedule.RoleDeadline, Value: target, BoundValue: deadline},
		{Node: schedule.Node{Schedule: "root"}, Role: schedule.RoleDeadline, Bound: schedule.RoleTarget, Value: deadline, BoundValue: target},
	} {
		p := &linkedPlan{root: "root", impact: schedule.Impact{Violations: []schedule.Violation{v}}}
		if got, ok := p.rootPastDeadline(); !ok || !got.Equal(deadline) {
			t.Fatalf("deadline=%v ok=%v", got, ok)
		}
		p.root = "other"
		if _, ok := p.rootPastDeadline(); ok {
			t.Fatal("dependent treated as root")
		}
	}
}

func TestLinkedResolveOwnDeadlineConfirmation(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET deadline_days=2,deadline_secs=0,deadline_behavior='stays_owed' WHERE id=?`, f.scheds[0].ID)
	f.link(t, 0, 1, "target", "target", oneDay)
	in := moveInput(f.scheds[0], "2030-03-08T09:00:00Z")
	in.LinkAccess = f.allEdit()
	before := deadlineWriteSnapshot(t, f.schedFixture)
	var past *ErrTargetAfterDeadline
	if _, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, in, rt("2030-03-04T08:00:00Z")); !errors.As(err, &past) {
		t.Fatalf("refusal=%v", err)
	}
	if !reflect.DeepEqual(before, deadlineWriteSnapshot(t, f.schedFixture)) {
		t.Fatal("refusal wrote")
	}
	in.ConfirmPastDeadline = true
	if res, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, in, rt("2030-03-04T08:00:00Z")); err != nil || !res.TargetAfterDeadline {
		t.Fatalf("confirmed=%+v err=%v", res, err)
	}
}
