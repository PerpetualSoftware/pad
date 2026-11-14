package store

import (
	"os"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestScheduleEndMigrationBackfillsDismissal(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(map[bool]string{false: "sqlite", true: "postgres"}[pg], func(t *testing.T) {
			var s *Store
			fs, path := migrationsFS, "migrations/180_schedule_end_dismissed.sql"
			if pg {
				url := os.Getenv("PAD_TEST_POSTGRES_URL")
				if url == "" {
					t.Skip("PAD_TEST_POSTGRES_URL not set")
				}
				s = testStorePostgres(t, url)
				s.mustExec(t, `ALTER TABLE item_schedules DROP COLUMN ended_dismissed_at`)
				fs, path = pgMigrationsFS, "pgmigrations/141_schedule_end_dismissed.sql"
			} else {
				db, _ := preMigrationDB(t, "180")
				s = &Store{db: db, dialect: &sqliteDialect{}}
			}
			ws := createTestWorkspace(t, s, "End migration")
			col := createTestCollection(t, s, ws.ID, "Tasks")
			ended := createTestItem(t, s, ws.ID, col.ID, "Ended", "")
			live := createTestItem(t, s, ws.ID, col.ID, "Live", "")
			const ts = "2026-10-30T09:05:00Z"
			for _, item := range []*models.Item{ended, live} {
				var endedAt any
				if item.ID == ended.ID {
					endedAt = ts
				}
				s.mustExec(t, `INSERT INTO item_schedules(id,workspace_id,item_id,kind,dtstart,tz,ended_at,created_at,updated_at) VALUES(?,?,?,'check','2026-10-30T09:00:00','UTC',?,?,?)`, newID(), ws.ID, item.ID, endedAt, ts, ts)
			}
			body, err := fs.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if pg {
				err = applyPostgresMigration(s.db, "999_end_dismissal_probe.sql", string(body))
			} else {
				err = applySQLiteMigration(s.db, "180_schedule_end_dismissed.sql", string(body))
			}
			if err != nil {
				t.Fatal(err)
			}
			sc, err := s.GetSchedule(ws.ID, ended.ID)
			if err != nil {
				t.Fatal(err)
			}
			if sc.EndedDismissedAt == nil || *sc.EndedDismissedAt != ts {
				t.Errorf("historical ending not dismissed: %+v", sc)
			}
			liveSC, err := s.GetSchedule(ws.ID, live.ID)
			if err != nil {
				t.Fatal(err)
			}
			if liveSC.EndedDismissedAt != nil {
				t.Fatal("live schedule dismissed")
			}
			if _, err := s.db.Exec(s.q(`UPDATE item_schedules SET ended_dismissed_at=? WHERE item_id=?`), ts, live.ID); err == nil {
				t.Fatal("dismissal without ending accepted")
			}
		})
	}
}
