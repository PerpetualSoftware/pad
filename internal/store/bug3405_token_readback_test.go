package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3405: CreateAPIToken committed, then read the row back through the pool
// and dereferenced it. A deletion landing between the two (the token's user's
// account deletion, a revoke) made the read answer nil, and the mint panicked.
// The hook deletes the row at exactly that point, so the race is
// deterministic: the mint must answer the token it committed, or a clean
// error, never panic.
func TestBug3405_MintSurvivesADeletionAfterItsCommit(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "bug3405@example.com", "Mint", "correct-horse-battery")
	createAPITokenAfterCommitHook = func(id string) {
		if _, err := s.db.Exec(s.q(`DELETE FROM api_tokens WHERE id = ?`), id); err != nil {
			t.Errorf("hook delete: %v", err)
		}
	}
	t.Cleanup(func() { createAPITokenAfterCommitHook = nil })

	var tok *models.APITokenWithSecret
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("CreateAPIToken panicked when its row was deleted after the commit: %v", r)
			}
		}()
		tok, err = s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "racing"}, 30, 365)
	}()
	if err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}
	if tok == nil || tok.ID == "" || tok.Token == "" || tok.UserID != u.ID || tok.Name != "racing" {
		t.Fatalf("the returned token does not describe what was committed: %+v", tok)
	}
}

// The rotation's twin: a revoke landing after the rotation commits must not
// make it panic; it answers what it committed.
func TestBug3405_RotationSurvivesADeletionAfterItsCommit(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "bug3405r@example.com", "Rotate", "correct-horse-battery")
	tok, err := s.CreateAPIToken(u.ID, models.APITokenCreate{Name: "rotating"}, 30, 365)
	if err != nil {
		t.Fatal(err)
	}
	rotateAPITokenAfterCommitHook = func(id string) {
		if _, err := s.db.Exec(s.q(`DELETE FROM api_tokens WHERE id = ?`), id); err != nil {
			t.Errorf("hook delete: %v", err)
		}
	}
	t.Cleanup(func() { rotateAPITokenAfterCommitHook = nil })
	var got *models.APITokenWithSecret
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("RotateAPIToken panicked when its row was deleted after the commit: %v", r)
			}
		}()
		got, err = s.RotateAPIToken(tok.ID, u.ID, 0, 0)
	}()
	if err != nil {
		t.Fatalf("RotateAPIToken: %v", err)
	}
	if got == nil || got.ID != tok.ID || got.Token == "" || got.Token == tok.Token {
		t.Fatalf("rotation result %+v", got)
	}
}

// A rotation of a token that is gone (revoked between the caller's read and
// the update) is the clean not-found every other missing token gets.
func TestBug3405_RotatingAGoneTokenIsNotFound(t *testing.T) {
	s := testStore(t)
	u := createTestUser(t, s, "bug3405g@example.com", "Gone", "correct-horse-battery")
	if _, err := s.RotateAPIToken("no-such-token", u.ID, 0, 0); err == nil {
		t.Fatal("rotating a missing token succeeded")
	}
}
