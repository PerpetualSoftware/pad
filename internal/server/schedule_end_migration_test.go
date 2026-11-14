package server

import (
	"os"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
	"github.com/PerpetualSoftware/pad/internal/usertz"
)

func TestAgendaScheduleEndBackfill(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(map[bool]string{false: "sqlite", true: "postgres"}[pg], func(t *testing.T) {
			var s *store.Store
			path := "../store/migrations/180_schedule_end_dismissed.sql"
			if pg {
				s = storetest.NewPostgres(t)
				path = "../store/pgmigrations/141_schedule_end_dismissed.sql"
			} else {
				s = storetest.NewSQLite(t)
			}
			srv := New(s)
			t.Cleanup(srv.Stop)
			owner := mustUser(t, srv, "end-migration@example.com", "migration", "")
			ws := mustWorkspace(t, srv, "Migration agenda", owner.ID)
			col := mustCollection(t, srv, ws.ID, "Tasks")
			item := mustItem(t, srv, ws.ID, col.ID, "Previously ended")
			// Reconstruct the schema immediately before the dismissal migration.
			exec := func(q string, args ...any) {
				t.Helper()
				if _, err := s.DB().Exec(s.D().Rebind(q), args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`UPDATE collections SET schema=? WHERE id=?`, agendaSchema, col.ID)
			exec(`ALTER TABLE item_schedules DROP COLUMN ended_dismissed_at`)
			const endedAt = "2031-03-10T11:00:00Z"
			exec(`INSERT INTO item_schedules(id,workspace_id,item_id,kind,dtstart,tz,ended_at,created_at,updated_at) VALUES('historical',?,?,'check','2031-03-10T09:00:00','UTC',?,?,?)`, ws.ID, item.ID, endedAt, endedAt, endedAt)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tx, err := s.DB().Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(string(body)); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			sc, err := s.GetSchedule(ws.ID, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if sc.EndedDismissedAt == nil || *sc.EndedDismissedAt != endedAt {
				t.Errorf("historical ending not dismissed: %+v", sc)
			}
			clock := time.Date(2031, 3, 10, 12, 0, 0, 0, time.UTC)
			srv.agendaClock = func() time.Time { return clock }
			run := srv.newAgendaRead(clock, clock.Add(time.Hour), usertz.Resolve(nil), "")
			srv.appendAgendaWork(run, ws.ID, store.PendingReminderScope{})
			if !run.response.PriorityListComplete || len(run.response.PriorityList) != 0 {
				t.Fatalf("historical ending in Outstanding: %+v", run.response)
			}
		})
	}
}
