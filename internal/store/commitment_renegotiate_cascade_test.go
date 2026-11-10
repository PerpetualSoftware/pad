package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func deadlineLinkFixture(t *testing.T, n int) *linkFix {
	t.Helper()
	f := newLinkFix(t, n)
	for _, sc := range f.scheds {
		f.exec(t, `UPDATE item_schedules SET deadline_days=0,deadline_secs=3600,deadline_behavior='stays_owed' WHERE id=?`, sc.ID)
	}
	return f
}
func (f *linkFix) renegotiateLinked(i int, in models.CommitmentRenegotiateInput) (*models.CommitmentRenegotiation, error) {
	return f.s.renegotiateCommitmentAt(f.ws.ID, f.items[i].ID, "user", in, rt("2030-03-03T12:00:00Z"), nil)
}
func linkedRenegotiationInput() models.CommitmentRenegotiateInput {
	return models.CommitmentRenegotiateInput{Scope: "occurrence", Deadline: ptr("2030-03-10T10:00:00Z"), Reason: "Changed plans", Preview: true}
}

func TestLinkedOccurrenceRenegotiationCascades(t *testing.T) {
	f := deadlineLinkFixture(t, 2)
	f.linkRaw(t, 0, 1, "deadline", "target", oneDay)
	in := linkedRenegotiationInput()
	in.Access = f.allEdit()
	// Moving the child's target beyond its own deadline requires acknowledgement;
	// keep its deadline later so this test checks a successful renegotiation.
	f.exec(t, `UPDATE item_schedules SET deadline_days=30 WHERE id=?`, f.scheds[1].ID)
	before := renegotiateState(t, f.s)
	p, err := f.renegotiateLinked(0, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cascade == nil || len(p.Cascade.Changes) != 2 || len(p.Revision) != 43 {
		t.Fatalf("preview missing cascade: %+v", p)
	}
	if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
		t.Fatal("preview wrote rows")
	}
	in.Preview, in.Revision = false, p.Revision
	a, err := f.renegotiateLinked(0, in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p, a) || f.role(t, 1, "target") != "2030-03-11T10:00:00Z" || f.role(t, 0, "deadline") != "2030-03-10T10:00:00Z" {
		t.Fatalf("renegotiation did not cascade: %+v", a)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`) != 2 || f.count(t, `SELECT COUNT(DISTINCT root_change_id) FROM schedule_changes WHERE change_type='move'`) != 1 {
		t.Fatal("renegotiation duplicated or split journal lineage")
	}
}

func TestLinkedOccurrenceRenegotiationRejectsChangedChildFirst(t *testing.T) {
	f := deadlineLinkFixture(t, 2)
	f.linkRaw(t, 0, 1, "deadline", "deadline", oneDay)
	in := linkedRenegotiationInput()
	in.Access = f.allEdit()
	p, err := f.renegotiateLinked(0, in)
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE item_schedules SET updated_at='2030-03-03T12:01:00Z' WHERE id=?`, f.scheds[1].ID)
	in.Preview, in.Revision, in.Access = false, p.Revision, models.CommitmentAccess{}
	before := renegotiateState(t, f.s)
	_, err = f.renegotiateLinked(0, in)
	if !errors.Is(err, ErrCommitmentChanged) {
		t.Fatalf("changed child = %v, want commitment changed before approval refusal", err)
	}
	if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
		t.Fatal("stale apply wrote rows")
	}
}

