package store

import (
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A link supplies the child's date (TASK-4353): writing one, or changing its
// offset, moves the child's outstanding occurrence to the parent plus the
// offset through the same plan as a cascade. Fixture dates: item 0 is
// 2030-03-04 09:00 UTC, item 1 a day later, item 2 two days later.

var threeDays = models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 3 * 86400}

func TestCreatingALinkMovesTheChildToTheParentPlusTheOffset(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", threeDays)
	if got := f.role(t, 1, "target"); got != "2030-03-07T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent plus three days", got)
	}
	assertMoveArm(t, f.schedFixture, f.scheds[1], "2030-03-07T09:00:00Z")
	moves := 0
	for _, r := range moveEntries(t, f.schedFixture, f.scheds[1].ID) {
		if r.ChangeType == "move" && r.Role != nil && *r.Role == "target" {
			moves++
		}
	}
	if moves != 1 {
		t.Fatalf("%d target move rows, want one", moves)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-04T09:00:00Z" {
		t.Fatalf("the parent moved to %s", got)
	}
}

func TestUpdatingALinkOffsetMovesTheChild(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &threeDays}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-07T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent plus three days", got)
	}
	assertMoveArm(t, f.schedFixture, f.scheds[1], "2030-03-07T09:00:00Z")
}

func TestALinkWhoseOffsetAlreadyMatchesMovesNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	l := f.link(t, 0, 1, "target", "target", oneDay) // the child is already a day after the parent
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &oneDay}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`); n != 0 {
		t.Fatalf("%d journal rows for a link that matches", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`); n != 0 {
		t.Fatalf("%d overrides for a link that matches", n)
	}
	if sc := f.reload(t, f.items[1].ID); sc.NextOverrideAt != nil || sc.UpdatedAt != f.scheds[1].UpdatedAt {
		t.Fatalf("the child schedule was written: %+v", sc)
	}
}

func (f *linkFix) create(parent, child int, pr, cr string, off models.ScheduleLinkOffset, mv models.LinkWriteOptions, acc models.CommitmentAccess) (*models.ScheduleLinkResult, error) {
	return f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[parent].ID, ParentRole: pr, ChildItemID: f.items[child].ID, ChildRole: cr, Offset: off, Move: mv}, acc)
}

func TestALinkPreviewMovesAndStoresNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	res, err := f.create(0, 1, "target", "target", threeDays, models.LinkWriteOptions{Preview: true}, f.allEdit())
	if err != nil || res.Link != nil || len(res.Moved.Changes) != 1 || res.Moved.Changes[0].ItemID != f.items[1].ID || res.Moved.Revision == "" {
		t.Fatalf("preview=%+v %v", res, err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`) + f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`) + f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`); n != 0 {
		t.Fatalf("a preview wrote %d rows", n)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-05T09:00:00Z" {
		t.Fatalf("a preview moved the child to %s", got)
	}
}

func TestALinkToADeadlineIsARenegotiationAtWriteTime(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id IN (?,?)`, f.scheds[1].ID, f.scheds[2].ID)
	// Item 1's deadline is 03-07; a link from item 0's target (03-04) a day ahead puts it on 03-05.
	if _, err := f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{}, f.allEdit()); err == nil {
		t.Fatal("a deadline moved without a reason")
	}
	prev, err := f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{Reason: "slip", Preview: true}, f.allEdit())
	if err != nil || !prev.Moved.RenegotiatesDeadline || prev.Link != nil {
		t.Fatalf("preview=%+v %v", prev, err)
	}
	if _, err := f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{Reason: "slip"}, f.allEdit()); err == nil {
		t.Fatal("a renegotiation applied without the preview revision")
	}
	// Authority over the deadline it changes: item 1 is edited, but a deadline below it is not.
	f.linkRaw(t, 1, 2, "deadline", "deadline", oneDay)
	// The link above changed the component, so the first preview is stale: take a current one.
	prev, err = f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{Reason: "slip", Preview: true}, f.allEdit())
	if err != nil {
		t.Fatal(err)
	}
	_, one := f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{Reason: "slip", Revision: prev.Moved.Revision}, f.access(0, 1))
	if !errors.Is(one, ErrDeadlineApprovalRequired) {
		t.Fatalf("a hidden dependent deadline: %v", one)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE parent_schedule_id=?`, f.scheds[0].ID); n != 0 {
		t.Fatal("a refused link write stored the link")
	}
	prev, err = f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{Reason: "slip", Preview: true}, f.allEdit())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(0, 1, "target", "deadline", oneDay, models.LinkWriteOptions{Reason: "slip", Revision: prev.Moved.Revision}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "deadline"); got != "2030-03-05T09:00:00Z" {
		t.Fatalf("deadline=%s", got)
	}
	if got := f.role(t, 2, "deadline"); got != "2030-03-06T09:00:00Z" {
		t.Fatalf("the deadline below it=%s, want it to follow", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND reason='slip' AND reason_source='user'`); n != 2 {
		t.Fatalf("%d renegotiation rows, want 2 under one reason", n)
	}
}

func TestALinkThatCrossesTheChildsOwnBoundNeedsAcknowledgement(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, f.scheds[1].ID)
	// The child's deadline is 03-07; a target ten days after the parent lands past it.
	ten := models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 10 * 86400}
	prev, err := f.create(0, 1, "target", "target", ten, models.LinkWriteOptions{Preview: true}, f.allEdit())
	if err != nil || len(prev.Moved.Violations) != 1 {
		t.Fatalf("preview=%+v %v", prev, err)
	}
	if _, err := f.create(0, 1, "target", "target", ten, models.LinkWriteOptions{}, f.allEdit()); err == nil {
		t.Fatal("crossing a bound applied without a preview")
	}
	if _, err := f.create(0, 1, "target", "target", ten, models.LinkWriteOptions{Revision: prev.Moved.Revision}, f.allEdit()); !errors.Is(err, ErrLinkViolationAck) {
		t.Fatalf("without acknowledgement: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 0 {
		t.Fatal("a refused link write stored the link")
	}
	if _, err := f.create(0, 1, "target", "target", ten, models.LinkWriteOptions{Revision: prev.Moved.Revision, AcknowledgeViolations: true}, f.allEdit()); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "deadline"); got != "2030-03-07T09:00:00Z" {
		t.Fatalf("the bound did not stay: %s", got)
	}
}

func TestALinkWriteOverTheTimeBudgetStoresNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	tick := time.Now()
	restore := f.s.SetLinkClockForTesting(func() time.Time { tick = tick.Add(2 * time.Second); return tick })
	defer restore()
	if _, err := f.create(0, 1, "target", "target", threeDays, models.LinkWriteOptions{}, f.allEdit()); !errors.Is(err, ErrLinkBudget) {
		t.Fatalf("err=%v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`) + f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`); n != 0 {
		t.Fatalf("%d rows written over budget", n)
	}
}

// A parent's own incoming link is not a drag: writing a link below it must not
// record an offset override for the parent.
func TestALinkWriteBelowALinkedParentRecordsNoDrag(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", threeDays)
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides`); n != 0 {
		t.Fatalf("%d link overrides after link writes", n)
	}
	if got := f.role(t, 2, "target"); got != "2030-03-08T09:00:00Z" {
		t.Fatalf("grandchild=%s", got)
	}
}
