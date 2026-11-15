package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestBUG4570SnoozeAgenda(t *testing.T) {
	for _, action := range []string{"snooze", "move", "done", "ended"} {
		t.Run(action, func(t *testing.T) {
			f := newSchedFixture(t)
			rule := "FREQ=DAILY"
			if action == "ended" {
				rule = ""
			}
			item, _ := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: rule, LeadMinutes: 30}, "2026-10-01T00:00:00Z")
			until := rt("2026-10-01T15:00:00Z")
			state := "snooze"
			if action == "move" {
				state = "move"
			}
			f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: state, Until: &until, Actor: "user"}, "2026-10-01T10:00:00Z")
			if action == "done" || action == "ended" {
				f.mustResolve(t, item, ResolveInput{OccurrenceAt: rt("2026-10-01T09:00:00Z"), State: "done", Actor: "user", EndSchedule: action == "ended"}, "2026-10-01T11:00:00Z")
			}
			entries := f.expand(t, item, time.UTC, "2026-10-01T00:00:00Z", "2026-10-03T00:00:00Z", "2026-10-01T12:00:00Z")
			found := false
			for _, e := range entries {
				if e.State != "outstanding" {
					if e.SnoozedUntil != nil {
						t.Errorf("resolved/future entry is snoozed: %+v", e)
					}
					continue
				}
				found = true
				wantAt := rt("2026-10-01T09:00:00Z")
				if action == "move" {
					wantAt = until
				}
				if action == "done" {
					wantAt = rt("2026-10-02T09:00:00Z")
				}
				if !e.At.Equal(wantAt) {
					t.Errorf("at = %s, want %s", e.At, wantAt)
				}
				if action == "snooze" {
					if e.SnoozedUntil == nil || !e.SnoozedUntil.Equal(until) || !e.Overdue {
						t.Errorf("snoozed entry = %+v, want until %s and overdue", e, until)
					}
				} else if e.SnoozedUntil != nil {
					t.Errorf("%s entry snoozed: %+v", action, e)
				}
			}
			if found != (action != "ended") {
				t.Fatalf("outstanding found = %v after %s", found, action)
			}
		})
	}
}

func TestBUG4570LinkedMoveNotSnoozed(t *testing.T) {
	f := newLinkFix(t, 2)
	f.link(t, 0, 1, "target", "target", oneDay)
	if _, err := f.move(0, "target", "2030-03-10T09:00:00Z", nil); err != nil {
		t.Fatal(err)
	}
	entries := f.expand(t, f.items[1], time.UTC, "2030-03-10T00:00:00Z", "2030-03-12T00:00:00Z", "2030-03-09T12:00:00Z")
	if len(entries) != 1 || entries[0].State != "outstanding" || !entries[0].At.Equal(rt("2030-03-11T09:00:00Z")) || entries[0].SnoozedUntil != nil {
		t.Fatalf("linked move: %+v", entries)
	}
}
