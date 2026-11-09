package store

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Time links, the cascade and their refusals (TASK-4353, 5b). Dates sit in
// 2030 so they are in the future of the real clock the store reads.

type linkFix struct {
	*schedFixture
	items  []*models.Item
	scheds []*models.ItemSchedule
}

func newLinkFix(t *testing.T, n int) *linkFix {
	t.Helper()
	f := &linkFix{schedFixture: newSchedFixture(t)}
	f.items = append(f.items, f.item)
	for i := 1; i < n; i++ {
		f.items = append(f.items, f.another(t, "linked"))
	}
	for i, it := range f.items {
		f.scheds = append(f.scheds, f.native(t, it, "2030-03-0"+string(rune('4'+i%5))+"T09:00", models.CommitmentRolesInput{}))
	}
	return f
}

func (f *schedFixture) native(t *testing.T, it *models.Item, start string, roles models.CommitmentRolesInput) *models.ItemSchedule {
	t.Helper()
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, it.ID, models.ScheduleCreate{Kind: "check", Dtstart: start, Tz: "UTC"}, roles)
	if err != nil {
		t.Fatalf("CreateCommitmentSchedule: %v", err)
	}
	return sc
}

func (f *linkFix) access(editable ...int) models.CommitmentAccess {
	a := models.CommitmentAccess{Readable: map[string]bool{}, Editable: map[string]bool{}}
	for _, it := range f.items {
		a.Readable[it.ID] = true
	}
	for _, i := range editable {
		a.Editable[f.items[i].ID] = true
	}
	return a
}

func (f *linkFix) allEdit() models.CommitmentAccess {
	var all []int
	for i := range f.items {
		all = append(all, i)
	}
	return f.access(all...)
}

var oneDay = models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 86400}

func (f *linkFix) link(t *testing.T, parent, child int, pr, cr string, off models.ScheduleLinkOffset) *models.ScheduleLink {
	t.Helper()
	res, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[parent].ID, ParentRole: pr, ChildItemID: f.items[child].ID, ChildRole: cr, Offset: off}, f.allEdit())
	if err != nil {
		t.Fatalf("CreateScheduleLink: %v", err)
	}
	return res.Link
}

// linkRaw stores a link without the move a link write makes, so a test can
// start from dates a link does not produce.
func (f *linkFix) linkRaw(t *testing.T, parent, child int, pr, cr string, off models.ScheduleLinkOffset) {
	t.Helper()
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	l := linkRow{ID: newID(), ParentSchedule: f.scheds[parent].ID, ParentRole: pr, ChildSchedule: f.scheds[child].ID, ChildRole: cr, Offset: off, CancelPolicy: models.LinkWaitingOn, CreatedAt: nowNano(), UpdatedAt: nowNano()}
	if err := f.s.insertLinkTx(tx, f.ws.ID, l); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *linkFix) role(t *testing.T, i int, role string) string {
	t.Helper()
	c, err := f.s.GetCommitment(f.ws.ID, f.items[i].ID)
	if err != nil {
		t.Fatal(err)
	}
	var v *string
	switch role {
	case "target":
		v = c.Roles.Target
	case "deadline":
		v = c.Roles.Deadline
	}
	if v == nil {
		return ""
	}
	return *v
}

func (f *linkFix) move(i int, role, to string, mut func(*models.LinkedMoveInput)) (*models.LinkedMoveResult, error) {
	in := models.LinkedMoveInput{ItemID: f.items[i].ID, Role: role, To: to, Actor: "user", Access: f.allEdit()}
	if mut != nil {
		mut(&in)
	}
	return f.s.MoveLinkedDate(f.ws.ID, in)
}

