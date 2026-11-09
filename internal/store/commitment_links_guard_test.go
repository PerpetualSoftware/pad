package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A schedule that is an end of a link is edited only where the edit cannot
// shift a date the link supplies (TASK-4353, 2a). Without the refusal a timing
// edit leaves the child skewed: it keeps the date the parent no longer implies.

func (f *linkFix) rowState(t *testing.T, i int) string {
	t.Helper()
	var dtstart, updated string
	if err := f.s.db.QueryRow(f.s.q(`SELECT dtstart, updated_at FROM item_schedules WHERE id=?`), f.scheds[i].ID).Scan(&dtstart, &updated); err != nil {
		t.Fatal(err)
	}
	return dtstart + "|" + updated + "|" + f.role(t, i, "target")
}

func sp[T any](v T) *T { return &v }

func TestATimingEditOfALinkedScheduleIsRefusedAndWritesNothing(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"child", "parent"} {
		for name, upd := range map[string]models.ScheduleUpdate{
			"dtstart":  {Dtstart: sp("2030-03-20T09:00")},
			"zone":     {Tz: sp("America/New_York")},
			"rule":     {Rrule: sp("FREQ=DAILY;COUNT=5")},
			"floating": {Floating: sp(true)},
		} {
			upd, idx := upd, 1
			if end == "parent" {
				idx = 0
			}
			t.Run(end+"/"+name, func(t *testing.T) {
				t.Parallel()
				f := newLinkFix(t, 2)
				f.link(t, 0, 1, "target", "target", oneDay)
				before0, before1 := f.rowState(t, 0), f.rowState(t, 1)
				journal := f.count(t, `SELECT COUNT(*) FROM schedule_changes`)
				_, err := f.s.UpdateSchedule(f.ws.ID, f.items[idx].ID, upd)
				if !errors.Is(err, ErrScheduleLinked) {
					t.Fatalf("err=%v, want ErrScheduleLinked; child target now %s (parent %s)", err, f.role(t, 1, "target"), f.role(t, 0, "target"))
				}
				if f.rowState(t, 0) != before0 || f.rowState(t, 1) != before1 {
					t.Fatalf("a refused edit wrote: %s / %s", f.rowState(t, 0), f.rowState(t, 1))
				}
				if n := f.count(t, `SELECT COUNT(*) FROM schedule_changes`); n != journal {
					t.Fatalf("journal grew from %d to %d", journal, n)
				}
				if f.role(t, 1, "target") != "2030-03-05T09:00:00Z" {
					t.Fatalf("child target skewed: %s", f.role(t, 1, "target"))
				}
			})
		}
	}
}

func TestARoleAnEdgeUsesCannotBeChangedButOthersCan(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	same := func(extra func(*models.CommitmentRolesInput)) models.CommitmentRolesInput {
		in := models.CommitmentRolesInput{TargetDays: 0, TargetSecs: 0, Outcome: ""}
		if extra != nil {
			extra(&in)
		}
		return in
	}
	for _, i := range []int{0, 1} {
		before := f.rowState(t, i)
		_, err := f.s.SetCommitmentRoles(f.ws.ID, f.items[i].ID, same(func(in *models.CommitmentRolesInput) { in.TargetSecs = 3600 }))
		if !errors.Is(err, ErrScheduleLinked) {
			t.Fatalf("item %d: target change: %v", i, err)
		}
		if f.rowState(t, i) != before {
			t.Fatalf("item %d: a refused role edit wrote", i)
		}
		// The target is unchanged and an unused role is added: allowed.
		if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.items[i].ID, same(func(in *models.CommitmentRolesInput) {
			in.EarliestDays, in.EarliestSecs = sp(-1), sp(0)
		})); err != nil {
			t.Fatalf("item %d: unused role: %v", i, err)
		}
		// Lead and audience are not dates.
		if _, err := f.s.UpdateSchedule(f.ws.ID, f.items[i].ID, models.ScheduleUpdate{LeadMinutes: sp(15), Audience: sp("agent")}); err != nil {
			t.Fatalf("item %d: lead/audience: %v", i, err)
		}
	}
	if got := f.role(t, 1, "target"); got != "2030-03-05T09:00:00Z" {
		t.Fatalf("child target=%s", got)
	}
}

// The partial door reaches the same guard, and a role no edge uses on a
// schedule that has an edge on another role stays editable.
func TestAPartialRolesPatchOnALinkedScheduleIsGuarded(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	_, err := f.s.UpdateCommitment(f.ws.ID, f.items[1].ID, models.ScheduleUpdate{}, &models.CommitmentRolesPatch{TargetSecs: fullRolePatchValue(sp(60))})
	if !errors.Is(err, ErrScheduleLinked) {
		t.Fatalf("patch of the used role: %v", err)
	}
	if _, err := f.s.UpdateCommitment(f.ws.ID, f.items[1].ID, models.ScheduleUpdate{}, &models.CommitmentRolesPatch{
		EarliestDays: fullRolePatchValue(sp(-1)), EarliestSecs: fullRolePatchValue(sp(0))}); err != nil {
		t.Fatalf("patch of an unused role: %v", err)
	}
}

// A legacy row derives its roles from kind and dtend, so a dtend edit moves its
// occupied_until without naming a role.
func TestALegacyWindowsEndEditMovesAnOccupiedUntilEdge(t *testing.T) {
	t.Parallel()
	f := &linkFix{schedFixture: newSchedFixture(t)}
	f.items = []*models.Item{f.item, f.another(t, "child")}
	win, err := f.s.CreateSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Kind: "window", Dtstart: "2030-03-04T09:00", Dtend: "2030-03-04T10:00", Tz: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	f.scheds = []*models.ItemSchedule{win, f.native(t, f.items[1], "2030-03-06T09:00", models.CommitmentRolesInput{})}
	f.link(t, 0, 1, "occupied_until", "target", oneDay)
	before := f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND dtend='2030-03-04T10:00'`, win.ID)
	_, err = f.s.UpdateSchedule(f.ws.ID, f.item.ID, models.ScheduleUpdate{Dtend: sp("2030-03-04T11:00")})
	if !errors.Is(err, ErrScheduleLinked) {
		t.Fatalf("err=%v", err)
	}
	if f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=? AND dtend='2030-03-04T10:00'`, win.ID) != before {
		t.Fatal("the refused dtend edit wrote")
	}
}
