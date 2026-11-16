package store

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const reminderHistoryReason = "one-shot reminder kept as history"

func TestReminderCollisionPolicy(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, mode := range []string{"migration", "bundle"} {
			for _, shape := range []string{"acked_fired", "armed_fired", "two_armed", "all_acked", "created_tie", "identity_tie", "offset_order", "active", "acknowledged", "ended"} {
				t.Run(fmt.Sprintf("pg=%t/%s/%s", pg, mode, shape), func(t *testing.T) {
					s, name, body := reminderMigrationStore(t, pg)
					ws, item, firstID := legacyReminderRow(t, s, "armed")
					first := models.Reminder{ID: firstID, WorkspaceID: ws, ItemID: item, RemindAt: "2020-01-01T00:00:00.1200+01:00", CreatedAt: "2019-01-01T01:02:03.4500+01:00", UpdatedAt: "2020-02-01T01:02:03.600+01:00"}
					second := first
					second.ID = newID()
					second.RemindAt = "2020-01-02T00:00:00Z"
					fired, acked := "2020-01-03T01:02:03.4500+01:00", "2020-01-04T01:02:03.600+01:00"
					winner := 1
					switch shape {
					case "acked_fired":
						first.FiredAt, first.AckedAt, second.FiredAt = &fired, &acked, &fired
					case "armed_fired":
						second.FiredAt = &fired
						winner = 0
					case "all_acked":
						first.FiredAt, first.AckedAt, second.FiredAt, second.AckedAt = &fired, &acked, &fired, &acked
					case "created_tie":
						second.RemindAt = first.RemindAt
						second.CreatedAt = "2019-01-02T00:00:00Z"
					case "identity_tie":
						second.RemindAt = first.RemindAt
						// Deliberately put the greater ID first: bundles must use position instead.
						if first.ID < second.ID {
							first.ID, second.ID = second.ID, first.ID
						}
						if mode == "migration" {
							winner = 0
						}
					case "offset_order":
						first.RemindAt = "2020-01-01T00:00:00.900Z"
						second.RemindAt = "2020-01-01T01:00:00.100+01:00"
						winner = 0
					case "active", "acknowledged", "ended":
						winner = -1
					}
					var existing *models.ItemSchedule
					if winner == -1 {
						var err error
						existing, err = s.CreateCommitmentSchedule(ws, item, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{})
						if err != nil {
							t.Fatal(err)
						}
						if shape == "acknowledged" {
							s.mustExec(t, `UPDATE item_reminders SET fired_at=?,acked_at=? WHERE schedule_id=?`, fired, acked, existing.ID)
						}
						if shape == "ended" {
							s.mustExec(t, `DELETE FROM item_reminders WHERE schedule_id=?`, existing.ID)
							s.mustExec(t, `UPDATE item_schedules SET next_at=NULL,next_wall=NULL,ended_at=? WHERE id=?`, past, existing.ID)
						}
						s.mustExec(t, `INSERT INTO schedule_occurrences(id,schedule_id,occurrence_at,occurrence_wall,state,resolved_at) VALUES(?,?,?,'2019-01-01T00:00:00','skipped',?)`, newID(), existing.ID, "2019-01-01T00:00:00Z", past)
					}
					s.mustExec(t, `DELETE FROM item_reminders WHERE schedule_id IS NULL`)
					legacy := []models.Reminder{first, second}
					for _, r := range legacy {
						s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,fired_at,acked_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, r.ID, ws, item, r.RemindAt, r.FiredAt, r.AckedAt, r.CreatedAt, r.UpdatedAt)
					}
					workspace, err := s.GetWorkspaceByID(ws)
					if err != nil {
						t.Fatal(err)
					}
					data, err := s.ExportWorkspace(workspace.Slug)
					if err != nil {
						t.Fatal(err)
					}
					// Keep a conflict-free item in the same population to verify accounting.
					col := createTestCollection(t, s, ws, "Other")
					single := createTestItem(t, s, ws, col.ID, "Single", "open")
					s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at) VALUES(?,?,?,?,?,?)`, newID(), ws, single.ID, future, past, past)
					scheduleBefore := renegotiateRows(t, s, "item_schedules", "")
					alarmBefore := renegotiateRows(t, s, "item_reminders", "WHERE schedule_id IS NOT NULL")
					occurrencesBefore := renegotiateRows(t, s, "schedule_occurrences", "")
					targetWS, targetItem := ws, item
					if mode == "migration" {
						if err := runReminderMigration(s, name, body); err != nil {
							t.Fatal(err)
						}
						if s.countRows(t, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, name) != 1 {
							t.Fatal("migration version missing")
						}
						if s.countRows(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id IS NULL`) != 0 {
							t.Fatal("bare rows remain")
						}
						histories := 1
						if winner == -1 {
							histories = 2
						}
						if s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE reason=?`, reminderHistoryReason) != histories || s.countRows(t, `SELECT COUNT(*) FROM item_reminders WHERE item_id IN (?,?)`, item, single.ID) != 3-histories+boolInt(existing != nil && shape != "ended") {
							t.Fatal("conversion accounting mismatch")
						}
						if winner == -1 && (scheduleBefore != renegotiateRows(t, s, "item_schedules", "WHERE item_id<>?", single.ID) || alarmBefore != renegotiateRows(t, s, "item_reminders", "WHERE item_id=?", item) || occurrencesBefore != renegotiateRows(t, s, "schedule_occurrences", "")) {
							t.Fatal("existing schedule, alarm or occurrences changed")
						}
					} else {
						data.Reminders = nil
						for _, r := range legacy {
							data.Reminders = append(data.Reminders, models.ReminderExport{ItemID: item, RemindAt: r.RemindAt, FiredAt: derefStr(r.FiredAt), AckedAt: derefStr(r.AckedAt), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt})
						}
						inputBefore, _ := json.Marshal(data)
						converted, err := convertLegacyReminderBundle(data)
						if err != nil {
							t.Fatal(err)
						}
						inputAfter, _ := json.Marshal(data)
						if string(inputBefore) != string(inputAfter) {
							t.Fatal("conversion mutated input bundle")
						}
						if winner == -1 && !reflect.DeepEqual(data.Schedules, converted.Schedules) {
							t.Fatal("existing bundle schedule changed")
						}
						// The source rows only construct the legacy bundle; live firing uses
						// the imported workspace, where every alarm has a schedule.
						s.mustExec(t, `DELETE FROM item_reminders WHERE schedule_id IS NULL`)
						imported, report, err := s.ImportWorkspaceWithReport(data, "Collision import", "", "test")
						if err != nil {
							t.Fatal(err)
						}
						if len(report.DroppedSchedules) != 0 {
							t.Fatalf("dropped history or schedule: %+v", report)
						}
						targetWS = imported.ID
						out, err := s.ExportWorkspace(imported.Slug)
						if err != nil || len(out.Schedules) != 1 || len(out.Reminders) != 0 {
							t.Fatalf("imported schedules: %+v %v", out, err)
						}
						targetItem = out.Schedules[0].ItemID
					}
					sc, err := s.GetSchedule(targetWS, targetItem)
					if err != nil || sc == nil {
						t.Fatalf("schedule: %+v %v", sc, err)
					}
					if s.countRows(t, `SELECT COUNT(*) FROM item_schedules WHERE item_id=?`, targetItem) != 1 {
						t.Fatal("second schedule")
					}
					if winner >= 0 {
						r, err := s.GetReminder(targetWS, *sc.ReminderID)
						want := legacy[winner]
						// The existing bundle carry path normalizes lifecycle stamps. History
						// retains the original bytes independently of that winner conversion.
						if mode == "bundle" {
							if want.FiredAt != nil {
								at, _ := normalizeRemindAt(*want.FiredAt)
								want.FiredAt = &at
							}
							if want.AckedAt != nil {
								at, _ := normalizeRemindAt(*want.AckedAt)
								want.AckedAt = &at
							}
						}
						at, _ := normalizeRemindAt(want.RemindAt)
						if err != nil || r.RemindAt != at || derefStr(r.FiredAt) != derefStr(want.FiredAt) || derefStr(r.AckedAt) != derefStr(want.AckedAt) || (mode == "migration" && r.ID != want.ID) {
							t.Fatalf("wrong winner: %+v %v", r, err)
						}
						if want.AckedAt != nil && derefStr(sc.ReminderAckedAt) != *want.AckedAt {
							t.Fatal("winner not acknowledged")
						}
					}
					changes, err := s.scheduleChangesQ(s.db, targetWS, "")
					if err != nil {
						t.Fatal(err)
					}
					var history []models.ScheduleChangeExport
					for _, c := range changes {
						if c.Reason == reminderHistoryReason {
							history = append(history, c)
						}
					}
					wantHistory := len(legacy)
					if winner >= 0 {
						wantHistory--
					}
					if len(history) != wantHistory {
						t.Fatalf("history=%d want=%d", len(history), wantHistory)
					}
					for i, r := range legacy {
						if i == winner {
							continue
						}
						want := map[string]any{"item_id": item, "remind_at": r.RemindAt, "fired_at": r.FiredAt, "acked_at": r.AckedAt, "created_at": r.CreatedAt, "updated_at": r.UpdatedAt}
						if mode == "migration" {
							want["id"], want["workspace_id"] = r.ID, ws
						} else {
							want["fired_at"], want["acked_at"], want["bundle_index"] = derefStr(r.FiredAt), derefStr(r.AckedAt), i
						}
						raw, _ := json.Marshal(want)
						var expected map[string]any
						json.Unmarshal(raw, &expected)
						found := false
						for _, c := range history {
							var got map[string]any
							if c.BeforeJSON == nil || json.Unmarshal([]byte(*c.BeforeJSON), &got) != nil {
								t.Fatal("invalid history")
							}
							if reflect.DeepEqual(got, expected) {
								found = true
								if c.ScheduleID != sc.ID || c.ItemID != targetItem || c.ChangeType != "import" || c.Actor != "" || c.ReasonSource != "system" || (c.AfterJSON != nil && *c.AfterJSON != "{}") {
									t.Fatalf("bad history metadata: %+v", c)
								}
							}
						}
						if !found {
							t.Fatalf("missing exact history %s: %+v", raw, history)
						}
					}
					if mode == "migration" && shape == "acked_fired" {
						out, err := s.ExportWorkspace(workspace.Slug)
						if err != nil {
							t.Fatal(err)
						}
						copied, report, err := s.ImportWorkspaceWithReport(out, "History roundtrip", "", "test")
						if err != nil || len(report.DroppedSchedules) != 0 {
							t.Fatalf("roundtrip: %+v %v", report, err)
						}
						carried, err := s.scheduleChangesQ(s.db, copied.ID, "")
						if err != nil {
							t.Fatal(err)
						}
						found := false
						for _, c := range carried {
							if c.Reason == reminderHistoryReason && derefStr(c.BeforeJSON) == *history[0].BeforeJSON {
								found = true
							}
						}
						if !found {
							t.Fatal("history bytes lost on export/import")
						}
					}
					for tick := 0; tick < 2; tick++ {
						rows, err := s.FireDueReminders("2026-01-01T00:00:00Z", 0)
						if err != nil {
							t.Fatal(err)
						}
						var got []*models.Reminder
						for _, r := range rows {
							if r.ItemID == targetItem {
								got = append(got, r)
							}
						}
						want := 0
						if tick == 0 && winner >= 0 && legacy[winner].FiredAt == nil {
							want = 1
						}
						if len(got) != want || (len(got) == 1 && got[0].ID != *sc.ReminderID) {
							t.Fatalf("tick %d: %+v want %d", tick, got, want)
						}
					}
				})
			}
		}
	}
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestLegacyReminderBundlePreflightRollback(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprint(pg), func(t *testing.T) {
			s, _, _ := reminderMigrationStore(t, pg)
			ws, _, _ := legacyReminderRow(t, s, "armed")
			workspace, err := s.GetWorkspaceByID(ws)
			if err != nil {
				t.Fatal(err)
			}
			data, err := s.ExportWorkspace(workspace.Slug)
			if err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []string{"orphan"} {
				t.Run(invalid, func(t *testing.T) {
					good := models.ReminderExport{ItemID: data.Items[0].ID, RemindAt: past, CreatedAt: past, UpdatedAt: past}
					bad := good
					bad.ItemID = "missing"
					data.Reminders = []models.ReminderExport{good, bad}
					before := renegotiateRows(t, s, "workspaces", "") + renegotiateRows(t, s, "item_schedules", "") + renegotiateRows(t, s, "schedule_changes", "")
					if _, err := s.ImportWorkspace(data, "Invalid legacy", "", "test"); err == nil {
						t.Fatal("invalid reminder accepted")
					}
					after := renegotiateRows(t, s, "workspaces", "") + renegotiateRows(t, s, "item_schedules", "") + renegotiateRows(t, s, "schedule_changes", "")
					if before != after {
						t.Fatal("preflight failure changed database")
					}
				})
			}
		})
	}
}
