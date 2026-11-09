package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Second-round review fixes on time links (TASK-4353).

func (f *linkFix) overrideModes(t *testing.T, i int) int {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_mode IS NOT NULL`)
}

// A finished child is history: an offset update neither clears its dragged
// offset nor journals a row for it.
func TestAnOffsetUpdateLeavesAFinishedChildsDraggedOffsetAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	f.setStatus(t, 1, "done")
	rows := f.journalRows(t, 1)
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &fiveDays}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if n := f.overrideModes(t, 1); n != 1 {
		t.Fatalf("%d offset overrides, want the finished child's drag intact", n)
	}
	if got := f.journalRows(t, 1); got != rows {
		t.Fatalf("%d journal rows written for a finished child", got-rows)
	}
}

// The reset is journalled under the caller's actor.
func TestReplacingADraggedOffsetJournalsTheCallersActor(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &fiveDays, Move: models.LinkWriteOptions{Actor: "agent:tern"}}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='update' AND actor='agent:tern' AND schedule_id=?`, f.scheds[1].ID); n != 1 {
		t.Fatalf("%d update rows under the caller's actor", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='update' AND actor='user' AND schedule_id=?`, f.scheds[1].ID); n != 0 {
		t.Fatalf("%d update rows under a hardcoded actor", n)
	}
}

// An ended parent plans nothing, but the saved offset must still replace the
// child's dragged one.
func TestAnOffsetUpdateReplacesADraggedOffsetWhenTheParentHasEnded(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil { // drag: three days
		t.Fatal(err)
	}
	f.exec(t, `UPDATE item_schedules SET ended_at='2030-03-04T10:00:00Z', next_at=NULL, next_wall=NULL WHERE id=?`, f.scheds[0].ID)
	childBefore, rows := f.role(t, 1, "target"), f.journalRows(t, 1)
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &fiveDays}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if n := f.overrideModes(t, 1); n != 0 {
		t.Fatalf("%d stale offset overrides survive an update under an ended parent", n)
	}
	if got := f.role(t, 1, "target"); got != childBefore {
		t.Fatalf("child moved %s -> %s with no parent date to follow", childBefore, got)
	}
	if got := f.journalRows(t, 1); got != rows+1 {
		t.Fatalf("%d new journal rows, want the one update row", got-rows)
	}
	// The parent comes back and moves: the child follows the new offset.
	f.exec(t, `UPDATE item_schedules SET ended_at=NULL, next_at=?, next_wall=? WHERE id=?`, "2030-03-04T09:00:00Z", "2030-03-04T09:00", f.scheds[0].ID)
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-15T09:00:00Z" {
		t.Fatalf("child target=%s after the parent re-armed, want the parent plus five days", got)
	}
}

// A stale revision is the first refusal, whatever else the apply would hit.
func TestAStaleRevisionIsRefusedBeforeAuthorityAndReason(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, f.scheds[1].ID)
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	f.setStatus(t, 1, "done")
	to := "2030-03-10T09:00:00Z"
	prev, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview = true })
	if err != nil || prev.RenegotiatesDeadline {
		t.Fatalf("preview=%+v %v", prev, err)
	}
	f.setStatus(t, 1, "open") // its linked hard deadline would now move
	rows := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
	deadline := f.role(t, 1, "deadline")
	for name, acc := range map[string]models.CommitmentAccess{"no authority": f.access(0), "no reason": f.allEdit()} {
		_, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Revision, in.Access = prev.Revision, acc })
		if !errors.Is(err, ErrCommitmentChanged) {
			t.Errorf("%s with a stale revision: err=%v, want ErrCommitmentChanged", name, err)
		}
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes`); n != rows || f.role(t, 1, "deadline") != deadline || f.role(t, 0, "target") != "2030-03-04T09:00:00Z" {
		t.Fatal("a stale apply wrote something")
	}
	// With no revision supplied the order stands: authority, then reason, then the revision.
	_, err = f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Access = f.access(0) })
	if !errors.Is(err, ErrDeadlineApprovalRequired) {
		t.Errorf("no revision, no authority: err=%v, want ErrDeadlineApprovalRequired", err)
	}
	var fe *ScheduleFieldError
	_, err = f.move(0, "target", to, nil)
	if !errors.As(err, &fe) || fe.Field != "reason" {
		t.Errorf("no revision, no reason: err=%v, want the reason refusal", err)
	}
	_, err = f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason = "slip" })
	if !errors.As(err, &fe) || fe.Field != "revision" {
		t.Errorf("no revision, with a reason: err=%v, want the revision refusal", err)
	}
	// A current revision still goes through to the later checks.
	cur, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview, in.Reason = true, "slip" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Revision, in.Reason = cur.Revision, "slip" }); err != nil {
		t.Fatalf("a current revision: %v", err)
	}
}

// A direct drag of a live child records the offset even when its parent is finished.
func TestADragBelowAFinishedParentIsKept(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.setStatus(t, 0, "done")
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil { // three days after the parent
		t.Fatal(err)
	}
	if n := f.overrideModes(t, 1); n != 1 {
		t.Fatalf("%d offset overrides after a drag below a finished parent, want 1", n)
	}
	f.setStatus(t, 0, "open")
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-13T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent plus the dragged three days", got)
	}
}

// The cascade still stops at a finished non-root node, with the drag recorded.
func TestADragBelowAFinishedParentStillStopsTheCascade(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	f.setStatus(t, 0, "done")
	grand := f.role(t, 2, "target")
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 2, "target"); got == grand {
		t.Fatalf("the open grandchild did not follow its parent")
	}
	f.setStatus(t, 1, "done")
	grand = f.role(t, 2, "target")
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 2, "target"); got != grand {
		t.Fatalf("the cascade passed a finished node: %s -> %s", grand, got)
	}
}

// The terminal flag alone distinguishes a finished child that has no change and
// no descendants.
func TestTheTerminalFlagAloneChangesTheRevision(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	// A move to the date the root already holds changes no date.
	to := f.role(t, 0, "target")
	prev, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview = true })
	if err != nil || len(prev.Changes) != 0 || len(prev.LeftInPlace) != 0 {
		t.Fatalf("setup: preview=%+v %v", prev, err)
	}
	f.setStatus(t, 1, "done")
	after, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Preview = true })
	if err != nil || len(after.Changes) != 0 || len(after.LeftInPlace) != 0 {
		t.Fatalf("setup: preview=%+v %v", after, err)
	}
	if after.Revision == prev.Revision {
		t.Fatal("finishing the child left the revision unchanged")
	}
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Revision = prev.Revision }); !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("stale apply: err=%v, want ErrCommitmentChanged", err)
	}
}