func TestLinkWriteNeedsEditOnBothEnds(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	create := models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[1].ID, ChildRole: "target", Offset: oneDay}
	for name, acc := range map[string]models.CommitmentAccess{
		"edit on the child, read-only parent": f.access(1),
		"edit on the parent, read-only child": f.access(0),
		"read-only on both":                   f.access(),
	} {
		if _, err := f.s.CreateScheduleLink(f.ws.ID, create, acc); !errors.Is(err, ErrLinkEditRequired) {
			t.Fatalf("%s: err=%v, want ErrLinkEditRequired", name, err)
		}
	}
	unreadable := f.access(0, 1)
	delete(unreadable.Readable, f.items[0].ID)
	if _, err := f.s.CreateScheduleLink(f.ws.ID, create, unreadable); !errors.Is(err, ErrScheduleLinkNotFound) {
		t.Fatalf("unreadable parent: err=%v, want not found", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 0 {
		t.Fatalf("%d links written by refused creates", n)
	}
	l := f.link(t, 0, 1, "target", "target", oneDay)
	two := models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 172800}
	if _, err := f.s.UpdateScheduleLink(f.ws.ID, l.ID, models.ScheduleLinkUpdate{Offset: &two}, f.access(1)); !errors.Is(err, ErrLinkEditRequired) {
		t.Fatalf("update with a read-only parent: %v", err)
	}
	if err := f.s.DeleteScheduleLink(f.ws.ID, l.ID, f.access(1)); !errors.Is(err, ErrLinkEditRequired) {
		t.Fatalf("delete with a read-only parent: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links WHERE offset_elapsed=86400`); n != 1 {
		t.Fatalf("refused update or delete changed the link (%d)", n)
	}
	// An agent token is the same decision: edit on the child alone is refused.
	if _, err := f.s.CreateScheduleLink(f.ws.ID, create, f.access(1)); !errors.Is(err, ErrLinkEditRequired) {
		t.Fatal("agent with child edit only was not refused")
	}
}

func TestLinkRefusalsNameNoItemAndShowNoCount(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[2].ID, ParentRole: "target", ChildItemID: f.items[0].ID, ChildRole: "target", Offset: oneDay}, f.allEdit())
	var refused *LinkRefusedError
	if !errors.As(err, &refused) || refused.Code != "link_cycle" {
		t.Fatalf("cycle: %v", err)
	}
	for _, id := range append(scheduleIDs(f.scheds), itemIDs(f.items)...) {
		if strings.Contains(err.Error(), id) {
			t.Fatalf("cycle refusal names %s: %v", id, err)
		}
	}
	// Two links into the same child role are one date written twice.
	_, err = f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: f.items[2].ID, ChildRole: "target", Offset: oneDay}, f.allEdit())
	if !errors.As(err, &refused) || refused.Code != "link_duplicate_incoming" {
		t.Fatalf("duplicate incoming: %v", err)
	}
}

func scheduleIDs(s []*models.ItemSchedule) (out []string) {
	for _, x := range s {
		out = append(out, x.ID)
	}
	return
}

func itemIDs(s []*models.Item) (out []string) {
	for _, x := range s {
		out = append(out, x.ID)
	}
	return
}

func TestLinkLimitsAreCheckedWhenALinkIsWritten(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 19)
	for i := 0; i < 16; i++ {
		f.link(t, i, i+1, "target", "target", oneDay)
	}
	// 16 hops are admitted; a 17th is refused at write time, with no count in the refusal.
	_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[16].ID, ParentRole: "target", ChildItemID: f.items[17].ID, ChildRole: "target", Offset: oneDay}, f.allEdit())
	var refused *LinkRefusedError
	if !errors.As(err, &refused) || refused.Code != "link_limit" || strings.ContainsAny(err.Error(), "0123456789") {
		t.Fatalf("hop limit: %v", err)
	}
	g := newLinkFix(t, 53)
	for i := 1; i <= 50; i++ {
		g.link(t, 0, i, "target", "target", oneDay)
	}
	// A 51st dependent anywhere below the root enlarges the root footprint past 50.
	_, err = g.s.CreateScheduleLink(g.ws.ID, models.ScheduleLinkCreate{ParentItemID: g.items[1].ID, ParentRole: "target", ChildItemID: g.items[51].ID, ChildRole: "target", Offset: oneDay}, g.allEdit())
	if !errors.As(err, &refused) || refused.Code != "link_limit" || strings.ContainsAny(err.Error(), "0123456789") {
		t.Fatalf("affected limit: %v", err)
	}
	// A role the commitment does not have is refused for that reason.
	_, err = g.s.CreateScheduleLink(g.ws.ID, models.ScheduleLinkCreate{ParentItemID: g.items[52].ID, ParentRole: "target", ChildItemID: g.items[51].ID, ChildRole: "deadline", Offset: oneDay}, g.allEdit())
	var field *ScheduleFieldError
	if !errors.As(err, &field) {
		t.Fatalf("missing child role: %v", err)
	}
}

