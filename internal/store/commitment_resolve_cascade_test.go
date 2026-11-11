package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (f *linkFix) resolveMove(t *testing.T, i int, to string, access models.CommitmentAccess, comment *ResolveComment) (*ResolveResult, error) {
	t.Helper()
	sc := f.reload(t, f.items[i].ID)
	return f.s.resolveOccurrenceAt(f.ws.ID, f.items[i].ID, ResolveInput{State: "move", OccurrenceAt: rt(*sc.NextAt), Until: ptr(rt(to)), Actor: "user", LinkAccess: access, Comment: comment}, rt("2030-03-03T12:00:00Z"))
}

func TestLinkedResolveMoveCascadesWithOneLineage(t *testing.T) {
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	before := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`)
	res, err := f.resolveMove(t, 0, "2030-03-10T09:00:00Z", models.CommitmentAccess{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.role(t, 1, "target") != "2030-03-11T09:00:00Z" || f.role(t, 2, "target") != "2030-03-12T09:00:00Z" {
		t.Fatal("resolve move left linked children unchanged")
	}
	if res.Schedule.NextOverrideAt == nil || *res.Schedule.NextOverrideAt != "2030-03-10T09:00:00Z" {
		t.Fatalf("returned schedule: %+v", res.Schedule)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`)-before != 3 || f.count(t, `SELECT COUNT(DISTINCT root_change_id) FROM schedule_changes WHERE change_type='move'`) != 1 || f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE parent_change_id IS NOT NULL`) != 2 {
		t.Fatal("cascade did not share one journal lineage")
	}
}

func TestLinkedResolveChildKeepsNewOffset(t *testing.T) {
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.resolveMove(t, 1, "2030-03-07T09:00:00Z", models.CommitmentAccess{}, nil); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_elapsed=259200`) != 1 {
		t.Fatal("resolve move did not keep the child's new offset")
	}
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if f.role(t, 1, "target") != "2030-03-13T09:00:00Z" {
		t.Fatal("parent cascade overwrote child's offset")
	}
}

func TestLinkedResolveHardDeadlineRefusalIsAtomic(t *testing.T) {
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET deadline_days=0,deadline_secs=3600,deadline_behavior='stays_owed' WHERE id=?`, f.scheds[1].ID)
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	u := f.user(t, "linked-refusal@example.com", "UTC")
	before := renegotiateState(t, f.s)
	comment := f.comment(f.items[0], u.ID, "Plans changed")
	_, err := f.resolveMove(t, 0, "2030-03-10T09:00:00Z", models.CommitmentAccess{}, comment)
	if !errors.Is(err, ErrDeadlineApprovalRequired) {
		t.Fatalf("nil access = %v, want deadline approval required", err)
	}
	_, err = f.resolveMove(t, 0, "2030-03-10T09:00:00Z", f.allEdit(), comment)
	if !errors.Is(err, ErrCommitmentPreviewRequired) {
		t.Fatalf("editable access = %v, want commitment preview required", err)
	}
	if !reflect.DeepEqual(before, renegotiateState(t, f.s)) || f.count(t, `SELECT COUNT(*) FROM comments`) != 0 || f.count(t, `SELECT COUNT(*) FROM activities WHERE action='commented'`) != 0 {
		t.Fatal("refused move wrote a date or comment")
	}
}

func TestLinkedResolveCommentStaysOnOccurrence(t *testing.T) {
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	u := f.user(t, "linked-comment@example.com", "UTC")
	res, err := f.resolveMove(t, 0, "2030-03-10T09:00:00Z", models.CommitmentAccess{}, f.comment(f.items[0], u.ID, "  Changed plans  "))
	if err != nil {
		t.Fatal(err)
	}
	if f.role(t, 1, "target") != "2030-03-11T09:00:00Z" {
		t.Fatal("commented move did not cascade")
	}
	if res.Comment == nil || f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE schedule_id=? AND comment_id=?`, f.scheds[0].ID, res.Comment.ID) != 1 {
		t.Fatal("move lost occurrence comment")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND reason='Changed plans' AND reason_source='user'`, f.scheds[0].ID) != 1 {
		t.Fatal("linked move did not use trimmed comment reason")
	}
}

func TestLinkedResolvePastMoveWaitsForDone(t *testing.T) {
	for _, root := range []bool{true, false} {
		t.Run(map[bool]string{true: "root", false: "child"}[root], func(t *testing.T) {
			f := newLinkFix(t, 2)
			for _, sc := range f.scheds {
				f.exec(t, `UPDATE item_schedules SET rrule='FREQ=DAILY' WHERE id=?`, sc.ID)
			}
			f.linkRaw(t, 0, 1, "target", "target", models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 3600})
			f.s.SetLinkClockForTesting(func() time.Time { return rt("2030-03-06T12:00:00Z") })
			i := 0
			if !root {
				i = 1
			}
			sc := f.reload(t, f.items[i].ID)
			original := *sc.NextAt
			res, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[i].ID, ResolveInput{State: "move", OccurrenceAt: rt(original), Until: ptr(rt("2030-03-05T07:00:00Z")), Actor: "user"}, rt("2030-03-06T12:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if res.Schedule.NextAt == nil || *res.Schedule.NextAt != original || res.MissedRecorded != 0 || f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NULL`, sc.ID) != 0 {
				t.Fatal("linked past move advanced or armed the row")
			}
			fired, err := f.s.FireDueReminders("2030-03-06T13:00:00Z", 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, reminder := range fired {
				if root || reminder.ItemID == f.items[i].ID {
					t.Fatal("past-move tick fired a moved row")
				}
			}
			res, err = f.s.resolveOccurrenceAt(f.ws.ID, f.items[i].ID, ResolveInput{State: "done", OccurrenceAt: rt(original), Actor: "user"}, rt("2030-03-06T14:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if res.MissedRecorded == 0 || res.Schedule.NextAt == nil || *res.Schedule.NextAt != "2030-03-07T09:00:00Z" || f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NULL`, sc.ID) != 1 {
				t.Fatalf("done did not record passed instances and arm once: %+v", res)
			}
		})
	}
}

