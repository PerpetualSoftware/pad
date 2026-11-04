package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4238 (calendar P1b): a schedule's audience routes the push. A fire of a
// managed (schedule) reminder pushes to its recipient only when the audience is
// human or both; an agent-only schedule writes the outbox event (webhooks and
// the agenda are unchanged) and no notification row. Runs on SQLite in make test
// and on Postgres in make test-pg.

// audienceFixture is a schedule whose item is assigned to a user with a push
// device, so a notification row is written whenever the fire is allowed to push.
func audienceFixture(t *testing.T, audience string) (*schedFixture, *models.User, *models.ItemSchedule) {
	t.Helper()
	f := newSchedFixture(t)
	u := f.user(t, "audience@example.com", "UTC")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY", Audience: audience})
	return f, u, sc
}

func TestAudienceRouting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		audience string
		pushes   int
	}{
		{"human", 1},
		{"agent", 0},
		{"both", 1},
	} {
		tc := tc
		t.Run(tc.audience, func(t *testing.T) {
			t.Parallel()
			f, u, sc := audienceFixture(t, tc.audience)
			f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
			if got := f.fire(t, "2026-10-26T09:00:05Z"); len(got) != 1 {
				t.Fatalf("fired %d, want 1", len(got))
			}
			if got := len(f.notifications(t, u.ID)); got != tc.pushes {
				t.Errorf("audience %s: %d notification rows, want %d", tc.audience, got, tc.pushes)
			}
			if got := f.events(t); got != 1 {
				t.Errorf("audience %s: %d outbox events, want 1 (the event is written for every audience)", tc.audience, got)
			}
		})
	}
}

// A one-shot (unmanaged) reminder is unchanged: it pushes to its recipient.
func TestOneShotReminderStillPushes(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "oneshot@example.com", "UTC")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	if _, err := f.s.CreateReminder(f.ws.ID, f.item.ID, "2026-10-26T09:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if got := f.fire(t, "2026-10-26T09:00:05Z"); len(got) != 1 {
		t.Fatalf("fired %d, want 1", len(got))
	}
	if got := len(f.notifications(t, u.ID)); got != 1 {
		t.Errorf("one-shot reminder: %d notification rows, want 1", got)
	}
}

// Changing the audience applies from the next fire: the first fire follows the
// audience it armed under, the second the audience read at its own fire.
func TestAudienceChangeAppliesFromTheNextFire(t *testing.T) {
	t.Parallel()
	f, u, sc := audienceFixture(t, "agent")
	f.force(t, sc, "2026-10-26T09:00:00", "2026-10-26T09:00:00Z", "2026-10-26T09:00:00Z")
	f.fire(t, "2026-10-26T09:00:05Z")
	if got := len(f.notifications(t, u.ID)); got != 0 {
		t.Fatalf("first fire (agent): %d notification rows, want 0", got)
	}

	human := "human"
	if _, err := f.s.UpdateSchedule(f.ws.ID, f.item.ID, models.ScheduleUpdate{Audience: &human}); err != nil {
		t.Fatal(err)
	}
	// A fired managed row stays fired until its occurrence is resolved; re-arm it for the next occurrence as a
	// resolve does, so the second fire is a real fire of the same schedule.
	f.force(t, f.reload(t, f.item.ID), "2026-10-27T09:00:00", "2026-10-27T09:00:00Z", "2026-10-27T09:00:00Z")
	f.exec(t, `UPDATE item_reminders SET fired_at = NULL WHERE schedule_id = ?`, sc.ID)
	if got := f.fire(t, "2026-10-27T09:00:05Z"); len(got) != 1 {
		t.Fatalf("second fire: fired %d, want 1", len(got))
	}
	if got := len(f.notifications(t, u.ID)); got != 1 {
		t.Errorf("second fire (human, after the change): %d notification rows, want 1", got)
	}
	if got := f.events(t); got != 2 {
		t.Errorf("%d outbox events, want 2 (one per fire)", got)
	}
}
