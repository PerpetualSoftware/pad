package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
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

// Admission of a terminal item's history judges each stamped row by the
// offsets it was resolved under, and an unstamped one by the current offsets
// (TASK-4714).
func TestAgendaHistoryWindowAdmissionUsesSnapshot(t *testing.T) {
	t.Parallel()
	f := newLinkFix(t, 2)
	// Item 0 is resolved with its target ten days after the seed and then put
	// back on the seed; item 1 is resolved on its seed and then moved ten days out.
	for i, days := range []int{10, 0} {
		f.exec(t, `DELETE FROM item_schedules WHERE id=?`, f.scheds[i].ID)
		sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.items[i].ID, models.ScheduleCreate{Kind: "check", Dtstart: "2030-03-04T09:00", Tz: "UTC", Rrule: "FREQ=DAILY"}, models.CommitmentRolesInput{TargetDays: days})
		if err != nil {
			t.Fatal(err)
		}
		f.scheds[i] = sc
		f.resolve(t, i, "done")
		if _, err := f.s.SetCommitmentRoles(f.ws.ID, f.items[i].ID, models.CommitmentRolesInput{TargetDays: 10 - days}); err != nil {
			t.Fatal(err)
		}
		f.setStatus(t, i, "done")
	}
	admitted := func(from, to string) map[string]bool {
		got, _, err := f.s.ListSchedulesForCalendar(f.ws.ID, PendingReminderScope{}, rt(from), rt(to), 100)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, a := range got {
			out[a.Schedule.ID] = true
		}
		return out
	}
	got := admitted("2030-03-14T00:00:00Z", "2030-03-15T00:00:00Z")
	if !got[f.scheds[0].ID] {
		t.Fatal("a row whose resolved target is in the window was not admitted")
	}
	if got[f.scheds[1].ID] {
		t.Fatal("a row was admitted by the current offset, not the one it was resolved under")
	}
	// Both rows sit on their seed day, which still admits them by instant.
	got = admitted("2030-03-04T00:00:00Z", "2030-03-05T00:00:00Z")
	if !got[f.scheds[0].ID] || !got[f.scheds[1].ID] {
		t.Fatalf("seed day admission = %v", got)
	}
	// An unstamped row falls back to the current offsets.
	f.exec(t, `UPDATE schedule_occurrences SET roles_target_days=NULL, roles_target_secs=NULL WHERE schedule_id IN (?, ?)`, f.scheds[0].ID, f.scheds[1].ID)
	got = admitted("2030-03-14T00:00:00Z", "2030-03-15T00:00:00Z")
	if got[f.scheds[0].ID] || !got[f.scheds[1].ID] {
		t.Fatalf("unstamped admission = %v, want the current offsets", got)
	}
}
