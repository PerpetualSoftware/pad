package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// BUG-3826: follow-ups to BUG-3811's last-active-admin guard.

// advisoryHasWaiter reports whether a session waits on the advisory lock
// keyed hashtext(key); bug3811GuardHasWaiter's query for any key.
func advisoryHasWaiter(t *testing.T, db *sql.DB, key string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND objsubid = 1
		AND classid = (((hashtext($1)::bigint) >> 32) & 4294967295)::oid
		AND objid = ((hashtext($1)::bigint) & 4294967295)::oid`, key).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// Unlike TestBUG3811_ConcurrentRemovalsKeepAnActiveAdmin, which holds demote
// or disable open as the FIRST door, this holds account deletion open, at the point it
// already holds its user-row lock, so a regression that moved the deletion's
// guard after that lock (inverting account-deletion -> active-admins -> row)
// lets the second door run straight through, and goes red.
func TestBUG3826_DeleteSelfHeldFirstKeepsAnActiveAdmin(t *testing.T) {
	second := map[string]struct {
		run      func(s *Store, id string) error
		waitsFor string
	}{
		"demote":      {func(s *Store, id string) error { return s.SetUserRole(id, "member") }, "pad:active-admins"},
		"disable":     {func(s *Store, id string) error { return s.DisableUserAndRevokeAccess(id) }, "pad:active-admins"},
		"delete-self": {func(s *Store, id string) error { return s.DeleteAccountAtomic(id) }, "pad:account-deletion"},
	}
	for sn, b2 := range second {
		t.Run("delete-self/"+sn, func(t *testing.T) {
			s := testStore(t)
			if s.dialect.Driver() != DriverPostgres {
				t.Skip("Postgres only: SQLite's single writer serializes the two removals")
			}
			a, b := bug3811Admins(t, s)

			done := make(chan error, 1)
			deleteAccountAfterUserLockHook = func() {
				deleteAccountAfterUserLockHook = nil
				go func() { done <- b2.run(s, b) }()
				// Let A commit only once B is seen WAITING, so the pair is
				// concurrent by observation, not by a timer.
				deadline := time.Now().Add(10 * time.Second)
				for !advisoryHasWaiter(t, s.db, b2.waitsFor) {
					if time.Now().After(deadline) {
						t.Errorf("B's %s never waited on %s while A's deletion held its user row", sn, b2.waitsFor)
						return
					}
					select {
					case err := <-done:
						t.Errorf("B's %s returned before A's deletion committed (err %v): it did not wait", sn, err)
						done <- err
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
			t.Cleanup(func() { deleteAccountAfterUserLockHook = nil })

			if err := s.DeleteAccountAtomic(a); err != nil {
				t.Fatalf("A's deletion: %v", err)
			}
			var bErr error
			select {
			case bErr = <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("B's %s never returned after A's deletion committed", sn)
			}
			if n := bug3811ActiveAdmins(t, s); n == 0 {
				t.Errorf("zero active admins: B's %s returned %v", sn, bErr)
			}
			if !errors.Is(bErr, ErrLastAdmin) {
				t.Errorf("B's %s err = %v, want ErrLastAdmin", sn, bErr)
			}
		})
	}
}

// A role change or disable whose user is gone answers ErrUserNotFound: a
// promotion used to answer ErrLastAdmin (a false 409 last_active_admin), a
// demotion a plain error (a 500), and a disable succeeded on nothing.
func TestBUG3826_VanishedUserIsNotFound(t *testing.T) {
	doors := map[string]func(s *Store, id string) error{
		"promote": func(s *Store, id string) error { return s.SetUserRole(id, "admin") },
		"demote":  func(s *Store, id string) error { return s.SetUserRole(id, "member") },
		"disable": func(s *Store, id string) error { return s.DisableUserAndRevokeAccess(id) },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			bug3811Admins(t, s)
			u := createTestUser(t, s, "gone@test.com", "Gone", "password123")
			if err := s.DeleteAccountAtomic(u.ID); err != nil {
				t.Fatal(err)
			}
			err := door(s, u.ID)
			if !errors.Is(err, ErrUserNotFound) {
				t.Errorf("%s of a deleted user err = %v, want ErrUserNotFound", name, err)
			}
			if errors.Is(err, ErrLastAdmin) {
				t.Errorf("%s of a deleted user answered ErrLastAdmin", name)
			}
		})
	}
}
