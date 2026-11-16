package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-4612: a forecast or an active milestone selection lives beside the
// schedule and cascades away with it, so a commitment that has either is not a
// simple reminder. The legacy alias operations refuse it and touch nothing.

// milestoneShape gives the one-shot commitment on f.item a forecast, an active
// selection (child or waiver), or a selection that was added and then removed.
func milestoneShape(t *testing.T, f *schedFixture, shape string) {
	t.Helper()
	switch shape {
	case "forecast":
		if err := f.s.SetForecast(f.ws.ID, f.item.ID, "u1", "user", "2099-02-01", "estimate"); err != nil {
			t.Fatal(err)
		}
	case "child", "waiver", "removed":
		kind, reason := "child", ""
		if shape == "waiver" {
			kind, reason = "waiver", "out of scope"
		}
		child := f.another(t, "Child "+shape)
		if err := f.s.SetMilestoneChild(f.ws.ID, f.item.ID, child.ID, kind, reason, "u1"); err != nil {
			t.Fatal(err)
		}
		if shape == "removed" {
			if err := f.s.RemoveMilestoneChild(f.ws.ID, f.item.ID, child.ID, "u1"); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unknown shape %q", shape)
	}
}

// milestoneRows counts the rows a cascading schedule delete would take.
func milestoneRows(t *testing.T, f *schedFixture, scheduleID string) (schedules, forecasts, active int) {
	t.Helper()
	return f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=?`, scheduleID),
		f.count(t, `SELECT COUNT(*) FROM schedule_forecasts WHERE schedule_id=?`, scheduleID),
		f.count(t, `SELECT COUNT(*) FROM milestone_children WHERE schedule_id=? AND removed_at IS NULL`, scheduleID)
}

func TestReminderAliasRefusesMilestoneCommitment(t *testing.T) {
	ops := map[string]func(f *schedFixture, id string) error{
		"rearm": func(f *schedFixture, id string) error {
			_, err := f.s.RearmReminder(f.ws.ID, id, "2099-03-01T00:00:00Z")
			return err
		},
		"ack": func(f *schedFixture, id string) error {
			_, err := f.s.AckReminder(f.ws.ID, id)
			return err
		},
		"delete": func(f *schedFixture, id string) error {
			ok, err := f.s.DeleteReminder(f.ws.ID, id)
			if ok && err == nil {
				return errors.New("deleted")
			}
			return err
		},
	}
	for name, op := range ops {
		for _, shape := range []string{"forecast", "child", "waiver"} {
			t.Run(name+"/"+shape, func(t *testing.T) {
				f := newSchedFixture(t)
				id := armReminder(t, f.s, f.ws.ID, f.item.ID, future)
				milestoneShape(t, f, shape)
				f.fire(t, "2099-01-01T00:00:01Z")
				sc := f.reload(t, f.item.ID)
				beforeS, beforeF, beforeC := milestoneRows(t, f, sc.ID)
				if err := op(f, id); !errors.Is(err, ErrReminderManaged) {
					t.Fatalf("%s: %v, want ErrReminderManaged", name, err)
				}
				s, fc, c := milestoneRows(t, f, sc.ID)
				if s != 1 || s != beforeS || fc != beforeF || c != beforeC {
					t.Fatalf("refusal changed rows: schedule %d->%d forecast %d->%d active %d->%d", beforeS, s, beforeF, fc, beforeC, c)
				}
				if shape == "forecast" && fc != 1 {
					t.Fatalf("forecast rows=%d", fc)
				}
				if shape != "forecast" && c != 1 {
					t.Fatalf("active selection rows=%d", c)
				}
				after := f.reload(t, f.item.ID)
				if after.ID != sc.ID || derefStr(after.NextAt) != derefStr(sc.NextAt) || derefStr(after.ReminderAckedAt) != "" {
					t.Fatalf("refusal changed the commitment: %+v -> %+v", sc, after)
				}
				if err := f.s.ReminderCompatibility(f.ws.ID, f.item.ID); !errors.Is(err, ErrReminderManaged) {
					t.Fatalf("compatibility: %v, want ErrReminderManaged", err)
				}
			})
		}
	}
}

// A selection that was removed is history only; the commitment is simple again.
func TestReminderAliasRemovedSelectionStaysSimple(t *testing.T) {
	f := newSchedFixture(t)
	id := armReminder(t, f.s, f.ws.ID, f.item.ID, future)
	sc := f.reload(t, f.item.ID)
	milestoneShape(t, f, "removed")
	if f.count(t, `SELECT COUNT(*) FROM milestone_children WHERE schedule_id=? AND removed_at IS NOT NULL`, sc.ID) != 1 {
		t.Fatal("removed selection row missing")
	}
	if err := f.s.ReminderCompatibility(f.ws.ID, f.item.ID); err != nil {
		t.Fatalf("compatibility: %v", err)
	}
	f.fire(t, "2099-01-01T00:00:01Z")
	if r, err := f.s.AckReminder(f.ws.ID, id); err != nil || r == nil {
		t.Fatalf("ack: %+v %v", r, err)
	}
	if r, err := f.s.RearmReminder(f.ws.ID, id, "2099-03-01T00:00:00Z"); err != nil || r == nil || !r.Armed() {
		t.Fatalf("rearm: %+v %v", r, err)
	}
	if ok, err := f.s.DeleteReminder(f.ws.ID, id); err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if f.count(t, `SELECT COUNT(*) FROM item_schedules WHERE id=?`, sc.ID) != 0 {
		t.Fatal("delete left the commitment")
	}
}

// A legacy create on an ended carrier with a forecast or selection must not
// retarget it: the milestone is changed only through the commitment surface.
func TestEndedReminderCarrierWithMilestoneNotReused(t *testing.T) {
	for _, shape := range []string{"none", "forecast", "child", "removed"} {
		t.Run(shape, func(t *testing.T) {
			f := newSchedFixture(t)
			sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{})
			if err != nil {
				t.Fatal(err)
			}
			f.exec(t, `DELETE FROM item_reminders WHERE schedule_id=?`, sc.ID)
			f.exec(t, `UPDATE item_schedules SET next_at=NULL,next_wall=NULL,ended_at=? WHERE id=?`, past, sc.ID)
			if shape != "none" {
				milestoneShape(t, f, shape)
			}
			beforeS, beforeF, beforeC := milestoneRows(t, f, sc.ID)
			r, err := f.s.CreateReminder(f.ws.ID, f.item.ID, "2099-03-01T00:00:00Z")
			if shape == "none" || shape == "removed" {
				if err != nil || r == nil || r.ScheduleID == nil || *r.ScheduleID != sc.ID {
					t.Fatalf("reuse: %+v %v", r, err)
				}
				return
			}
			if !errors.Is(err, ErrScheduleExists) {
				t.Fatalf("create: %+v %v, want ErrScheduleExists", r, err)
			}
			s, fc, c := milestoneRows(t, f, sc.ID)
			if s != 1 || fc != beforeF || c != beforeC || beforeS != 1 || fc+c != 1 {
				t.Fatalf("refusal changed rows: schedule %d->%d forecast %d->%d active %d->%d", beforeS, s, beforeF, fc, beforeC, c)
			}
			if after := f.reload(t, f.item.ID); after.EndedAt == nil || after.ReminderID != nil {
				t.Fatalf("refusal revived the carrier: %+v", after)
			}
		})
	}
}
