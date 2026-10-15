package store

import (
	"errors"
	"testing"
	"time"
)

// bug3816OAuthOnlyUser makes an account with google and github linked and no
// password: either provider may go, but not both.
func bug3816OAuthOnlyUser(t *testing.T, s *Store) string {
	t.Helper()
	u, err := s.CreateOAuthUser("oauth-only@example.com", "OAuth Only", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"google", "github"} {
		if err := s.AddOAuthProvider(u.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HasPassword() || len(got.GetOAuthProviders()) != 2 {
		t.Fatalf("instrument: want two providers and no password, got %v, password %v", got.GetOAuthProviders(), got.HasPassword())
	}
	return u.ID
}

// bug3816Providers reads the account's providers fresh.
func bug3816Providers(t *testing.T, s *Store, id string) []string {
	t.Helper()
	u, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	return u.GetOAuthProviders()
}

// Two unlinks of different providers on an account with no password: the
// second waits on the account row lock while the first is uncommitted, then
// must refuse rather than write an empty list (BUG-3816). Before the fix it
// filtered its provider out of the list the first had already shortened.
//
// Postgres only: on SQLite the open transaction holds the database write
// lock, so the second unlink cannot start until the first commits.
func TestBUG3816_ConcurrentUnlinksKeepASignInMethod(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("Postgres only: SQLite's single writer serializes the two unlinks")
	}
	id := bug3816OAuthOnlyUser(t, s)

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.removeOAuthProviderTx(tx, id, "google"); err != nil {
		t.Fatalf("hold the google unlink: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.RemoveOAuthProvider(id, "github") }()

	// Commit the first only once the second is seen waiting on a lock in the
	// account row's FOR NO KEY UPDATE read, so the pair is concurrent by
	// observation, not by a timer, and the wait is the unlink's own, not
	// some other query's (the class codex r1 found in BUG-3817's test).
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%FROM users WHERE id = $1 FOR NO KEY UPDATE%'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("the github unlink returned (err %v) before the google unlink committed: it did not wait on the account lock", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the github unlink never waited on the account lock")
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the google unlink: %v", err)
	}
	var second error
	select {
	case second = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the github unlink never returned after the google unlink committed")
	}

	if got := bug3816Providers(t, s, id); len(got) != 1 || got[0] != "github" {
		t.Errorf("providers = %v, want [github]: the account must keep a sign-in method", got)
	}
	if !errors.Is(second, ErrLastSignInMethod) {
		t.Errorf("the github unlink err = %v, want ErrLastSignInMethod", second)
	}
}

// The sequential cases, on both drivers.
func TestBUG3816_UnlinkRefusesOnlyTheLastSignInMethod(t *testing.T) {
	t.Run("last provider, no password", func(t *testing.T) {
		s := testStore(t)
		id := bug3816OAuthOnlyUser(t, s)
		if err := s.RemoveOAuthProvider(id, "google"); err != nil {
			t.Fatalf("first unlink: %v", err)
		}
		if err := s.RemoveOAuthProvider(id, "github"); !errors.Is(err, ErrLastSignInMethod) {
			t.Errorf("unlink of the last provider err = %v, want ErrLastSignInMethod", err)
		}
		if got := bug3816Providers(t, s, id); len(got) != 1 || got[0] != "github" {
			t.Errorf("providers = %v, want [github]", got)
		}
	})
	t.Run("last provider, password set", func(t *testing.T) {
		s := testStore(t)
		id := bug3816OAuthOnlyUser(t, s)
		if _, err := s.db.Exec(s.q(`UPDATE users SET password_set = ? WHERE id = ?`), true, id); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"google", "github"} {
			if err := s.RemoveOAuthProvider(id, p); err != nil {
				t.Fatalf("unlink %s with a password set: %v", p, err)
			}
		}
		if got := bug3816Providers(t, s, id); len(got) != 0 {
			t.Errorf("providers = %v, want none", got)
		}
	})
	t.Run("provider not linked", func(t *testing.T) {
		s := testStore(t)
		id := bug3816OAuthOnlyUser(t, s)
		if err := s.RemoveOAuthProvider(id, "google"); err != nil {
			t.Fatal(err)
		}
		// A repeat of an unlink that already landed (a double submit) stays
		// a no-op, even though only one method remains.
		if err := s.RemoveOAuthProvider(id, "google"); err != nil {
			t.Errorf("repeat unlink err = %v, want nil", err)
		}
		if got := bug3816Providers(t, s, id); len(got) != 1 || got[0] != "github" {
			t.Errorf("providers = %v, want [github]", got)
		}
	})
}
