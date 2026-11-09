package store

import (
	"os"
	"testing"
)

func TestCommitmentNotices_MigrationSQLite(t *testing.T) {
	t.Parallel()
	db, _ := preMigrationDB(t, "169")
	s := &Store{db: db, dialect: &sqliteDialect{}}
	checkNoticeMigration(t, s, false)
	for _, name := range []string{"idx_user_notifications_state", "idx_user_notifications_sent", "idx_user_notifications_workspace", "idx_user_notifications_actor"} {
		if n := s.countRows(t, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, name); n != 1 {
			t.Fatalf("lost index %s", name)
		}
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}

func TestCommitmentNotices_MigrationPostgres(t *testing.T) {
	t.Parallel()
	url := os.Getenv("PAD_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, url)
	s.mustExec(t, `ALTER TABLE user_notifications DROP CONSTRAINT user_notifications_kind_check`)
	s.mustExec(t, `ALTER TABLE user_notifications ADD CONSTRAINT user_notifications_kind_check CHECK(kind IN ('assigned','comment','reply','reminder','mention'))`)
	checkNoticeMigration(t, s, true)
}

func checkNoticeMigration(t *testing.T, s *Store, pg bool) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Notice migration")
	user := notifyUser(t, s, "migration-notices@example.com")
	insert := func(id, kind string) error {
		_, err := s.db.Exec(s.q(`INSERT INTO user_notifications (id,user_id,workspace_id,item_id,comment_id,kind,actor_user_id,actor_is_agent,dedupe_key,created_at,push_state,skip_reason,claim_token,claimed_at,attempts,sent_at) VALUES (?,?,?,'item','comment',?,?,? ,?,'2026-11-01T00:00:00Z','sent','reason','claim','2026-11-01T00:00:00Z',3,'2026-11-01T00:00:01Z')`), id, user.ID, ws.ID, kind, user.ID, s.dialect.BoolToInt(true), kind)
		return err
	}
	for _, kind := range []string{"assigned", "comment", "reply", "reminder", "mention"} {
		if err := insert(kind, kind); err != nil {
			t.Fatal(err)
		}
	}
	before := renegotiateRows(t, s, "user_notifications", "")
	fs, path := migrationsFS, "migrations/169_commitment_notifications.sql"
	if pg {
		fs, path = pgMigrationsFS, "pgmigrations/132_commitment_notifications.sql"
	}
	body, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if pg {
		err = applyPostgresMigration(s.db, "999_notice_probe.sql", string(body))
	} else {
		err = applySQLiteMigration(s.db, "169_commitment_notifications.sql", string(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if after := renegotiateRows(t, s, "user_notifications", ""); after != before {
		t.Fatal("migration changed existing rows")
	}
	for _, kind := range []string{"unassigned", "renegotiation_refused"} {
		if err := insert(kind, kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := insert("bad", "unknown"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if err := insert("duplicate", "unassigned"); err == nil {
		t.Fatal("duplicate key accepted")
	}
}
