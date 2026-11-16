package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestReminderHistoryCarrierAgenda(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, mode := range []string{"migration", "bundle"} {
			for _, kind := range []string{"acked", "unreadable"} {
				t.Run(fmt.Sprintf("pg=%t/%s/%s", pg, mode, kind), func(t *testing.T) {
					s, name, body := reminderMigrationStore(t, pg)
					ws, item, id := legacyReminderRow(t, s, "acked")
					if kind == "unreadable" {
						s.mustExec(t, `UPDATE item_reminders SET remind_at='unreadable' WHERE id=?`, id)
					}
					if mode == "migration" {
						if err := runReminderMigration(s, name, body); err != nil {
							t.Fatal(err)
						}
					} else {
						workspace, err := s.GetWorkspaceByID(ws)
						if err != nil {
							t.Fatal(err)
						}
						data, err := s.ExportWorkspace(workspace.Slug)
						if err != nil {
							t.Fatal(err)
						}
						target := past
						if kind == "unreadable" {
							target = "unreadable"
						}
						data.Reminders = []models.ReminderExport{{ItemID: item, RemindAt: target, FiredAt: past, AckedAt: past, CreatedAt: past, UpdatedAt: past}}
						imported, report, err := s.ImportWorkspaceWithReport(data, "Carrier agenda", "", "test")
						if err != nil || len(report.DroppedSchedules) != 0 {
							t.Fatalf("import: %+v %v", report, err)
						}
						ws, item = imported.ID, report.ItemIDs[item]
					}
					sc, err := s.GetSchedule(ws, item)
					if err != nil || sc == nil {
						t.Fatalf("schedule: %+v %v", sc, err)
					}
					if sc.EndedAt == nil || sc.EndedDismissedAt == nil || *sc.EndedAt != *sc.EndedDismissedAt || *sc.EndedAt != sc.CreatedAt {
						t.Errorf("carrier not dismissed at creation: %+v", sc)
					}
					var raw string
					if err := s.db.QueryRow(s.q(`SELECT after_json FROM schedule_changes WHERE schedule_id=? AND reason=?`), sc.ID, legacyReminderCarrierReason).Scan(&raw); err != nil {
						t.Fatal(err)
					}
					var snapshot map[string]any
					if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
						t.Fatal(err)
					}
					if snapshot["ended_dismissed_at"] != sc.CreatedAt || snapshot["ended_at"] != sc.CreatedAt {
						t.Errorf("dismissal not journaled: %s", raw)
					}
					scope := PendingReminderScope{ItemIDs: []string{item}, CollectionIDs: []string{}}
					work, more, err := s.ListAgendaWork(ws, scope, "", "", 100, true)
					if err != nil || more || len(work) != 1 {
						t.Fatalf("ordinary work: %+v %v %v", work, more, err)
					}
					details, gates, err := s.AgendaWorkDetails(ws, []string{item})
					if err != nil || gates[item] || details[item].EndedDismissedAt == nil {
						t.Errorf("agenda work not dismissed: %+v %v %v", details[item], gates, err)
					}
					from := time.Now().UTC().Truncate(24 * time.Hour)
					to := from.Add(24 * time.Hour)
					for _, read := range []struct {
						name string
						list func(string, PendingReminderScope, time.Time, time.Time, int) ([]AgendaSchedule, bool, error)
					}{{"agenda", s.ListSchedulesForAgenda}, {"calendar", s.ListSchedulesForCalendar}} {
						rows, more, err := read.list(ws, scope, from, to, 100)
						if err != nil || more {
							t.Fatalf("%s read: %v %v", read.name, more, err)
						}
						for _, row := range rows {
							entries, cut, unavailable, err := s.ExpandAgenda(row.Schedule, time.UTC, from, to, time.Now())
							if err != nil || cut || unavailable != "" || len(entries) != 0 {
								t.Errorf("%s carrier occurrences: %+v %v %s %v", read.name, entries, cut, unavailable, err)
							}
						}
					}
					target := from.Add(12 * time.Hour).Format(time.RFC3339)
					r, err := s.CreateReminder(ws, item, target)
					if err != nil || r.ScheduleID == nil || *r.ScheduleID != sc.ID {
						t.Fatalf("rearm: %+v %v", r, err)
					}
					sc, err = s.GetSchedule(ws, item)
					if err != nil || sc.EndedAt != nil || sc.EndedDismissedAt != nil {
						t.Fatalf("rearm ending fields: %+v %v", sc, err)
					}
					entries, cut, unavailable, err := s.ExpandAgenda(sc, time.UTC, from, to, from)
					if err != nil || cut || unavailable != "" || len(entries) != 1 || entries[0].State != "outstanding" {
						t.Fatalf("rearmed agenda: %+v %v %s %v", entries, cut, unavailable, err)
					}
					for tick := 0; tick < 2; tick++ {
						fired, err := s.FireDueReminders(to.Format(time.RFC3339), 0)
						if err != nil || len(fired) != 1-tick || (len(fired) == 1 && fired[0].ID != r.ID) {
							t.Fatalf("tick %d: %+v %v", tick, fired, err)
						}
					}
				})
			}
		}
	}
}
