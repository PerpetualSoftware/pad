package store

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// emailVerifiedBackfillSQL reads the ACTUAL backfill statement out of the
// embedded migration file (070 for SQLite, 048 for Postgres) so the backfill
// test exercises the real dialect-specific DML rather than a hand-copied
// duplicate. The ALTER TABLE line is dropped — the column already exists
// (migration ran at store open) and SQLite's ADD COLUMN would error on
// re-apply — leaving just the `UPDATE users SET email_verified_at ...` chunk.
func emailVerifiedBackfillSQL(t *testing.T) string {
	t.Helper()
	var (
		data []byte
		err  error
	)
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		data, err = pgMigrationsFS.ReadFile("pgmigrations/048_email_verified.sql")
	} else {
		data, err = migrationsFS.ReadFile("migrations/070_email_verified.sql")
	}
	if err != nil {
		t.Fatalf("read email_verified migration: %v", err)
	}
	// The backfill UPDATE is the FINAL statement in the file. Take everything
	// from the last "UPDATE users" through end-of-file so the test runs the real
	// DML without re-executing the ALTER (which SQLite rejects on re-apply).
	// Indexing from the LAST occurrence avoids tripping on the word appearing in
	// a preceding comment — and, unlike a naive split on ';', is immune to
	// semicolons inside comment prose.
	full := string(data)
	idx := strings.LastIndex(strings.ToUpper(full), "UPDATE USERS")
	if idx < 0 {
		t.Fatalf("no UPDATE users statement found in email_verified migration:\n%s", full)
	}
	return full[idx:]
}

// insertLegacyUnverifiedUser inserts a user row with email_verified_at = NULL,
// simulating an account that existed BEFORE migration 070 added + backfilled
// the column. CreateUser can't produce this state directly (its SAFE default
// is verified), so we go straight to SQL.
func insertLegacyUnverifiedUser(t *testing.T, s *Store, email string) string {
	t.Helper()
	id := newID()
	ts := now()
	if _, err := s.db.Exec(s.q(`
		INSERT INTO users (id, email, username, name, password_hash, role, email_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, NULL, ?, ?)
	`), id, email, "u_"+id, "Legacy "+id, "x", "member", ts, ts); err != nil {
		t.Fatalf("insert legacy unverified user: %v", err)
	}
	return id
}

// TestEmailVerifiedBackfill covers DR-3's UNCONDITIONAL backfill: every row
// that predates the column must come out VERIFIED so no existing / OAuth /
// self-host account is write-locked on deploy. Runs the real migration DML
// against pre-existing NULL rows (both dialects via make test-pg).
func TestEmailVerifiedBackfill(t *testing.T) {
	s := testStore(t)

	legacy := []string{
		insertLegacyUnverifiedUser(t, s, "legacy1@example.com"),
		insertLegacyUnverifiedUser(t, s, "legacy2@example.com"),
	}

	// Precondition: the legacy rows start unverified (NULL round-trips as "").
	for _, id := range legacy {
		u, err := s.GetUser(id)
		if err != nil {
			t.Fatalf("GetUser(%s): %v", id, err)
		}
		if u.IsEmailVerified() {
			t.Fatalf("precondition failed: legacy row %s should start unverified, got %q", id, u.EmailVerifiedAt)
		}
	}

	// Run the migration's backfill statement.
	if _, err := s.db.Exec(emailVerifiedBackfillSQL(t)); err != nil {
		t.Fatalf("run backfill DML: %v", err)
	}

	// Postcondition: every pre-existing row is now verified with a timestamp
	// that Go's time.Parse(time.RFC3339, …) can read back (the parse round-trip
	// is the whole point of the strftime/to_char 'Z'-suffixed formats).
	for _, id := range legacy {
		u, err := s.GetUser(id)
		if err != nil {
			t.Fatalf("GetUser(%s) after backfill: %v", id, err)
		}
		if !u.IsEmailVerified() {
			t.Errorf("backfill did not verify legacy row %s (EmailVerifiedAt=%q)", id, u.EmailVerifiedAt)
		}
		if _, err := time.Parse(time.RFC3339, u.EmailVerifiedAt); err != nil {
			t.Errorf("backfilled timestamp for %s is not RFC3339: %q (%v)", id, u.EmailVerifiedAt, err)
		}
	}
}

