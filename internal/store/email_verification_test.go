package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// createUnverifiedUser makes a user whose email starts UNVERIFIED, so a
// consume can be observed flipping it verified. CreateUser's safe default is
// verified, so we pass Unverified explicitly.
func createUnverifiedUser(t *testing.T, s *Store, email string) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{
		Email: email, Name: "Unv " + email, Password: "password123", Unverified: true,
	})
	if err != nil {
		t.Fatalf("CreateUser(unverified): %v", err)
	}
	if u.IsEmailVerified() {
		t.Fatalf("precondition: %s should start unverified, got %q", email, u.EmailVerifiedAt)
	}
	return u
}

// insertVerificationToken writes a token row directly with a caller-chosen
// expires_at / used_at so tests can craft expired and already-used rows.
func insertVerificationToken(t *testing.T, s *Store, userID, plaintext, expiresAt string, usedAt *string) {
	t.Helper()
	hash := sha256.Sum256([]byte(plaintext))
	tokenHash := hex.EncodeToString(hash[:])
	var used any
	if usedAt != nil {
		used = *usedAt
	}
	if _, err := s.db.Exec(s.q(`
		INSERT INTO email_verification_tokens (id, user_id, token_hash, expires_at, used_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`), newID(), userID, tokenHash, expiresAt, used, now()); err != nil {
		t.Fatalf("insert verification token: %v", err)
	}
}

