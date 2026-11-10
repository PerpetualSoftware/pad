package server

import (
	"context"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/events"
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

// lapseTickFixture is a one-off native deadline that lapses on the next tick,
// with its event subscription already open.
func lapseTickFixture(t *testing.T, behavior string) (*Server, *models.ItemSchedule, func() int) {
	t.Helper()
	srv := testServerWithEvents(t)
	slug := createWSWithCollections(t, srv)
	item := createItem(t, srv, slug, "tasks", map[string]interface{}{"title": "Lapsing", "fields": `{"status":"open"}`})
	sc, err := srv.store.CreateSchedule(item.WorkspaceID, item.ID, models.ScheduleCreate{Kind: "check", Dtstart: "2099-01-01T09:00", Tz: "UTC"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE item_schedules SET roles_origin='native',deadline_days=0,deadline_secs=0,deadline_behavior=?,next_at='2020-01-01T09:00:00Z',next_wall='2020-01-01T09:00:00' WHERE id=?`), behavior, sc.ID); err != nil {
		t.Fatal(err)
	}
	ch, _, _ := srv.events.Subscribe(context.Background(), item.WorkspaceID)
	t.Cleanup(func() { srv.events.Unsubscribe(ch) })
	drain := func() (updated int) {
		for {
			select {
			case ev := <-ch:
				if ev.Type == events.ItemUpdated && ev.ItemID == item.ID {
					if ev.Actor != "system" || ev.Source != "system" {
						t.Errorf("lapse event actor=%q source=%q", ev.Actor, ev.Source)
					}
					updated++
				}
			case <-time.After(150 * time.Millisecond):
				return updated
			}
		}
	}
	drain()
	return srv, sc, drain
}

func lapseActivities(t *testing.T, srv *Server, itemID string) []string {
	t.Helper()
	rows, err := srv.store.DB().Query(srv.store.D().Rebind(`SELECT actor || ' ' || source || ' ' || metadata FROM activities WHERE document_id = ? AND action = 'schedule_resolved'`), itemID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReminderTickPublishesLapseOnce(t *testing.T) {
	srv, sc, drain := lapseTickFixture(t, "lapses")
	srv.runReminderTick()
	if got := drain(); got != 1 {
		t.Fatalf("item_updated after a lapse = %d, want 1", got)
	}
	want := []string{`system system {"kind":"check","state":"lapsed"}`}
	if got := lapseActivities(t, srv, sc.ItemID); !reflect.DeepEqual(got, want) {
		t.Fatalf("activities = %q, want %q", got, want)
	}
	srv.runReminderTick()
	if got := drain(); got != 0 {
		t.Fatalf("second tick published %d item_updated", got)
	}
	if got := lapseActivities(t, srv, sc.ItemID); len(got) != 1 {
		t.Fatalf("second tick logged again: %q", got)
	}
}

// The miss was journaled by an earlier pass, so this tick closes the occurrence
// without recording anything new. Open views still hold the old occurrence.
func TestReminderTickPublishesLapseOfRecordedMiss(t *testing.T) {
	srv, sc, drain := lapseTickFixture(t, "lapses")
	key := "2020-01-01T09:00:00Z|2020-01-01T09:00:00"
	id := uuid.NewString()
	if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`INSERT INTO schedule_changes(id,workspace_id,item_id,schedule_id,change_type,occurrence_key,role,after_json,actor,root_change_id,recorded_at) VALUES(?,?,?,?,'missed_recorded',?,'deadline',?,'system',?,?)`), id, sc.WorkspaceID, sc.ItemID, sc.ID, key, `{"deadline":"2020-01-01T09:00:00Z"}`, id, "2020-01-02T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	srv.runReminderTick()
	if got := drain(); got != 1 {
		t.Fatalf("item_updated after lapsing a recorded miss = %d, want 1", got)
	}
	if got := lapseActivities(t, srv, sc.ItemID); len(got) != 1 {
		t.Fatalf("activities = %q", got)
	}
}

func TestReminderTickStaysOwedPublishesNoLapse(t *testing.T) {
	srv, sc, drain := lapseTickFixture(t, "stays_owed")
	srv.runReminderTick()
	if got := drain(); got != 0 {
		t.Fatalf("stays_owed miss published %d item_updated", got)
	}
	if got := lapseActivities(t, srv, sc.ItemID); len(got) != 0 {
		t.Fatalf("stays_owed miss logged %q", got)
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
