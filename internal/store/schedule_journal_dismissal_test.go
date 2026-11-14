package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestScheduleJournalEndedDismissal(t *testing.T) {
	t.Parallel()
	f := newSchedFixture(t)
	item, sc := f.mk(t, models.ScheduleCreate{Kind: "check", Dtstart: "2026-10-30T09:00", Tz: "UTC"}, "2026-10-29T12:00:00Z")
	f.exec(t, `UPDATE collections SET schema='{"fields":[]}' WHERE id=?`, item.CollectionID)
	f.exec(t, `UPDATE item_reminders SET fired_at=?, acked_at=? WHERE schedule_id=?`, "2026-10-30T09:00:00Z", "2026-10-30T09:01:00Z", sc.ID)
	before := f.reload(t, item.ID)
	f.mustResolve(t, item, ResolveInput{OccurrenceAt: f.outstanding(t, item), State: "done", Actor: "user"}, "2026-10-30T09:05:00Z")
	ended := f.reload(t, item.ID)
	if ended.EndedDismissedAt == nil {
		t.Fatal("ending was not dismissed")
	}
	check := func(change string, wantBefore, wantAfter *models.ItemSchedule) {
		t.Helper()
		var beforeJSON, afterJSON string
		if err := f.s.db.QueryRow(f.s.q(`SELECT before_json, after_json FROM schedule_changes WHERE schedule_id=? AND change_type=? ORDER BY recorded_at DESC LIMIT 1`), sc.ID, change).Scan(&beforeJSON, &afterJSON); err != nil {
			t.Fatal(err)
		}
		for _, snapshot := range []struct {
			name, raw string
			want      *models.ItemSchedule
		}{{"before", beforeJSON, wantBefore}, {"after", afterJSON, wantAfter}} {
			var got map[string]any
			if err := json.Unmarshal([]byte(snapshot.raw), &got); err != nil {
				t.Fatal(err)
			}
			for key, value := range map[string]*string{"ended_at": snapshot.want.EndedAt, "ended_dismissed_at": snapshot.want.EndedDismissedAt} {
				var want any
				if value != nil {
					want = *value
				}
				if v, exists := got[key]; !exists || v != want {
					t.Errorf("%s %s %s = %v (present %v), want %v", change, snapshot.name, key, v, exists, want)
				}
			}
			if change == "done" && snapshot.name == "before" {
				if got["reminder_remind_at"] == nil || got["reminder_fired_at"] != "2026-10-30T09:00:00Z" || got["reminder_acked_at"] != "2026-10-30T09:01:00Z" {
					t.Errorf("reminder state missing: %s", snapshot.raw)
				}
			}
			if snapshot.want.EndedAt != nil && got["reminder_remind_at"] != nil {
				t.Errorf("ended snapshot kept a reminder: %s", snapshot.raw)
			}
		}
	}
	check("done", before, ended)
	start := "2026-11-05T09:00"
	revived, err := f.s.updateScheduleAt(f.ws.ID, item.ID, models.ScheduleUpdate{Dtstart: &start}, rt("2026-10-30T10:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if revived.EndedAt != nil || revived.EndedDismissedAt != nil {
		t.Fatalf("schedule did not revive: %+v", revived)
	}
	check("update", ended, revived)
}
