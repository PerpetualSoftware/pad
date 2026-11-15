package store

import (
	"os"
	"testing"
)

func TestScheduleReopenMigration(t *testing.T) {
	for _, pg := range []bool{false, true} {
		t.Run(map[bool]string{false: "sqlite", true: "postgres"}[pg], func(t *testing.T) {
			var s *Store
			if pg {
				url := os.Getenv("PAD_TEST_POSTGRES_URL")
				if url == "" {
					t.Skip("PAD_TEST_POSTGRES_URL not set")
				}
				s = testStorePostgres(t, url)
				s.mustExec(t, `DROP TABLE schedule_reopen_checkpoints`)
				s.mustExec(t, `ALTER TABLE schedule_changes DROP CONSTRAINT schedule_changes_change_type_check`)
				s.mustExec(t, `ALTER TABLE schedule_changes ADD CONSTRAINT schedule_changes_change_type_check CHECK(change_type IN ('create','update','delete','move','snooze','done','skip','missed_recorded','end','import','carry','ack'))`)
			} else {
				db, _ := preMigrationDB(t, "181")
				s = &Store{db: db, dialect: &sqliteDialect{}}
			}
			ws := createTestWorkspace(t, s, "Reopen migration")
			insert := func(id, change string) error {
				_, err := s.db.Exec(s.q(`INSERT INTO schedule_changes(id,workspace_id,item_id,schedule_id,occurrence_key,change_type,role,before_json,after_json,actor,reason,reason_source,root_change_id,parent_change_id,recorded_at) VALUES(?,?, 'item','schedule','key',?,'target','{"old":1}','{"new":2}','agent','reason','user','root','parent','2026-10-30T10:00:00Z')`), id, ws.ID, change)
				return err
			}
			for _, c := range []string{"create", "update", "delete", "move", "snooze", "done", "skip", "missed_recorded", "end", "import", "carry", "ack"} {
				if err := insert(c, c); err != nil {
					t.Fatal(err)
				}
			}
			var objects string
			if !pg {
				objects = renegotiateRows(t, s, "(SELECT name AS id,type,name,sql FROM sqlite_master WHERE tbl_name='schedule_changes' AND type IN ('index','trigger'))", "")
			}
			before := renegotiateRows(t, s, "schedule_changes", "")
			fs, path := migrationsFS, "migrations/181_schedule_reopen.sql"
			if pg {
				fs, path = pgMigrationsFS, "pgmigrations/142_schedule_reopen.sql"
			}
			body, err := fs.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if pg {
				err = applyPostgresMigration(s.db, "999_reopen_probe.sql", string(body))
			} else {
				err = applySQLiteMigration(s.db, "181_schedule_reopen.sql", string(body))
			}
			if err != nil {
				t.Fatal(err)
			}
			if after := renegotiateRows(t, s, "schedule_changes", ""); after != before {
				t.Fatal("journal changed during migration")
			}
			if !pg && objects != renegotiateRows(t, s, "(SELECT name AS id,type,name,sql FROM sqlite_master WHERE tbl_name='schedule_changes' AND type IN ('index','trigger'))", "") {
				t.Fatal("index or trigger definitions changed")
			}
			if err := insert("reopen", "reopen"); err != nil {
				t.Fatal(err)
			}
			if err := insert("invalid", "invalid"); err == nil {
				t.Fatal("invalid enum accepted")
			}
			if !pg {
				for _, name := range []string{"idx_schedule_changes_schedule", "idx_schedule_changes_item"} {
					if s.countRows(t, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name) != 1 {
						t.Fatal("lost index", name)
					}
				}
				for _, col := range []string{"actor", "reason", "occurrence_key", "before_json", "after_json", "role"} {
					if _, err := s.db.Exec(`UPDATE schedule_changes SET `+col+`=? WHERE id='reopen'`, "\x00"); err == nil {
						t.Fatal("lost NUL guard", col)
					}
				}
				rows, err := s.db.Query(`PRAGMA foreign_key_check`)
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
				if rows.Next() {
					t.Fatal("foreign key violation")
				}
			}
			s.mustExec(t, `DELETE FROM workspaces WHERE id=?`, ws.ID)
			if s.countRows(t, `SELECT COUNT(*) FROM schedule_changes WHERE workspace_id=?`, ws.ID) != 0 {
				t.Fatal("lost cascading FK")
			}
		})
	}
}
