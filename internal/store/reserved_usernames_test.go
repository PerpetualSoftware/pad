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