func TestCascadeMovesLinkedTargetsAndKeepsOneLineage(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	res, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 3 || res.RenegotiatesDeadline || res.Revision == "" {
		t.Fatalf("result=%+v", res)
	}
	for i, want := range []string{"2030-03-10T09:00:00Z", "2030-03-11T09:00:00Z", "2030-03-12T09:00:00Z"} {
		if got := f.role(t, i, "target"); got != want {
			t.Fatalf("item %d target=%s, want %s", i, got, want)
		}
		sc := f.reload(t, f.items[i].ID)
		if r := f.managed(t, sc); r.RemindAt != want {
			t.Fatalf("item %d reminder=%s, want it to follow the target %s", i, r.RemindAt, want)
		}
	}
	rows := moveEntries(t, f.schedFixture, f.scheds[2].ID)
	root := moveEntries(t, f.schedFixture, f.scheds[0].ID)
	if len(rows) == 0 || len(root) == 0 {
		t.Fatal("no journal rows")
	}
	last, first := rows[len(rows)-1], root[len(root)-1]
	if last.RootChangeID != first.ID || last.ParentChangeID == nil || *last.ParentChangeID != first.ID {
		t.Fatalf("lineage root=%s parent=%v, want both %s", last.RootChangeID, last.ParentChangeID, first.ID)
	}
}

func TestCascadeLeavesAChildDeadlineAndCrossingABoundNeedsAcknowledgement(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	// The child keeps its own hard deadline two days after its target.
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id=?`, f.scheds[1].ID)
	f.link(t, 0, 1, "target", "target", oneDay)
	deadline := f.role(t, 1, "deadline")
	if res, err := f.move(0, "target", "2030-03-05T09:00:00Z", nil); err != nil || res.RenegotiatesDeadline {
		t.Fatalf("a target-only cascade stays cheap: %+v %v", res, err)
	}
	if got := f.role(t, 1, "deadline"); got != deadline {
		t.Fatalf("child deadline moved %s -> %s", deadline, got)
	}
	// A move that carries the child's target past its own deadline is case 10.
	prev, err := f.move(0, "target", "2030-03-20T09:00:00Z", func(in *models.LinkedMoveInput) { in.Preview = true })
	if err != nil || len(prev.Violations) != 1 || prev.Violations[0].ItemID != f.items[1].ID {
		t.Fatalf("preview=%+v %v", prev, err)
	}
	if _, err := f.move(0, "target", "2030-03-20T09:00:00Z", nil); err == nil {
		t.Fatal("crossing a bound applied without a preview")
	}
	if _, err := f.move(0, "target", "2030-03-20T09:00:00Z", func(in *models.LinkedMoveInput) { in.Revision = prev.Revision }); !errors.Is(err, ErrLinkViolationAck) {
		t.Fatalf("without acknowledgement: %v", err)
	}
	if _, err := f.move(0, "target", "2030-03-20T09:00:00Z", func(in *models.LinkedMoveInput) { in.Revision, in.AcknowledgeViolations = prev.Revision, true }); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "deadline"); got != deadline {
		t.Fatalf("the bound did not stay: %s", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_links`); n != 1 {
		t.Fatal("the link did not stay")
	}
}

