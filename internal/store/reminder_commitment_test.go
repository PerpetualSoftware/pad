package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestOneShotReminderCreatesCommitment(t *testing.T) {
	for _, at := range []string{future, "2099-01-01T01:00:00+01:00", "2099-01-01T00:00:00.1Z", nowTS(), past} {
		t.Run(at, func(t *testing.T) {
			f := newSchedFixture(t)
			r, err := f.s.CreateReminder(f.ws.ID, f.item.ID, at)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := normalizeRemindAt(at)
			if r.ScheduleID == nil || r.OccurrenceAt == nil || *r.OccurrenceAt != want || r.RemindAt != want {
				t.Fatalf("not a managed exact occurrence: %+v", r)
			}
			sc := f.reload(t, f.item.ID)
			if sc.ReminderID == nil || *sc.ReminderID != r.ID || sc.ID != *r.ScheduleID || derefStr(sc.Tz) != "UTC" || sc.Floating || sc.Anchor != "calendar" || sc.LeadMinutes != 0 {
				t.Fatalf("wrong commitment: %+v", sc)
			}
			tx, err := f.s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			roles, err := f.s.scheduleRolesOf(tx, sc)
			tx.Rollback()
			if err != nil || roles.Origin != "native" || roles.TargetDays != 0 || roles.TargetSecs != 0 || roles.DeadlineDays != nil || roles.EarliestDays != nil || roles.OccupiedDays != nil {
				t.Fatalf("roles=%+v err=%v", roles, err)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM item_reminders WHERE schedule_id IS NULL`); n != 0 {
				t.Fatalf("bare rows: %d", n)
			}
			atTime, _ := time.Parse(time.RFC3339, want)
			rows, _, err := f.s.ListSchedulesForAgenda(f.ws.ID, PendingReminderScope{}, atTime.Add(-time.Hour), atTime.Add(time.Hour), 100)
			if err != nil || len(rows) != 1 {
				t.Fatalf("agenda=%+v err=%v", rows, err)
			}
		})
	}
}

func TestOneShotReminderExistingCommitment(t *testing.T) {
	for _, state := range []string{"active", "acknowledged", "ended", "concurrent"} {
		t.Run(state, func(t *testing.T) {
			f := newSchedFixture(t)
			if state == "concurrent" {
				start := make(chan struct{})
				errs := make(chan error, 2)
				var wg sync.WaitGroup
				for range 2 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						_, err := f.s.CreateReminder(f.ws.ID, f.item.ID, future)
						errs <- err
					}()
				}
				close(start)
				wg.Wait()
				close(errs)
				successes, conflicts := 0, 0
				for err := range errs {
					if err == nil {
						successes++
					} else if errors.Is(err, ErrScheduleExists) {
						conflicts++
					} else {
						t.Fatal(err)
					}
				}
				if successes != 1 || conflicts != 1 || f.count(t, `SELECT COUNT(*) FROM item_reminders`) != 1 {
					t.Fatalf("success=%d conflict=%d", successes, conflicts)
				}
				return
			}
			sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{})
			if err != nil {
				t.Fatal(err)
			}
			if state == "acknowledged" {
				f.exec(t, `UPDATE item_reminders SET fired_at=?,acked_at=? WHERE schedule_id=?`, past, past, sc.ID)
			}
			if state == "ended" {
				f.exec(t, `DELETE FROM item_reminders WHERE schedule_id=?`, sc.ID)
				f.exec(t, `UPDATE item_schedules SET next_at=NULL,next_wall=NULL,ended_at=? WHERE id=?`, past, sc.ID)
			}
			before := renegotiateRows(t, f.s, "item_schedules", "") + renegotiateRows(t, f.s, "item_reminders", "")
			if _, err := f.s.CreateReminder(f.ws.ID, f.item.ID, future); !errors.Is(err, ErrScheduleExists) {
				t.Fatalf("want schedule_exists, got %v", err)
			}
			if after := renegotiateRows(t, f.s, "item_schedules", "") + renegotiateRows(t, f.s, "item_reminders", ""); after != before {
				t.Fatal("conflict changed rows")
			}
		})
	}
}

func TestOneShotReminderLifecycle(t *testing.T) {
	for _, state := range []string{"armed", "fired", "acked"} {
		for _, at := range []string{past, future} {
			t.Run(state+at, func(t *testing.T) {
				f := newSchedFixture(t)
				id := armReminder(t, f.s, f.ws.ID, f.item.ID, past)
				if state != "armed" {
					f.fire(t, nowTS())
				}
				if state == "acked" {
					r, err := f.s.AckReminder(f.ws.ID, id)
					if err != nil || r == nil {
						t.Fatalf("ack: %v %v", r, err)
					}
					again, err := f.s.AckReminder(f.ws.ID, id)
					if err != nil || again == nil || derefStr(r.AckedAt) != derefStr(again.AckedAt) || r.UpdatedAt != again.UpdatedAt {
						t.Fatalf("retry: %v %v", again, err)
					}
					if f.count(t, `SELECT COUNT(*) FROM schedule_changes WHERE change_type='ack'`) != 1 {
						t.Fatal("ack journal missing or repeated")
					}
				}
				r, err := f.s.RearmReminder(f.ws.ID, id, at)
				if err != nil || r == nil || r.ID != id || !r.Armed() || r.ScheduleID == nil || derefStr(r.OccurrenceAt) != at {
					t.Fatalf("rearm=%+v err=%v", r, err)
				}
				sc := f.reload(t, f.item.ID)
				if derefStr(sc.NextAt) != at || sc.ReminderAckedAt != nil {
					t.Fatalf("rearm commitment=%+v", sc)
				}
				if r, err := f.s.AckReminder(f.ws.ID, id); r != nil || (err != nil && !errors.Is(err, ErrOccurrenceNotFired)) {
					t.Fatalf("armed ack=%+v %v", r, err)
				}
				ok, err := f.s.DeleteReminder(f.ws.ID, id)
				if err != nil || !ok {
					t.Fatalf("delete=%v %v", ok, err)
				}
				if f.count(t, `SELECT COUNT(*) FROM item_schedules`) != 0 || f.count(t, `SELECT COUNT(*) FROM item_reminders`) != 0 {
					t.Fatal("delete left commitment")
				}
			})
		}
	}
}

func TestOneShotReminderIdentityRace(t *testing.T) {
	for _, action := range []string{"ack", "rearm", "delete"} {
		for _, change := range []string{"replace", "enrich"} {
			t.Run(action+"/"+change, func(t *testing.T) {
				f := newSchedFixture(t)
				id := armReminder(t, f.s, f.ws.ID, f.item.ID, past)
				f.fire(t, nowTS())
				f.s.beforeCommitmentPin = func() {
					f.s.beforeCommitmentPin = nil
					if change == "replace" {
						if _, err := f.s.DeleteScheduleChecked(f.ws.ID, f.item.ID, true, nil); err != nil {
							t.Fatal(err)
						}
						armReminder(t, f.s, f.ws.ID, f.item.ID, future)
					} else {
						f.exec(t, `UPDATE item_schedules SET rrule='FREQ=DAILY' WHERE item_id=?`, f.item.ID)
					}
				}
				var err error
				switch action {
				case "ack":
					_, err = f.s.AckReminder(f.ws.ID, id)
				case "rearm":
					_, err = f.s.RearmReminder(f.ws.ID, id, future)
				case "delete":
					_, err = f.s.DeleteReminder(f.ws.ID, id)
				}
				f.s.beforeCommitmentPin = nil
				if change == "enrich" && !errors.Is(err, ErrReminderManaged) {
					t.Fatalf("enriched mutation: %v", err)
				}
				sc := f.reload(t, f.item.ID)
				r := f.managed(t, sc)
				if change == "replace" && (r.ID == id || r.RemindAt != future || !r.Armed()) {
					t.Fatalf("replacement changed: %+v", r)
				}
				if change == "enrich" && (r.AckedAt != nil || r.RemindAt != past || r.FiredAt == nil) {
					t.Fatalf("richer plan changed: %+v", r)
				}
			})
		}
	}
	t.Run("stale fire", func(t *testing.T) {
		f := newSchedFixture(t)
		id := armReminder(t, f.s, f.ws.ID, f.item.ID, past)
		if _, err := f.s.RearmReminder(f.ws.ID, id, future); err != nil {
			t.Fatal(err)
		}
		r, err := f.s.fireOneReminder(id, nowTS())
		if err != nil || r != nil || f.events(t) != 0 {
			t.Fatalf("stale fire=%v %v", r, err)
		}
	})
}

func TestNativePastOneShotStillRefused(t *testing.T) {
	f := newSchedFixture(t)
	_, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2020-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{})
	if !errors.Is(err, ErrScheduleExhausted) {
		t.Fatalf("native create=%v", err)
	}
}

func TestReminderRequiresSchedule(t *testing.T) {
	f := newSchedFixture(t)
	for _, query := range []string{
		`INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		`INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at,schedule_id) VALUES(?,?,?,?,?,?,NULL)`,
		`INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at,schedule_id) SELECT ?,?,?,?,?,?,NULL`,
	} {
		if _, err := f.s.db.Exec(f.s.q(query), newID(), f.ws.ID, f.item.ID, past, past, past); err == nil {
			t.Error("NULL schedule accepted")
		}
	}
	sc, err := f.s.CreateCommitmentSchedule(f.ws.ID, f.item.ID, models.ScheduleCreate{Dtstart: "2099-01-01T00:00:00", Tz: "UTC"}, models.CommitmentRolesInput{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`UPDATE item_reminders SET schedule_id=NULL`, `UPDATE item_reminders SET schedule_id='missing'`} {
		if _, err := f.s.db.Exec(q); err == nil {
			t.Error("invalid update accepted")
		}
	}
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO item_reminders(id,workspace_id,item_id,remind_at,created_at,updated_at,schedule_id) VALUES(?,?,?,?,?,?,?)`), newID(), f.ws.ID, f.item.ID, past, past, past, sc.ID); err == nil {
		t.Error("duplicate schedule accepted")
	}
}

func TestLegacyReminderBundleConversion(t *testing.T) {
	f := newSchedFixture(t)
	data, err := f.s.ExportWorkspace(f.ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	data.Reminders = []models.ReminderExport{{ItemID: f.item.ID, RemindAt: past, FiredAt: past, AckedAt: past, CreatedAt: past, UpdatedAt: past}}
	ws, err := f.s.ImportWorkspace(data, "Imported reminders", "", "test")
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.s.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Reminders) != 0 || len(out.Schedules) != 1 || out.Schedules[0].Reminder == nil || out.Schedules[0].Reminder.AckedAt != past {
		t.Fatalf("bad bundle: reminders=%+v schedules=%+v", out.Reminders, out.Schedules)
	}
	before := f.count(t, `SELECT COUNT(*) FROM workspaces`)
	data.Reminders = append(data.Reminders, data.Reminders[0])
	if _, err := f.s.ImportWorkspace(data, "Collision", "", "test"); err == nil {
		t.Fatal("collision accepted")
	}
	if f.count(t, `SELECT COUNT(*) FROM workspaces`) != before {
		t.Fatal("collision wrote workspace")
	}
}

func TestOneShotReminderClosedItemPauses(t *testing.T) {
	f := newSchedFixture(t)
	id := armReminder(t, f.s, f.ws.ID, f.item.ID, past)
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"done"}`), Force: true}); err != nil {
		t.Fatal(err)
	}
	if fired := f.fire(t, nowTS()); len(fired) != 0 {
		t.Fatal("closed commitment fired")
	}
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: ptr(`{"status":"open"}`)}); err != nil {
		t.Fatal(err)
	}
	fired := f.fire(t, nowTS())
	if len(fired) != 1 || fired[0].ID != id || len(f.fire(t, nowTS())) != 0 {
		t.Fatalf("reopened commitment: %+v", fired)
	}
}