// TestCreateUserEmailVerifiedDefault covers DR-3's SAFE default: a plain
// CreateUser yields a VERIFIED user, and only an explicit Unverified request
// (the future cloud self-serve branch) produces an unverified one.
func TestCreateUserEmailVerifiedDefault(t *testing.T) {
	s := testStore(t)

	// Default: verified.
	u := createTestUser(t, s, "default@example.com", "Default", "password123")
	if !u.IsEmailVerified() {
		t.Errorf("CreateUser default should be VERIFIED (safe default), got EmailVerifiedAt=%q", u.EmailVerifiedAt)
	}
	if _, err := time.Parse(time.RFC3339, u.EmailVerifiedAt); err != nil {
		t.Errorf("default-verified timestamp not RFC3339: %q (%v)", u.EmailVerifiedAt, err)
	}

	// Explicit Unverified=true: unverified, and it must persist as NULL (not
	// just live on the returned struct).
	unv, err := s.CreateUser(models.UserCreate{
		Email: "unverified@example.com", Name: "Unv", Password: "password123", Unverified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser(Unverified): %v", err)
	}
	if unv.IsEmailVerified() {
		t.Errorf("CreateUser with Unverified=true should be UNVERIFIED, got EmailVerifiedAt=%q", unv.EmailVerifiedAt)
	}
	if got, _ := s.GetUser(unv.ID); got == nil || got.IsEmailVerified() {
		t.Errorf("unverified user should stay unverified after re-fetch, got %+v", got)
	}
}

// TestCreateOAuthUserVerified covers DR-3's "OAuth = verified" rule.
func TestCreateOAuthUserVerified(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateOAuthUser("oauth@example.com", "OAuth User", "https://example.com/a.png")
	if err != nil {
		t.Fatalf("CreateOAuthUser: %v", err)
	}
	if !u.IsEmailVerified() {
		t.Errorf("OAuth users must be verified, got EmailVerifiedAt=%q", u.EmailVerifiedAt)
	}
}

// TestEmailVerifiedBothScanPaths guards the two-scan-site foot-gun: the field
// must round-trip through scanUser (GetUser/ListUsers) AND the inline scan in
// SearchUsers. Missing the second breaks the admin user list at runtime with a
// column/target mismatch, which a compile check would NOT catch.
func TestEmailVerifiedBothScanPaths(t *testing.T) {
	s := testStore(t)

	verified := createTestUser(t, s, "scanverified@example.com", "V", "password123")
	unverifiedID := insertLegacyUnverifiedUser(t, s, "scanunverified@example.com")

	// --- scanUser path (GetUser) ---
	if got, err := s.GetUser(verified.ID); err != nil || got == nil || !got.IsEmailVerified() {
		t.Fatalf("scanUser (GetUser) verified: err=%v got=%+v", err, got)
	}
	if got, err := s.GetUser(unverifiedID); err != nil || got == nil || got.IsEmailVerified() {
		t.Fatalf("scanUser (GetUser) unverified: err=%v got=%+v", err, got)
	}

	// --- SearchUsers inline scan path ---
	res, err := s.SearchUsers(AdminUserSearchParams{})
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	byEmail := map[string]AdminUserListEntry{}
	for _, u := range res.Users {
		byEmail[u.Email] = u
	}
	if e, ok := byEmail["scanverified@example.com"]; !ok || !e.IsEmailVerified() {
		t.Errorf("SearchUsers inline scan: verified user missing/unverified (ok=%v EmailVerifiedAt=%q)", ok, e.EmailVerifiedAt)
	}
	if e, ok := byEmail["scanunverified@example.com"]; !ok || e.IsEmailVerified() {
		t.Errorf("SearchUsers inline scan: unverified user missing/verified (ok=%v EmailVerifiedAt=%q)", ok, e.EmailVerifiedAt)
	}
}

// TestListUsersEmailVerified is a lightweight extra assertion that the
// ListUsers scanUser path also surfaces the field (defensive; scanUser is
// shared, but ListUsers is the admin-facing bulk reader).
func TestListUsersEmailVerified(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		createTestUser(t, s, fmt.Sprintf("list%d@example.com", i), "L", "password123")
	}
	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("want 3 users, got %d", len(users))
	}
	for _, u := range users {
		if !u.IsEmailVerified() {
			t.Errorf("ListUsers: user %s should be verified, got EmailVerifiedAt=%q", u.Email, u.EmailVerifiedAt)
		}
	}
}
