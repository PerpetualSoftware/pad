package store

import (
	"testing"
	"time"
)

func TestAgendaHistoryWindowAdmission(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		shifted, override, inWindow bool
		want                        int
	}{
		{name: "old offset", shifted: true},
		{name: "old override", override: true},
		{name: "role in window", shifted: true, inWindow: true, want: 1},
		{name: "override in window", override: true, inWindow: true, want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedFixture(t)
			f.exec(t, `UPDATE collections SET schema=? WHERE id=?`, `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]}]}`, f.item.CollectionID)
			f.exec(t, `UPDATE items SET fields=? WHERE id=?`, `{"status":"done"}`, f.item.ID)
			from, to := rt("2031-03-10T00:00:00Z"), rt("2031-03-11T00:00:00Z")
			old := rt("2020-01-01T09:00:00Z")
			days := 0
			if tc.shifted {
				days = 1
				if tc.inWindow {
					days = int(from.Sub(old.Truncate(24*time.Hour)) / (24 * time.Hour))
				}
			}
			id := "history-window"
			f.exec(t, `INSERT INTO item_schedules (id,workspace_id,item_id,kind,dtstart,tz,ended_at,target_days,created_at,updated_at) VALUES (?,?,?,'check',?,'UTC',?,?,?,?)`, id, f.ws.ID, f.item.ID, old.Format("2006-01-02T15:04:05"), old.Format(time.RFC3339), days, old.Format(time.RFC3339), old.Format(time.RFC3339))
			f.exec(t, `INSERT INTO schedule_occurrences (id,schedule_id,occurrence_at,occurrence_wall,state,resolved_at) VALUES (?,?,?,?,'done',?)`, id, id, old.Format(time.RFC3339), old.Format("2006-01-02T15:04:05"), old.Format(time.RFC3339))
			if tc.override {
				at := old.Add(24 * time.Hour)
				if tc.inWindow {
					at = from.Add(9 * time.Hour)
				}
				f.exec(t, `INSERT INTO schedule_occurrence_overrides (id,schedule_id,occurrence_key,role,value_at,created_at,updated_at) VALUES (?,?,?,'target',?,?,?)`, id, id, old.Format(time.RFC3339)+"|"+old.Format("2006-01-02T15:04:05"), at.Format(time.RFC3339), old.Format(time.RFC3339), old.Format(time.RFC3339))
			}
			rows, more, err := f.s.ListSchedulesForAgenda(f.ws.ID, PendingReminderScope{}, from, to, 1)
			if err != nil || more || len(rows) != tc.want {
				t.Fatalf("history admission rows=%d want=%d more=%v err=%v", len(rows), tc.want, more, err)
			}
		})
	}
}