func countVerificationTokens(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM email_verification_tokens`)).Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	return n
}

// TestEmailVerification_CreateLookupConsume covers the happy path: mint →
// non-destructive lookup → consume flips users.email_verified_at → single-use.
func TestEmailVerification_CreateLookupConsume(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "verify@example.com")

	token, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatalf("CreateEmailVerification: %v", err)
	}
	if !strings.HasPrefix(token, "padver_") {
		t.Errorf("token missing padver_ prefix: %q", token)
	}

	// Lookup is non-destructive: it returns the user but must NOT consume.
	got, err := s.LookupEmailVerification(token)
	if err != nil {
		t.Fatalf("LookupEmailVerification: %v", err)
	}
	if got == nil || got.ID != u.ID {
		t.Fatalf("Lookup returned wrong/nil user: %+v", got)
	}
	if got.IsEmailVerified() {
		t.Errorf("Lookup must not verify the user")
	}
	// A second lookup still works — proof the first didn't consume.
	if again, _ := s.LookupEmailVerification(token); again == nil {
		t.Errorf("second Lookup should still succeed (non-destructive)")
	}

	// Consume flips email_verified_at and returns the now-verified user.
	consumed, err := s.ConsumeEmailVerification(token)
	if err != nil {
		t.Fatalf("ConsumeEmailVerification: %v", err)
	}
	if consumed == nil || consumed.ID != u.ID {
		t.Fatalf("Consume returned wrong/nil user: %+v", consumed)
	}
	if !consumed.IsEmailVerified() {
		t.Errorf("Consume should mark the returned user verified, got %q", consumed.EmailVerifiedAt)
	}
	if _, err := time.Parse(time.RFC3339, consumed.EmailVerifiedAt); err != nil {
		t.Errorf("email_verified_at not RFC3339: %q (%v)", consumed.EmailVerifiedAt, err)
	}
	// Persisted (not just on the returned struct).
	if refetched, _ := s.GetUser(u.ID); refetched == nil || !refetched.IsEmailVerified() {
		t.Errorf("verification did not persist: %+v", refetched)
	}

	// Single-use: consuming again returns nil (no user), and lookup fails too.
	if reused, err := s.ConsumeEmailVerification(token); err != nil || reused != nil {
		t.Errorf("second Consume should return (nil, nil), got (%+v, %v)", reused, err)
	}
	if look, _ := s.LookupEmailVerification(token); look != nil {
		t.Errorf("Lookup of a used token should return nil, got %+v", look)
	}
}

// TestEmailVerification_ExpiredRejected: an expired token validates as nil and
// cannot be consumed, and its consume leaves the user unverified.
func TestEmailVerification_ExpiredRejected(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "expired@example.com")

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	insertVerificationToken(t, s, u.ID, "padver_expiredtoken", past, nil)

	if got, err := s.LookupEmailVerification("padver_expiredtoken"); err != nil || got != nil {
		t.Errorf("expired token Lookup should be (nil, nil), got (%+v, %v)", got, err)
	}
	if got, err := s.ConsumeEmailVerification("padver_expiredtoken"); err != nil || got != nil {
		t.Errorf("expired token Consume should be (nil, nil), got (%+v, %v)", got, err)
	}
	if refetched, _ := s.GetUser(u.ID); refetched == nil || refetched.IsEmailVerified() {
		t.Errorf("expired-token consume must not verify the user, got %+v", refetched)
	}
}

// TestEmailVerification_ResendInvalidatesPrior: minting a new token invalidates
// the previous unused one (so resend-verification burns the old link).
func TestEmailVerification_ResendInvalidatesPrior(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "resend@example.com")

	first, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatalf("CreateEmailVerification (first): %v", err)
	}
	second, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatalf("CreateEmailVerification (second): %v", err)
	}
	if first == second {
		t.Fatalf("resend produced an identical token")
	}

	// The first token is now invalidated.
	if got, _ := s.LookupEmailVerification(first); got != nil {
		t.Errorf("first token should be invalidated after resend, got %+v", got)
	}
	if got, _ := s.ConsumeEmailVerification(first); got != nil {
		t.Errorf("first token should not be consumable after resend, got %+v", got)
	}
	// The second (current) token still works.
	if got, _ := s.ConsumeEmailVerification(second); got == nil || !got.IsEmailVerified() {
		t.Errorf("current token should consume + verify, got %+v", got)
	}
}

// TestEmailVerification_HashAtRest: the plaintext is never stored — the column
// holds the SHA-256 hex of the plaintext.
func TestEmailVerification_HashAtRest(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "hash@example.com")

	token, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatalf("CreateEmailVerification: %v", err)
	}

	var stored string
	if err := s.db.QueryRow(s.q(`SELECT token_hash FROM email_verification_tokens WHERE user_id = ? AND used_at IS NULL`), u.ID).Scan(&stored); err != nil {
		t.Fatalf("read token_hash: %v", err)
	}
	if stored == token {
		t.Fatalf("plaintext token stored at rest")
	}
	if strings.Contains(stored, "padver_") {
		t.Errorf("stored hash contains the plaintext prefix: %q", stored)
	}
	want := sha256.Sum256([]byte(token))
	if stored != hex.EncodeToString(want[:]) {
		t.Errorf("stored hash mismatch: got %q want %q", stored, hex.EncodeToString(want[:]))
	}
}

// TestCleanExpiredEmailVerifications: the reaper's store method deletes expired
// AND used rows but keeps live ones. Exercised on both dialects via test-pg.
func TestCleanExpiredEmailVerifications(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "clean@example.com")

	past := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	usedTs := now()

	insertVerificationToken(t, s, u.ID, "padver_expired", past, nil)    // expired
	insertVerificationToken(t, s, u.ID, "padver_used", future, &usedTs) // used but not expired
	insertVerificationToken(t, s, u.ID, "padver_live", future, nil)     // live

	if got := countVerificationTokens(t, s); got != 3 {
		t.Fatalf("precondition: want 3 rows, got %d", got)
	}

	if err := s.CleanExpiredEmailVerifications(); err != nil {
		t.Fatalf("CleanExpiredEmailVerifications: %v", err)
	}

	if got := countVerificationTokens(t, s); got != 1 {
		t.Errorf("after clean: want 1 live row, got %d", got)
	}
	// The surviving row is the live one.
	if got, _ := s.LookupEmailVerification("padver_live"); got == nil {
		t.Errorf("live token should survive the reaper sweep")
	}
}
