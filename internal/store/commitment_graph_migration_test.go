package store

import (
	"os"
	"testing"
)

var graphNoticeKinds = []string{"deadline_approval_requested", "commitment_moved", "commitment_moved_past_deadline", "commitment_gate_opened"}

func TestTASK4423_R05Migration(t *testing.T) {
	t.Run("SQLite", func(t *testing.T) {
		db, _ := preMigrationDB(t, "175")
		s := &Store{db: db, dialect: &sqliteDialect{}}
		checkGraphNoticeMigration(t, s, false)
		rows, err := db.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if rows.Next() {
			t.Fatal("foreign key violation")
		}
	})
	t.Run("Postgres", func(t *testing.T) {
		url := os.Getenv("PAD_TEST_POSTGRES_URL")
		if url == "" {
			t.Skip("PAD_TEST_POSTGRES_URL not set")
		}
		s := testStorePostgres(t, url)
		s.mustExec(t, `ALTER TABLE user_notifications DROP CONSTRAINT user_notifications_kind_check`)
		s.mustExec(t, `ALTER TABLE user_notifications ADD CONSTRAINT user_notifications_kind_check CHECK(kind IN ('assigned','comment','reply','reminder','mention','unassigned','renegotiation_refused'))`)
		checkGraphNoticeMigration(t, s, true)
	})
}

func checkGraphNoticeMigration(t *testing.T, s *Store, pg bool) {
	t.Helper()
	ws := createTestWorkspace(t, s, "Graph migration")
	// Seed the historical schema without the current user-column reader.
	userID := newID()
	s.mustExec(t, `INSERT INTO users (id, email, name, password_hash, created_at, updated_at) VALUES (?, ?, 'Migration', '', ?, ?)`, userID, "graph-migration@example.com", now(), now())
	insert := func(id, kind string) error {
		_, err := s.db.Exec(s.q(`INSERT INTO user_notifications (id,user_id,workspace_id,item_id,comment_id,kind,actor_user_id,actor_is_agent,dedupe_key,created_at,push_state,skip_reason,claim_token,claimed_at,attempts,sent_at) VALUES (?,?,?,'subject','comment',?,?,?,?,'2026-11-01T00:00:00Z','sent','reason','claim','2026-11-01T00:00:00Z',3,'2026-11-01T00:00:01Z')`), id, userID, ws.ID, kind, userID, s.dialect.BoolToInt(true), kind)
		return err
	}
	old := []string{"assigned", "comment", "reply", "reminder", "mention", "unassigned", "renegotiation_refused"}
	for _, kind := range old {
		if err := insert(kind, kind); err != nil {
			t.Fatal(err)
		}
	}
	before := renegotiateRows(t, s, "user_notifications", "")
	schema := func() string {
		if pg {
			rows, err := s.db.Query(`SELECT indexname,indexdef FROM pg_indexes WHERE schemaname=current_schema() AND tablename='user_notifications' ORDER BY indexname`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			out := ""
			for rows.Next() {
				var name, body string
				if err := rows.Scan(&name, &body); err != nil {
					t.Fatal(err)
				}
				out += name + "|" + body + "\n"
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			return out
		}
		// All indexes, including the unique autoindex, and every currently installed
		// trigger must survive. The server-only NUL classification installs none.
		rows, err := s.db.Query(`SELECT type,name,COALESCE(sql,'') FROM sqlite_master WHERE tbl_name='user_notifications' AND type IN ('trigger','index') ORDER BY type,name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := ""
		for rows.Next() {
			var typ, name, body string
			if err := rows.Scan(&typ, &name, &body); err != nil {
				t.Fatal(err)
			}
			out += typ + "|" + name + "|" + body + "\n"
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	beforeSchema := schema()
	fs, path := migrationsFS, "migrations/175_commitment_graph_notifications.sql"
	if pg {
		fs, path = pgMigrationsFS, "pgmigrations/137_commitment_graph_notifications.sql"
	}
	body, err := fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if pg {
		err = applyPostgresMigration(s.db, "999_graph_notice_probe.sql", string(body))
	} else {
		err = applySQLiteMigration(s.db, "175_commitment_graph_notifications.sql", string(body))
	}
	if err != nil {
		t.Fatal(err)
	}
	if after := renegotiateRows(t, s, "user_notifications", ""); after != before {
		t.Fatal("notification columns or rows changed")
	}
	if after := schema(); after != beforeSchema {
		t.Fatalf("indexes or triggers changed:\n%s\n%s", beforeSchema, after)
	}
	for _, kind := range graphNoticeKinds {
		if err := insert(kind, kind); err != nil {
			t.Fatal(err)
		}
	}
	if err := insert("bad", "unknown"); err == nil {
		t.Fatal("accepted unknown kind")
	}
	if err := insert("duplicate", graphNoticeKinds[0]); err == nil {
		t.Fatal("lost unique recipient/key constraint")
	}
	if err := insert("duplicate-old", "assigned"); err == nil {
		t.Fatal("lost old unique recipient/key constraint")
	}
}
