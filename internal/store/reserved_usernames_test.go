package store

import "testing"

func TestEnsureUniqueUsernameReserved(t *testing.T) {
	s := testStoreSQLite(t)
	for _, tc := range []struct{ base, want string }{
		{"settings", "settings-2"},
		{"tutorials", "tutorials-2"},
		{"needs", "needs-2"},
		{"mara", "mara"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			got, err := s.EnsureUniqueUsername(tc.base)
			if err != nil || got != tc.want {
				t.Fatalf("EnsureUniqueUsername(%q) = %q, %v; want %q", tc.base, got, err, tc.want)
			}
		})
	}
}

func TestUniqueUsernameTxReserved(t *testing.T) {
	s := testStoreSQLite(t)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := s.uniqueUsernameTx(tx, "settings", "unused-user-id")
	if err != nil || got != "settings-2" {
		t.Fatalf("uniqueUsernameTx(settings) = %q, %v; want settings-2", got, err)
	}
}

func TestBackfillUsernamesSkipsReserved(t *testing.T) {
	s := testStoreSQLite(t)
	ts := "2026-01-01T00:00:00Z"
	if _, err := s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, password_set, created_at, updated_at)
		VALUES (?, ?, '', ?, 'x', 'member', ?, ?, ?)
	`), "backfill-reserved-user", "needs@example.invalid", "Needs", true, ts, ts); err != nil {
		t.Fatal(err)
	}
	if err := s.backfillUsernames(); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow(s.q(`SELECT username FROM users WHERE id = ?`), "backfill-reserved-user").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "needs-2" {
		t.Fatalf("backfilled username = %q; want needs-2", got)
	}
}
