package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// BUG-3811: the instance always keeps at least one ACTIVE human admin. Three
// doors can take an admin out of that set (demote, disable, delete own
// account), and each runs guardActiveAdminLossTx in its transaction.

func bug3811Admins(t *testing.T, s *Store) (a, b string) {
	t.Helper()
	ua := createTestUser(t, s, "a@test.com", "A", "password123")
	ub := createTestUser(t, s, "b@test.com", "B", "password123")
	for _, id := range []string{ua.ID, ub.ID} {
		if err := s.SetUserRole(id, "admin"); err != nil {
			t.Fatal(err)
		}
	}
	return ua.ID, ub.ID
}

func bug3811ActiveAdmins(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM users
		WHERE role = 'admin' AND kind = 'human' AND disabled_at IS NULL`)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// bug3811GuardHasWaiter reports whether a session is waiting on the
// pad:active-admins advisory lock. pg_locks splits the bigint key into classid
// (high 32 bits) and objid (low 32 bits), with objsubid 1.
func bug3811GuardHasWaiter(t *testing.T, db *sql.DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE locktype = 'advisory' AND NOT granted
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND objsubid = 1
		AND classid = (((hashtext('pad:active-admins')::bigint) >> 32) & 4294967295)::oid
		AND objid = ((hashtext('pad:active-admins')::bigint) & 4294967295)::oid`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// The racy pairs. The interleaving is made deterministic by holding the
// FIRST removal of admin A open in a transaction (the door's real
// transaction body, uncommitted) while the SECOND door, the real public
// store call, removes admin B. The second must wait for the first and then
// refuse with ErrLastAdmin. Before the fix the demote pair returned at once
// and both committed; disable and delete had no guard at all.
//
// Postgres only: on SQLite the open transaction holds the database write
// lock, so the second writer serializes by construction.
func TestBUG3811_ConcurrentRemovalsKeepAnActiveAdmin(t *testing.T) {
	first := map[string]func(s *Store, tx *sql.Tx, id string) error{
		"demote":  func(s *Store, tx *sql.Tx, id string) error { return s.demoteGuardedTx(tx, id) },
		"disable": func(s *Store, tx *sql.Tx, id string) error { return s.disableGuardedTx(tx, id) },
	}
	second := map[string]func(s *Store, id string) error{
		"demote":      func(s *Store, id string) error { return s.SetUserRole(id, "member") },
		"disable":     func(s *Store, id string) error { return s.DisableUserAndRevokeAccess(id) },
		"delete-self": func(s *Store, id string) error { return s.DeleteAccountAtomic(id) },
	}
	for fn, hold := range first {
		for sn, run := range second {
			t.Run(fn+"/"+sn, func(t *testing.T) {
				s := testStore(t)
				if s.dialect.Driver() != DriverPostgres {
					t.Skip("Postgres only: SQLite's single writer serializes the two removals")
				}
				a, b := bug3811Admins(t, s)

				tx, err := s.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				if err := hold(s, tx, a); err != nil {
					t.Fatalf("hold A's %s: %v", fn, err)
				}

				done := make(chan error, 1)
				go func() { done <- run(s, b) }()

				// Commit A only once B is seen WAITING on the guard's lock, so
				// the pair is concurrent by observation, not by a timer: a B
				// that ran after the commit would refuse sequentially and pass
				// without the lock (codex r1 on #2178).
				var bErr error
				returnedEarly := false
				deadline := time.Now().Add(10 * time.Second)
				for !returnedEarly && !bug3811GuardHasWaiter(t, s.db) {
					if time.Now().After(deadline) {
						t.Fatalf("B's %s never waited on the active-admin lock while A's %s was held", sn, fn)
					}
					select {
					case bErr = <-done:
						returnedEarly = true
					case <-time.After(20 * time.Millisecond):
					}
				}
				if returnedEarly {
					t.Fatalf("B's %s returned before A's %s committed (err %v): it did not wait on the active-admin lock", sn, fn, bErr)
				}
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit A's %s: %v", fn, err)
				}
				select {
				case bErr = <-done:
				case <-time.After(10 * time.Second):
					t.Fatalf("B's %s never returned after A's %s committed", sn, fn)
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
}

// The sequential cases, on both drivers: each door refuses the last active
// admin outright.
func TestBUG3811_EachDoorRefusesTheLastActiveAdmin(t *testing.T) {
	doors := map[string]func(s *Store, id string) error{
		"demote":      func(s *Store, id string) error { return s.SetUserRole(id, "member") },
		"disable":     func(s *Store, id string) error { return s.DisableUserAndRevokeAccess(id) },
		"delete-self": func(s *Store, id string) error { return s.DeleteAccountAtomic(id) },
	}
	for name, door := range doors {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			a, b := bug3811Admins(t, s)
			if err := door(s, b); err != nil {
				t.Fatalf("removing B while A remains: %v", err)
			}
			if err := door(s, a); !errors.Is(err, ErrLastAdmin) {
				t.Errorf("removing the last active admin err = %v, want ErrLastAdmin", err)
			}
			if err := s.CheckActiveAdminLoss(a); !errors.Is(err, ErrLastAdmin) {
				t.Errorf("CheckActiveAdminLoss(last) = %v, want ErrLastAdmin", err)
			}
			if n := bug3811ActiveAdmins(t, s); n != 1 {
				t.Errorf("active admins = %d, want 1", n)
			}
		})
	}
}

// The behaviour change the PR states: a DISABLED admin is not the admin who
// remains, so the last active one cannot be demoted, disabled or deleted
// while a disabled admin exists. Before BUG-3811 the demote guard counted
// disabled admins.
func TestBUG3811_ADisabledAdminDoesNotCount(t *testing.T) {
	s := testStore(t)
	a, b := bug3811Admins(t, s)
	if err := s.DisableUserAndRevokeAccess(b); err != nil {
		t.Fatal(err)
	}
	if err := s.SetUserRole(a, "member"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("demote the last active admin beside a disabled one err = %v, want ErrLastAdmin", err)
	}
	if err := s.CheckActiveAdminLoss(a); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("CheckActiveAdminLoss = %v, want ErrLastAdmin", err)
	}
	// CONTROL: a non-admin, and a disabled admin, are never refused.
	m := createTestUser(t, s, "m@test.com", "M", "password123")
	if err := s.CheckActiveAdminLoss(m.ID); err != nil {
		t.Errorf("CheckActiveAdminLoss(member) = %v, want nil", err)
	}
	if err := s.SetUserRole(b, "member"); err != nil {
		t.Errorf("demote a disabled admin err = %v, want nil", err)
	}
}