func TestLinkedOccurrenceRenegotiationKeepsIncomingOffset(t *testing.T) {
	f := deadlineLinkFixture(t, 2)
	f.linkRaw(t, 0, 1, "deadline", "deadline", oneDay)
	in := linkedRenegotiationInput()
	// Root authority was already decided by the caller; Access may be empty here.
	p, err := f.renegotiateLinked(1, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Preview, in.Revision = false, p.Revision
	if _, err := f.renegotiateLinked(1, in); err != nil {
		t.Fatal(err)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides WHERE offset_elapsed=518400`) != 1 {
		t.Fatal("renegotiation did not keep incoming link at new offset")
	}
	p2, err := f.move(0, "deadline", "2030-03-12T10:00:00Z", func(in *models.LinkedMoveInput) { in.Preview, in.Reason = true, "Changed again" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.move(0, "deadline", "2030-03-12T10:00:00Z", func(in *models.LinkedMoveInput) { in.Revision, in.Reason = p2.Revision, "Changed again" }); err != nil {
		t.Fatal(err)
	}
	if f.role(t, 1, "deadline") != "2030-03-18T10:00:00Z" {
		t.Fatal("parent overwrote renegotiated child offset")
	}
}

func TestLinkedOccurrenceRenegotiationHidesDependentApproval(t *testing.T) {
	var answer string
	for _, n := range []int{2, 3} {
		f := deadlineLinkFixture(t, n)
		for i := 1; i < n; i++ {
			f.linkRaw(t, 0, i, "deadline", "deadline", oneDay)
		}
		in := linkedRenegotiationInput()
		before := renegotiateState(t, f.s)
		_, err := f.renegotiateLinked(0, in)
		if !errors.Is(err, ErrDeadlineApprovalRequired) {
			t.Fatalf("%d hidden dependents: %v, want deadline approval required", n-1, err)
		}
		if answer != "" && answer != err.Error() {
			t.Fatal("approval refusal revealed count")
		}
		answer = err.Error()
		if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
			t.Fatal("approval refusal wrote rows")
		}
	}
}

func TestLinkedOccurrenceRenegotiationTargetOnlyKeepsPlainPath(t *testing.T) {
	f := deadlineLinkFixture(t, 2)
	f.linkRaw(t, 0, 1, "target", "target", oneDay)
	in := linkedRenegotiationInput()
	p, err := f.renegotiateLinked(0, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.Cascade != nil {
		t.Fatal("target-only link entered deadline cascade")
	}
	in.Preview, in.Revision = false, p.Revision
	if _, err := f.renegotiateLinked(0, in); err != nil {
		t.Fatal(err)
	}
	if f.role(t, 1, "target") != "2030-03-05T09:00:00Z" || f.role(t, 0, "deadline") != "2030-03-10T10:00:00Z" {
		t.Fatal("target-only link changed plain renegotiation")
	}
}

func TestLinkedOccurrenceRenegotiationLapsesAllDueNodes(t *testing.T) {
	for _, rootLapses := range []bool{false, true} {
		t.Run(map[bool]string{false: "child", true: "root-and-child"}[rootLapses], func(t *testing.T) {
			f := deadlineLinkFixture(t, 2)
			f.linkRaw(t, 0, 1, "deadline", "deadline", oneDay)
			f.exec(t, `UPDATE item_schedules SET deadline_behavior='lapses' WHERE id=?`, f.scheds[1].ID)
			if rootLapses {
				f.exec(t, `UPDATE item_schedules SET deadline_behavior='lapses' WHERE id=?`, f.scheds[0].ID)
			}
			in := linkedRenegotiationInput()
			in.Access = f.allEdit()
			call := func() (*models.CommitmentRenegotiation, error) {
				return f.s.renegotiateCommitmentAt(f.ws.ID, f.items[0].ID, "user", in, rt("2030-03-06T12:00:00Z"), nil)
			}
			p, err := call()
			if err != nil {
				t.Fatal(err)
			}
			in.Preview, in.Revision = false, p.Revision
			_, err = call()
			if !errors.Is(err, ErrCommitmentChanged) {
				t.Fatalf("lapsing renegotiation = %v, want commitment changed", err)
			}
			want := 1
			if rootLapses {
				want = 2
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE state='lapsed'`) != want || f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND change_type='missed_recorded'`) != 2 {
				t.Fatal("renegotiation did not close every due lapsing occurrence")
			}
			if f.count(t, `SELECT COUNT(*) FROM schedule_occurrence_overrides`)+f.count(t, `SELECT COUNT(*) FROM schedule_link_overrides`)+f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='move'`) != 0 {
				t.Fatal("lapse refusal moved dates or wrote offsets")
			}
		})
	}
}

func TestLinkedOccurrenceRenegotiationAcknowledgesReadableBound(t *testing.T) {
	f := deadlineLinkFixture(t, 2)
	f.linkRaw(t, 0, 1, "deadline", "target", oneDay)
	in := linkedRenegotiationInput()
	in.Access = f.allEdit()
	p, err := f.renegotiateLinked(0, in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Cascade.Violations) != 1 {
		t.Fatalf("preview violations = %+v", p.Cascade.Violations)
	}
	in.Preview, in.Revision = false, p.Revision
	before := renegotiateState(t, f.s)
	if _, err := f.renegotiateLinked(0, in); !errors.Is(err, ErrLinkViolationAck) {
		t.Fatalf("unacknowledged = %v, want violation acknowledgement", err)
	}
	if !reflect.DeepEqual(before, renegotiateState(t, f.s)) {
		t.Fatal("refused apply wrote rows")
	}
	in.AcknowledgeViolations = true
	if _, err := f.renegotiateLinked(0, in); err != nil {
		t.Fatalf("acknowledged: %v", err)
	}
	if f.role(t, 1, "target") != "2030-03-11T10:00:00Z" {
		t.Fatal("the acknowledged apply did not move the child")
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND role='target' AND reason='Moved with a linked date' AND reason_source='system'`, f.scheds[1].ID) != 1 {
		t.Fatal("a dependent target took the deadline's reason")
	}
}

func TestLapseDueInPlanOnlyRefusesAClosedOccurrence(t *testing.T) {
	f := dueLinkedFixture(t, 2, "lapses")
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	// An occurrence that cannot be expanded keeps its miss and stays outstanding.
	f.exec(t, `UPDATE item_schedules SET rrule='NOT A RULE' WHERE id=?`, f.scheds[1].ID)
	p, err := f.move(0, "target", "2030-03-10T09:00:00Z", func(in *models.LinkedMoveInput) { in.Preview, in.Reason = true, "Changed plans" })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", func(in *models.LinkedMoveInput) { in.Revision, in.Reason = p.Revision, "Changed plans" }); err != nil {
		t.Fatalf("apply = %v, want the plan applied", err)
	}
	if f.count(t, `SELECT COUNT(*) FROM schedule_occurrences WHERE state='lapsed'`) != 0 || f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND change_type='missed_recorded'`) != 1 {
		t.Fatal("expected one miss and no lapse")
	}
	if f.role(t, 1, "deadline") != "2030-03-11T09:00:00Z" {
		t.Fatal("the plan did not apply")
	}
}