func TestLinkToAChildDeadlineIsARenegotiation(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.exec(t, `UPDATE item_schedules SET roles_origin='native', deadline_days=2, deadline_secs=0, deadline_behavior='stays_owed' WHERE id IN (?,?)`, f.scheds[1].ID, f.scheds[2].ID)
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	f.linkRaw(t, 0, 2, "target", "deadline", oneDay)
	before := f.role(t, 1, "deadline")
	const to = "2030-03-10T09:00:00Z"
	// No authority over one dependent's deadline, then over both: the refusal is the same value.
	_, one := f.move(0, "target", to, func(in *models.LinkedMoveInput) {
		in.Reason, in.Access = "slip", f.access(0, 1)
	})
	_, both := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason, in.Access = "slip", f.access(0) })
	if !errors.Is(one, ErrDeadlineApprovalRequired) || !errors.Is(both, ErrDeadlineApprovalRequired) || one.Error() != both.Error() || one != both {
		t.Fatalf("one=%v both=%v", one, both)
	}
	// Nothing is stored as a pending move.
	for _, tbl := range []string{"schedule_changes", "schedule_occurrence_overrides"} {
		if n := f.count(t, `SELECT COUNT(*) FROM `+tbl+` WHERE `+map[string]string{"schedule_changes": "change_type='move'", "schedule_occurrence_overrides": "role='deadline'"}[tbl]); n != 0 {
			t.Fatalf("%s holds %d rows after a refusal", tbl, n)
		}
	}
	if _, err := f.move(0, "target", to, nil); err == nil || errors.Is(err, ErrDeadlineApprovalRequired) {
		t.Fatalf("a missing reason: %v", err)
	}
	prev, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason, in.Preview = "slip", true })
	if err != nil || !prev.RenegotiatesDeadline || len(prev.Changes) != 3 {
		t.Fatalf("preview=%+v %v", prev, err)
	}
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason = "slip" }); err == nil {
		t.Fatal("applied a renegotiation without the revision")
	}
	// Permission lost after the preview: refused, nothing written.
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) {
		in.Reason, in.Revision, in.Access = "slip", prev.Revision, f.access(0, 2)
	}); !errors.Is(err, ErrDeadlineApprovalRequired) {
		t.Fatalf("lost authority: %v", err)
	}
	if got := f.role(t, 1, "deadline"); got != before {
		t.Fatalf("deadline changed to %s", got)
	}
	// A stale token is the existing commitment_changed refusal.
	f.exec(t, `UPDATE item_schedules SET updated_at='2031-01-01T00:00:00Z' WHERE id=?`, f.scheds[2].ID)
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason, in.Revision = "slip", prev.Revision }); !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("stale: %v", err)
	}
	prev, _ = f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason, in.Preview = "slip", true })
	if _, err := f.move(0, "target", to, func(in *models.LinkedMoveInput) { in.Reason, in.Revision = "slip", prev.Revision }); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "deadline"); got != "2030-03-11T09:00:00Z" {
		t.Fatalf("deadline=%s", got)
	}
	if len(prev.Revision) != 43 {
		t.Fatalf("token length %d", len(prev.Revision))
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND reason='slip' AND reason_source='user'`); n != 2 {
		t.Fatalf("%d renegotiation journal rows, want 2 under one reason", n)
	}
}

func TestLinkedMoveShowsAReaderOnlyWhatTheyCanRead(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 0, 2, "target", "target", oneDay)
	acc := f.allEdit()
	delete(acc.Readable, f.items[2].ID)
	res, err := f.move(0, "target", "2030-03-10T09:00:00Z", func(in *models.LinkedMoveInput) { in.Access = acc })
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changes) != 2 {
		t.Fatalf("changes=%+v, want the root and the readable child only", res.Changes)
	}
	// The hidden child still moved: the cascade is not limited by what the mover sees.
	if got := f.role(t, 2, "target"); got != "2030-03-11T09:00:00Z" {
		t.Fatalf("hidden child target=%s", got)
	}
	// The link detail is not shown to a reader who cannot read both ends.
	if got, err := f.s.ListScheduleLinks(f.ws.ID, f.items[0].ID, acc); err != nil || len(got) != 1 || got[0].ChildItemID != f.items[1].ID {
		t.Fatalf("links=%+v %v", got, err)
	}
	child := f.access(1, 2)
	delete(child.Readable, f.items[0].ID)
	child.Readable[f.items[2].ID] = true
	if got, err := f.s.ListScheduleLinks(f.ws.ID, f.items[2].ID, child); err != nil || len(got) != 0 {
		t.Fatalf("a child whose parent is unreadable lists %+v %v", got, err)
	}
}

func TestLinkedMoveOverTheTimeBudgetWritesNothing(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 1, 2, "target", "target", oneDay)
	var tick time.Time = time.Now()
	restore := f.s.SetLinkClockForTesting(func() time.Time { tick = tick.Add(2 * time.Second); return tick })
	defer restore()
	_, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil)
	if !errors.Is(err, ErrLinkBudget) || IsDBOutcomeUnknown(err) {
		t.Fatalf("err=%v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`) + f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides WHERE role='target'`); n != 0 {
		t.Fatalf("%d rows written over budget", n)
	}
	if got := f.role(t, 0, "target"); got != "2030-03-04T09:00:00Z" {
		t.Fatalf("root moved to %s", got)
	}
}

func TestDraggingAChildKeepsTheLinkAtTheNewOffset(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(1, "target", "2030-03-07T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_elapsed=? AND offset_mode='elapsed'`, 3*86400); n != 1 {
		t.Fatalf("override rows=%d, want one at the new offset", n)
	}
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-13T09:00:00Z" {
		t.Fatalf("child target=%s, want the parent plus the new offset", got)
	}
}

