package store

import (
	"errors"
	"testing"
	"time"
)

// BUG-3681: unlinking an OAuth provider must advance users.credential_epoch so
// an in-flight sign-in that read the account before the unlink cannot mint a
// session afterwards.

func TestRemoveOAuthProviderAdvancesCredentialEpoch(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3816OAuthOnlyUser(t, s)

	before, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}

	epoch, err := s.RemoveOAuthProvider(id, "github")
	if err != nil {
		t.Fatalf("RemoveOAuthProvider: %v", err)
	}
	if epoch != before.CredentialEpoch+1 {
		t.Errorf("returned epoch = %d, want %d", epoch, before.CredentialEpoch+1)
	}

	after, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.CredentialEpoch != before.CredentialEpoch+1 {
		t.Errorf("GetUser().CredentialEpoch = %d, want %d", after.CredentialEpoch, before.CredentialEpoch+1)
	}
}

func TestRemoveOAuthProviderFencesInFlightSignIn(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3816OAuthOnlyUser(t, s)

	before, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}

	epoch, err := s.RemoveOAuthProvider(id, "github")
	if err != nil {
		t.Fatalf("RemoveOAuthProvider: %v", err)
	}

	// A sign-in that read the account before the unlink carries the stale epoch
	// and must be refused.
	if tok, err := s.CreateSessionFenced(id, before.CredentialEpoch, "web", "192.0.2.1", "test-ua", time.Hour, time.Time{}, time.Time{}); !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("CreateSessionFenced(stale epoch %d) = (%q, %v), want ErrCredentialsChanged", before.CredentialEpoch, tok, err)
	}

	// Minting with the post-unlink epoch succeeds.
	tok, err := s.CreateSessionFenced(id, epoch, "web", "192.0.2.1", "test-ua", time.Hour, time.Time{}, time.Time{})
	if err != nil || tok == "" {
		t.Fatalf("CreateSessionFenced(new epoch %d) = (%q, %v), want non-empty token and nil err", epoch, tok, err)
	}
}

func TestRemoveOAuthProviderLastMethodDoesNotAdvanceEpoch(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	id := bug3816OAuthOnlyUser(t, s)

	if _, err := s.RemoveOAuthProvider(id, "google"); err != nil {
		t.Fatalf("first unlink: %v", err)
	}
	before, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}

	epoch, err := s.RemoveOAuthProvider(id, "github")
	if !errors.Is(err, ErrLastSignInMethod) {
		t.Fatalf("unlink last method err = %v, want ErrLastSignInMethod", err)
	}
	if epoch != 0 {
		t.Errorf("returned epoch on error = %d, want 0", epoch)
	}

	after, err := s.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.CredentialEpoch != before.CredentialEpoch {
		t.Errorf("CredentialEpoch after refused unlink = %d, want %d", after.CredentialEpoch, before.CredentialEpoch)
	}
}
