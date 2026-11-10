package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4423_R01Carry(t *testing.T) {
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	if n, err := f.s.carryLinkCount(f.s.Q(), f.scheds[0].ID, nil); err != nil || n != 0 {
		t.Errorf("nil link predicate: count=%d err=%v, want 0", n, err)
	}
	bare := f.another(t, "no schedule")
	access := f.allEdit()
	access.Readable[bare.ID] = true
	access.Editable[bare.ID] = true
	if _, err := f.s.CreateScheduleGate(f.ws.ID, bare.ID, f.items[1].ID, access); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.ItemGatesDropped(f.ws.ID, bare.ID, nil); err != nil || n != 0 {
		t.Errorf("nil gate predicate: count=%d err=%v, want 0", n, err)
	}
	n, err := f.s.ItemGatesDropped(f.ws.ID, bare.ID, func(string) bool { return true })
	if err != nil || n != 1 {
		t.Fatalf("bare gate count=%d err=%v", n, err)
	}
	report, err := f.s.PreviewItemScheduleCarry(f.ws.ID, bare.ID, "", "", false, nil, func(string) bool { return true })
	if err != nil || report.HasSchedule || report.GatesDropped != 1 {
		t.Fatalf("bare gate report: %+v %v", report, err)
	}
	report, err = f.s.PreviewItemScheduleCarry(f.ws.ID, bare.ID, "", "", false, nil, nil)
	if err != nil || report.GatesDropped != 0 {
		t.Fatalf("nil bare gate report: %+v %v", report, err)
	}
}

func TestTASK4423_R16CarriedLineage(t *testing.T) {
	f := newLinkFix(t, 2)
	f.deadline(t, 1, "stays_owed")
	f.linkRaw(t, 0, 1, "target", "deadline", oneDay)
	in := models.LinkedMoveInput{ItemID: f.items[0].ID, Role: "target", To: "2030-04-01T09:00:00Z", Actor: "agent", Reason: "parent-private-reason", Access: f.allEdit(), Preview: true}
	p, err := f.s.MoveLinkedDate(f.ws.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Preview = false
	in.Revision = p.Revision
	in.AcknowledgeViolations = true
	if _, err = f.s.MoveLinkedDate(f.ws.ID, in); err != nil {
		t.Fatal(err)
	}
	source, err := f.s.GetCommitmentFor(f.ws.ID, f.items[1].ID, func(id string) bool { return id == f.items[1].ID })
	if err != nil {
		t.Fatal(err)
	}

	for _, orphan := range []bool{true, false} {
		t.Run(map[bool]string{true: "import orphan lineage", false: "import complete lineage"}[orphan], func(t *testing.T) {
			bundle, err := f.s.ExportWorkspace(f.ws.Slug)
			if err != nil {
				t.Fatal(err)
			}
			if orphan {
				items := bundle.Items[:0]
				for _, it := range bundle.Items {
					if it.ID == f.items[1].ID {
						items = append(items, it)
					}
				}
				bundle.Items = items
				schedules := bundle.Schedules[:0]
				for _, sc := range bundle.Schedules {
					if sc.ItemID == f.items[1].ID {
						schedules = append(schedules, sc)
					}
				}
				bundle.Schedules = schedules
				changes := bundle.ScheduleChanges[:0]
				for _, ch := range bundle.ScheduleChanges {
					if ch.ItemID == f.items[1].ID {
						changes = append(changes, ch)
					}
				}
				bundle.ScheduleChanges = changes
				bundle.ScheduleLinks = nil
			}
			imported, report, err := f.s.ImportWorkspaceWithReport(bundle, "Lineage import", "", "test")
			if err != nil {
				t.Fatal(err)
			}
			item, err := f.s.ResolveItem(imported.ID, f.items[1].Slug)
			if err != nil || item == nil {
				t.Fatalf("imported dependent: %v", err)
			}
			c, err := f.s.GetCommitmentFor(imported.ID, item.ID, func(string) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ch := range c.Journal {
				if ch.ChangeType != "move" {
					continue
				}
				found = true
				if orphan && (ch.Reason != "Date changed" || ch.Actor != "system" || ch.ReasonSource != "system") {
					t.Fatalf("import leaked orphan lineage: %+v", ch)
				}
				if !orphan && (ch.Reason != in.Reason || ch.Actor != in.Actor) {
					t.Fatalf("complete imported lineage changed: %+v; drops=%+v; source=%+v", ch, report.DroppedSchedules, bundle.ScheduleChanges)
				}
			}
			if !found {
				t.Fatal("imported move absent")
			}
		})
	}
	dest := createTestWorkspace(t, f.s, "carry dest")
	col := createTestCollection(t, f.s, dest.ID, "Tasks")
	for _, i := range []int{1, 0} {
		t.Run([]string{"own root", "foreign root"}[i], func(t *testing.T) {
			res, err := f.s.CopyItemAcrossWorkspaces(CrossWorkspaceCopyRequest{SourceItemID: f.items[i].ID, TargetWorkspaceID: dest.ID, TargetCollectionID: col.ID, Actor: "user", ArchiveSource: true})
			if err != nil {
				t.Fatal(err)
			}
			c, err := f.s.GetCommitmentFor(dest.ID, res.Item.ID, func(string) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, ch := range c.Journal {
				if ch.ChangeType != "move" {
					continue
				}
				found = true
				if i == 1 && (ch.Reason != "Date changed" || ch.Actor != "system" || ch.ReasonSource != "system") {
					t.Errorf("carried foreign lineage leaked: reason=%q actor=%q source=%q; source=%+v", ch.Reason, ch.Actor, ch.ReasonSource, source.Journal)
				}
				if i == 0 && (ch.Reason != in.Reason || ch.Actor != in.Actor) {
					t.Errorf("own reason/actor changed: %+v", ch)
				}
			}
			if !found {
				t.Fatal("carried move absent")
			}
		})
	}
}
