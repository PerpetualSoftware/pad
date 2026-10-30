package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/kernelevents"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-4103 C2b-1 and C2b-2: the frozen instant of an overridden floating
// fire, and the fire stamp that keys the notification.

func (f *schedFixture) notifications(t *testing.T, userID string) []string {
	t.Helper()
	rows, err := f.s.db.Query(f.s.q(`SELECT dedupe_key FROM user_notifications WHERE user_id = ? AND kind = 'reminder' ORDER BY dedupe_key`), userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	return keys
}

// A snoozed or moved floating occurrence is frozen at the instant the caller
// saw (next_at), so a recipient zone change before it re-fires cannot move it:
// occurrence_at, the outbox payload and the dedupe key carry the frozen instant.
// RED when the fire re-resolves the wall in the recipient's zone (C2b-1).
func TestFloatingOverrideFireKeepsTheFrozenInstantAfterAZoneChange(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "c2b1@example.com", "America/Los_Angeles")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-11-02T09:00", Floating: true, Rrule: "FREQ=DAILY"})
	// What a snooze leaves: the occurrence frozen at LA 09:00, an override two
	// days on, the row armed at the override.
	f.exec(t, `UPDATE item_schedules SET next_at = ?, next_override_at = ? WHERE id = ?`, "2026-11-02T17:00:00Z", "2026-11-04T20:00:00Z", sc.ID)
	f.exec(t, `UPDATE item_reminders SET remind_at = ? WHERE schedule_id = ?`, "2026-11-04T20:00:00Z", sc.ID)
	f.exec(t, `UPDATE users SET timezone = ? WHERE id = ?`, "Asia/Tokyo", u.ID)

	got := f.fire(t, "2026-11-04T20:00:00Z")
	if len(got) != 1 || got[0].OccurrenceAt == nil || *got[0].OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Fatalf("fired %+v, want occurrence_at 2026-11-02T17:00:00Z (Tokyo would read 2026-11-02T00:00:00Z)", got)
	}
	var payload string
	if err := f.s.db.QueryRow(f.s.q(`SELECT payload FROM event_outbox WHERE event_type = ?`), kernelevents.ItemReminderDue).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var pl struct {
		Reminder struct {
			OccurrenceAt string `json:"occurrence_at"`
			FireStamp    string `json:"fire_stamp"`
		} `json:"reminder"`
	}
	if err := json.Unmarshal([]byte(payload), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Reminder.OccurrenceAt != "2026-11-02T17:00:00Z" {
		t.Errorf("payload occurrence_at = %q", pl.Reminder.OccurrenceAt)
	}
	want := "reminder:" + *sc.ReminderID + ":2026-11-02T17:00:00Z:" + pl.Reminder.FireStamp
	if keys := f.notifications(t, u.ID); len(keys) != 1 || keys[0] != want {
		t.Errorf("dedupe keys = %v, want [%s]", keys, want)
	}
	if cur := f.reload(t, f.item.ID); cur.NextAt == nil || *cur.NextAt != "2026-11-02T17:00:00Z" {
		t.Errorf("next_at = %v", deref(cur.NextAt))
	}
}

// Two fires of one row inside one second are two notifications, managed and
// unmanaged, and a redelivery of ONE fire is a single notification (C2b-2).
// RED with fired_at (second resolution) in the key.
func TestReminderDedupeKeyCarriesTheFireStamp(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	u := f.user(t, "c2b2@example.com", "")
	addDevice(t, f.s, u.ID)
	f.assign(t, f.item.ID, u.ID)
	const nowTS = "2026-10-29T12:00:00Z" // both fires of each pair take this same pass clock

	sc := f.create(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-01T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"})
	f.force(t, sc, "2026-10-28T09:00:00", "2026-10-28T09:00:00Z", "2026-10-28T09:00:00Z")
	first := f.fire(t, nowTS)
	if len(first) != 1 || first[0].FireStamp == nil {
		t.Fatalf("first fire %+v", first)
	}
	// Re-armed for the SAME occurrence (what a snooze does), fired in the same second.
	f.exec(t, `UPDATE item_reminders SET fired_at = NULL, acked_at = NULL, remind_at = ? WHERE id = ?`, "2026-10-28T09:00:00Z", *sc.ReminderID)
	second := f.fire(t, nowTS)
	if len(second) != 1 || second[0].FireStamp == nil || *second[0].FireStamp == *first[0].FireStamp {
		t.Fatalf("second fire %+v shares the first's stamp", second)
	}
	if first[0].FiredAt == nil || second[0].FiredAt == nil || *first[0].FiredAt != *second[0].FiredAt {
		t.Fatalf("setup: the two fires must share fired_at, got %v and %v", deref(first[0].FiredAt), deref(second[0].FiredAt))
	}
	if keys := f.notifications(t, u.ID); len(keys) != 2 {
		t.Errorf("managed re-fire in the same second: %d notifications %v, want 2", len(keys), keys)
	}

	// Unmanaged: the same pair on an ordinary reminder.
	other := f.another(t, "Plain")
	f.assign(t, other.ID, u.ID)
	r, err := f.s.CreateReminder(f.ws.ID, other.ID, "2026-10-28T09:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	f.fire(t, nowTS)
	f.exec(t, `UPDATE item_reminders SET fired_at = NULL, acked_at = NULL WHERE id = ?`, r.ID)
	f.fire(t, nowTS)
	n := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND item_id = ? AND kind = 'reminder'`, u.ID, other.ID)
	if n != 2 {
		t.Errorf("unmanaged re-fire in the same second: %d notifications, want 2", n)
	}

	// Redelivery: the notification enqueued again from the SAME fired snapshot
	// (the payload carries its stamp) is the same notification.
	before := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND kind = 'reminder'`, u.ID)
	for _, snap := range []*models.Reminder{first[0], second[0]} {
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		it, err := f.s.GetItemQ(tx, snap.ItemID)
		if err != nil || it == nil {
			t.Fatal(err)
		}
		if err := f.s.enqueueReminderNotificationTx(tx, snap, it); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if after := f.count(t, `SELECT COUNT(*) FROM user_notifications WHERE user_id = ? AND kind = 'reminder'`, u.ID); after != before {
		t.Errorf("a redelivered fire wrote %d more notifications", after-before)
	}
}
