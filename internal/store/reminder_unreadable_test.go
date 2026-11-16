package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestUnreadableReminderHistory(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, mode := range []string{"migration", "bundle"} {
			for _, shape := range []string{"alone", "readable", "existing"} {
				for _, field := range []string{"remind_at", "created_at", "updated_at", "fired_at", "acked_at", "state", "target"} {
					t.Run(fmt.Sprintf("pg=%t/%s/%s/%s", pg, mode, shape, field), func(t *testing.T) {
						s, name, body := reminderMigrationStore(t, pg)
						ws, item, id := legacyReminderRow(t, s, "armed")
						bad := models.Reminder{ID: id, WorkspaceID: ws, ItemID: item, RemindAt: past, CreatedAt: past, UpdatedAt: past}
						reasonField := field
						switch field {
						case "remind_at":
							bad.RemindAt = "bad target bytes"
						case "created_at":
							bad.CreatedAt = "bad creation bytes"
						case "updated_at":
							bad.UpdatedAt = "bad update bytes"
						case "fired_at":
							bad.FiredAt = ptr("bad fire bytes")
						case "acked_at":
							bad.FiredAt, bad.AckedAt = ptr(past), ptr("bad ack bytes")
						case "state":
							bad.AckedAt, reasonField = ptr(past), "acked_at"
						case "target":
							bad.RemindAt, reasonField = "9999-12-31T23:59:59.9Z", "remind_at"
						}
						s.mustExec(t, `UPDATE item_reminders SET remind_at=?,created_at=?,updated_at=?,fired_at=?,acked_at=? WHERE id=?`, bad.RemindAt, bad.CreatedAt, bad.UpdatedAt, bad.FiredAt, bad.AckedAt, id)
						legacy := []models.Reminder{bad}
						var existing *models.ItemSchedule
						if shape == "readable" {
							good := models.Reminder{ID: newID(), WorkspaceID: ws, ItemID: item, RemindAt: past, CreatedAt: past, UpdatedAt: past}
							legacy = append(legacy, good)
							s.mustExec(t, `INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at) VALUES(?,?,?,?,?,?)`, good.ID, ws, item, past, past, past)
						}
						if shape == "existing" {
							var err error
							existing, err = s.CreateCommitmentSchedule(ws, item, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{})
							if err != nil {
								t.Fatal(err)
							}
						}
						beforeSchedule := renegotiateRows(t, s, "item_schedules", "")
						start := time.Now().UTC().Truncate(time.Second)
						targetWS, targetItem := ws, item
						var logs bytes.Buffer
						old := slog.Default()
						slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
						defer slog.SetDefault(old)
						if mode == "migration" {
							if err := runReminderMigration(s, name, body); err != nil {
								t.Fatal(err)
							}
							if s.countRows(t, `SELECT COUNT(*) FROM schema_migrations WHERE version=?`, name) != 1 {
								t.Fatal("missing version")
							}
							if existing != nil && beforeSchedule != renegotiateRows(t, s, "item_schedules", "") {
								t.Fatal("existing schedule changed")
							}
							assertUnreadableLog(t, logs.String(), map[string]int{reasonField: 1})
						} else {
							workspace, err := s.GetWorkspaceByID(ws)
							if err != nil {
								t.Fatal(err)
							}
							data, err := s.ExportWorkspace(workspace.Slug)
							if err != nil {
								t.Fatal(err)
							}
							for _, r := range legacy {
								data.Reminders = append(data.Reminders, models.ReminderExport{ItemID: item, RemindAt: r.RemindAt, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, FiredAt: derefStr(r.FiredAt), AckedAt: derefStr(r.AckedAt)})
							}
							original, _ := json.Marshal(data)
							imported, report, err := s.ImportWorkspaceWithReport(data, "Unreadable import", "", "test")
							if err != nil {
								t.Fatal(err)
							}
							if len(report.DroppedSchedules) != 0 {
								t.Fatalf("dropped: %+v", report)
							}
							after, _ := json.Marshal(data)
							if !bytes.Equal(original, after) {
								t.Fatal("input mutated")
							}
							out, err := s.ExportWorkspace(imported.Slug)
							if err != nil || len(out.Schedules) != 1 {
								t.Fatalf("export: %+v %v", out, err)
							}
							targetWS, targetItem = imported.ID, out.Schedules[0].ItemID
						}
						sc, err := s.GetSchedule(targetWS, targetItem)
						if err != nil || sc == nil {
							t.Fatalf("schedule: %+v %v", sc, err)
						}
						if s.countRows(t, `SELECT COUNT(*) FROM item_schedules WHERE item_id=?`, targetItem) != 1 {
							t.Fatal("schedule count")
						}
						if shape == "alone" {
							simple, err := s.simpleReminderCommitmentQ(s.db, sc)
							created, parseErr := time.Parse(time.RFC3339, sc.CreatedAt)
							if err != nil || !simple || sc.EndedAt == nil || sc.ReminderID != nil || sc.NextAt != nil || sc.NextWall != nil || derefStr(sc.Tz) != "UTC" || parseErr != nil || created.Before(start) || created.After(time.Now().Add(time.Second)) || sc.Dtstart != strings.TrimSuffix(sc.CreatedAt, "Z") {
								t.Fatalf("not minimal ended schedule: %+v %v", sc, err)
							}
							if s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE schedule_id=? AND change_type='import'`, sc.ID) < 2 {
								t.Fatal("missing carrier choice journal")
							}
							var choice string
							if err := s.db.QueryRow(s.q(`SELECT reason FROM schedule_changes WHERE schedule_id=? AND reason<>? AND change_type='import' LIMIT 1`), sc.ID, "reminder kept as history: unreadable "+reasonField).Scan(&choice); err != nil || !strings.Contains(choice, "migration time") {
								t.Fatalf("choice journal: %q %v", choice, err)
							}
						} else if sc.ReminderID == nil || sc.EndedAt != nil {
							t.Fatalf("readable schedule lost: %+v", sc)
						}
						if shape == "readable" && mode == "migration" && *sc.ReminderID != legacy[1].ID {
							t.Fatal("unreadable row won")
						}
						want := map[string]any{"id": id, "workspace_id": ws, "item_id": item, "remind_at": bad.RemindAt, "created_at": bad.CreatedAt, "updated_at": bad.UpdatedAt, "fired_at": bad.FiredAt, "acked_at": bad.AckedAt}
						if mode == "bundle" {
							delete(want, "id")
							delete(want, "workspace_id")
							want["bundle_index"] = 0
							want["fired_at"] = derefStr(bad.FiredAt)
							want["acked_at"] = derefStr(bad.AckedAt)
						}
						raw, _ := json.Marshal(want)
						var expected map[string]any
						json.Unmarshal(raw, &expected)
						changes, err := s.scheduleChangesQ(s.db, targetWS, "")
						if err != nil {
							t.Fatal(err)
						}
						found := 0
						for _, c := range changes {
							if c.Reason != "reminder kept as history: unreadable "+reasonField {
								continue
							}
							found++
							var got map[string]any
							if c.BeforeJSON == nil || json.Unmarshal([]byte(*c.BeforeJSON), &got) != nil || !reflect.DeepEqual(got, expected) || c.ScheduleID != sc.ID || c.ChangeType != "import" || c.ReasonSource != "system" || c.Actor != "" || (c.AfterJSON != nil && *c.AfterJSON != "{}") {
								t.Fatalf("history mismatch: %+v want %s", c, raw)
							}
						}
						if found != 1 {
							t.Fatalf("history rows=%d", found)
						}
					})
				}
			}
		}
	}
}

func assertUnreadableLog(t *testing.T, raw string, want map[string]int) {
	t.Helper()
	found := 0
	for _, line := range strings.Split(raw, "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil || entry["msg"] != "reminder migration kept unreadable rows as history" {
			continue
		}
		found++
		for _, field := range []string{"remind_at", "created_at", "updated_at", "fired_at", "acked_at"} {
			if entry[field] != float64(want[field]) {
				t.Fatalf("log counts: %s", line)
			}
		}
		if len(entry) != 8 {
			t.Fatalf("log contains more than counts: %s", line)
		}
	}
	if found != boolInt(len(want) > 0) {
		t.Fatalf("log lines=%d: %s", found, raw)
	}
}

func TestUnreadableReminderLogCounts(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, unreadable := range []bool{false, true} {
			t.Run(fmt.Sprintf("pg=%t/unreadable=%t", pg, unreadable), func(t *testing.T) {
				s, name, body := reminderMigrationStore(t, pg)
				counts := map[string]int{}
				for _, field := range []string{"remind_at", "remind_at", "created_at", "updated_at", "fired_at", "acked_at"} {
					_, _, id := legacyReminderRow(t, s, "fired")
					if unreadable {
						s.mustExec(t, `UPDATE item_reminders SET `+field+`='unreadable private text' WHERE id=?`, id)
						counts[field]++
					}
				}
				var logs bytes.Buffer
				old := slog.Default()
				slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
				defer slog.SetDefault(old)
				if err := runReminderMigration(s, name, body); err != nil {
					t.Fatal(err)
				}
				assertUnreadableLog(t, logs.String(), counts)
			})
		}
	}
}

func TestUnreadableReminderReuse(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(fmt.Sprint(pg), func(t *testing.T) {
			s, name, body := reminderMigrationStore(t, pg)
			ws, item, id := legacyReminderRow(t, s, "armed")
			s.mustExec(t, `UPDATE item_reminders SET remind_at='unreadable' WHERE id=?`, id)
			if err := runReminderMigration(s, name, body); err != nil {
				t.Fatal(err)
			}
			sc, err := s.GetSchedule(ws, item)
			if err != nil || sc == nil {
				t.Fatal(err)
			}
			if _, err := s.CreateCommitmentSchedule(ws, item, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{}); !errors.Is(err, ErrScheduleExists) {
				t.Fatalf("native create=%v", err)
			}
			history := renegotiateRows(t, s, "schedule_changes", "WHERE change_type='import'")
			start := make(chan struct{})
			results := make(chan error, 2)
			var wg sync.WaitGroup
			for range 2 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					_, err := s.CreateReminder(ws, item, "2099-01-01T00:00:00Z")
					results <- err
				}()
			}
			close(start)
			wg.Wait()
			close(results)
			success, conflict := 0, 0
			for err := range results {
				if err == nil {
					success++
				} else if errors.Is(err, ErrScheduleExists) {
					conflict++
				} else {
					t.Fatal(err)
				}
			}
			if success != 1 || conflict != 1 {
				t.Fatalf("success=%d conflict=%d", success, conflict)
			}
			got, err := s.GetSchedule(ws, item)
			if err != nil || got.ID != sc.ID || got.EndedAt != nil || got.ReminderID == nil || got.Dtstart != "2099-01-01T00:00:00" || s.countRows(t, `SELECT COUNT(*) FROM item_schedules WHERE item_id=?`, item) != 1 {
				t.Fatalf("reuse: %+v %v", got, err)
			}
			for _, tick := range []struct {
				at string
				n  int
			}{{"2098-12-31T23:59:59Z", 0}, {"2099-01-01T00:00:00Z", 1}, {"2099-01-01T00:00:01Z", 0}} {
				fired, err := s.FireDueReminders(tick.at, 0)
				if err != nil || len(fired) != tick.n {
					t.Fatalf("fire %s=%+v %v", tick.at, fired, err)
				}
			}
			if history != renegotiateRows(t, s, "schedule_changes", "WHERE change_type='import'") {
				t.Fatal("history changed")
			}
			if ok, err := s.DeleteScheduleChecked(ws, item, true, nil); err != nil || !ok {
				t.Fatalf("native delete=%t %v", ok, err)
			}
			if r, err := s.CreateReminder(ws, item, future); err != nil || r == nil || *r.ScheduleID == sc.ID {
				t.Fatalf("fresh create=%+v %v", r, err)
			}
		})
	}
}

func TestEndedReminderCreateRefusals(t *testing.T) {
	for _, pg := range []bool{false, true} {
		for _, shape := range []string{"alarm", "recurring"} {
			t.Run(fmt.Sprintf("pg=%t/%s", pg, shape), func(t *testing.T) {
				s, _, _ := reminderMigrationStore(t, pg)
				ws := createTestWorkspace(t, s, "Ended")
				col := createTestCollection(t, s, ws.ID, "Tasks")
				item := createTestItem(t, s, ws.ID, col.ID, "Ended", "open")
				in := models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}
				if shape == "recurring" {
					in.Rrule = "FREQ=DAILY"
				}
				sc, err := s.CreateCommitmentSchedule(ws.ID, item.ID, in, models.CommitmentRolesInput{})
				if err != nil {
					t.Fatal(err)
				}
				if shape != "alarm" {
					s.mustExec(t, `DELETE FROM item_reminders WHERE schedule_id=?`, sc.ID)
				}
				s.mustExec(t, `UPDATE item_schedules SET next_at=NULL,next_wall=NULL,ended_at=? WHERE id=?`, past, sc.ID)
				before := renegotiateRows(t, s, "item_schedules", "") + renegotiateRows(t, s, "item_reminders", "")
				if _, err := s.CreateReminder(ws.ID, item.ID, future); !errors.Is(err, ErrScheduleExists) {
					t.Fatalf("create=%v", err)
				}
				if before != renegotiateRows(t, s, "item_schedules", "")+renegotiateRows(t, s, "item_reminders", "") {
					t.Fatal("refusal changed state")
				}
			})
		}
	}
}
