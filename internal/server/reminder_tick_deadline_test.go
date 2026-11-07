package server

import (
	"log/slog"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func tickDeadlineFixture(t *testing.T) (*Server, *models.ItemSchedule, *lockedBuf) {
	t.Helper()
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Tick deadline", "fields": `{"status":"open"}`})
	sc, err := srv.store.CreateSchedule(item.WorkspaceID, item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = srv.store.DB().Exec(`UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=0,deadline_behavior='stays_owed',next_at='2020-01-01T09:00:00Z',next_wall='2020-01-01T09:00:00' WHERE id=?`, sc.ID); err != nil {
		t.Fatal(err)
	}
	logs := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return srv, sc, logs
}

func TestReminderTickDeadlineCapLog(t *testing.T) {
	srv, _, logs := tickDeadlineFixture(t)
	srv.SetReminderTickConfig(0, 1)
	srv.runReminderTick()
	if logs.count(`level=WARN msg="reminder tick: deadline evaluation reached cap" cap=1`) != 1 {
		t.Fatal("deadline cap warning missing")
	}
	if logs.count(`level=INFO msg="reminder tick: missed deadlines" count=1`) != 1 {
		t.Fatal("deadline count missing")
	}
}

func TestReminderTickRunsBothHalvesOnError(t *testing.T) {
	for _, half := range []string{"reminder", "deadline"} {
		t.Run(half, func(t *testing.T) {
			srv, sc, logs := tickDeadlineFixture(t)
			db := srv.store.DB()
			if _, err := db.Exec(`UPDATE item_reminders SET remind_at='2020-01-01T09:00:00Z' WHERE schedule_id=?`, sc.ID); err != nil {
				t.Fatal(err)
			}
			trigger := `CREATE TRIGGER fail_tick BEFORE UPDATE OF fired_at ON item_reminders BEGIN SELECT RAISE(ABORT,'reminder failure'); END`
			if half == "deadline" {
				trigger = `CREATE TRIGGER fail_tick BEFORE INSERT ON schedule_changes WHEN NEW.change_type='missed_recorded' BEGIN SELECT RAISE(ABORT,'deadline failure'); END`
			}
			if _, err := db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			srv.runReminderTick()
			var misses, fired int
			if err := db.QueryRow(`SELECT COUNT(*) FROM schedule_changes WHERE role='deadline' AND change_type='missed_recorded'`).Scan(&misses); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow(`SELECT COUNT(*) FROM item_reminders WHERE schedule_id=? AND fired_at IS NOT NULL`, sc.ID).Scan(&fired); err != nil {
				t.Fatal(err)
			}
			if half == "reminder" {
				if misses != 1 || fired != 0 || logs.count("level=ERROR") == 0 {
					t.Fatalf("reminder failure: misses=%d fired=%d", misses, fired)
				}
			} else {
				if misses != 0 || fired != 1 || logs.count("deadline evaluation failed") == 0 {
					t.Fatalf("deadline failure: misses=%d fired=%d", misses, fired)
				}
			}
		})
	}
}