func TestLinkedResolveInactiveOtherEndUsesPlainPastMove(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminal", true: "deleted"}[deleted], func(t *testing.T) {
			f := newLinkFix(t, 2)
			f.exec(t, `UPDATE item_schedules SET rrule='FREQ=DAILY' WHERE id=?`, f.scheds[0].ID)
			f.linkRaw(t, 0, 1, "target", "target", oneDay)
			if deleted {
				if err := f.s.DeleteItem(f.items[1].ID); err != nil {
					t.Fatal(err)
				}
			} else {
				f.exec(t, `UPDATE items SET fields='{"status":"done"}' WHERE id=?`, f.items[1].ID)
			}
			original := *f.scheds[0].NextAt
			res, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{State: "move", OccurrenceAt: rt(original), Until: ptr(rt("2030-03-05T08:00:00Z")), Actor: "user"}, rt("2030-03-06T12:00:00Z"))
			if err != nil {
				t.Fatal(err)
			}
			if res.MissedRecorded == 0 || res.Schedule.NextAt == nil || *res.Schedule.NextAt != "2030-03-07T09:00:00Z" || f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NULL`, f.scheds[0].ID) != 1 {
				t.Fatalf("inactive link changed plain move: %+v", res)
			}
		})
	}
}

func TestLinkedResolveLapseRefusalLeavesNoComment(t *testing.T) {
	f := dueLinkedFixture(t, 2, "lapses")
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	u := f.user(t, "linked-lapse@example.com", "UTC")
	// The caller cannot read the child's bound; it must still lapse.
	_, err := f.s.resolveOccurrenceAt(f.ws.ID, f.items[0].ID, ResolveInput{State: "move", OccurrenceAt: rt(*f.scheds[0].NextAt), Until: ptr(rt("2030-03-10T09:00:00Z")), Actor: "user", Comment: f.comment(f.items[0], u.ID, "Plans changed")}, rt("2030-03-06T12:00:00Z"))
	if !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("lapsing resolve = %v, want commitment changed", err)
	}
	if f.count(t, `SELECT COUNT(*) FROM comments`) != 0 || f.count(t, `SELECT COUNT(*) FROM activities WHERE action='commented'`) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`) != 0 {
		t.Fatal("lapse refusal left comment or moved dates")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE state='lapsed'`) != 1 || f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND change_type='missed_recorded'`) != 1 {
		t.Fatal("resolve lost lapse or miss")
	}
}