func TestCascadeLeavesAnArchivedChildAlone(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 3)
	f.link(t, 0, 1, "target", "target", oneDay)
	f.link(t, 0, 2, "target", "target", oneDay)
	if err := f.s.DeleteItem(f.items[2].ID); err != nil {
		t.Fatal(err)
	}
	res, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil)
	if err != nil || len(res.Changes) != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := f.role(t, 1, "target"); got != "2030-03-11T09:00:00Z" {
		t.Fatalf("live child=%s", got)
	}
}

func TestLinkedDateOutsideTheSupportedYearsInTheChildZoneIsRefused(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.exec(t, `UPDATE item_schedules SET tz='Asia/Tokyo' WHERE id=?`, f.scheds[1].ID)
	// 2200-12-31T20:00Z is still 2200 in UTC but 2201-01-01 05:00 in Tokyo.
	f.link(t, 0, 1, "target", "target", models.ScheduleLinkOffset{Mode: "elapsed", ElapsedSecs: 3600})
	written := func() int {
		return f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`) + f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`)
	}
	before := written()
	if _, err := f.move(0, "target", "2200-12-31T19:30:00Z", nil); err == nil {
		t.Fatal("a derived Tokyo date in 2201 was stored")
	}
	if n := written() - before; n != 0 {
		t.Fatalf("%d rows written", n)
	}
	if _, err := f.move(0, "target", "2200-12-30T09:00:00Z", nil); err != nil {
		t.Fatalf("inside the range: %v", err)
	}
}

func TestLinksStayWithinOneWorkspace(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	other := createTestWorkspace(t, f.s, "Other")
	col := createTestCollection(t, f.s, other.ID, "Tasks")
	foreign := createTestItem(t, f.s, other.ID, col.ID, "foreign", "")
	if _, err := f.s.CreateCommitmentSchedule(other.ID, foreign.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC"}, models.CommitmentRolesInput{}); err != nil {
		t.Fatal(err)
	}
	acc := f.allEdit()
	acc.Readable[foreign.ID], acc.Editable[foreign.ID] = true, true
	_, err := f.s.CreateScheduleLink(f.ws.ID, models.ScheduleLinkCreate{ParentItemID: f.items[0].ID, ParentRole: "target", ChildItemID: foreign.ID, ChildRole: "target", Offset: oneDay}, acc)
	if !errors.Is(err, ErrScheduleLinkNotFound) {
		t.Fatalf("cross-workspace link: %v", err)
	}
}
