package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// TASK-3351: a claim unlinks every sign-in provider the account held and
// forgets their bound provider accounts. They were linked by whoever held the
// account before; the claimant links their own.
func TestTASK3351_ClaimUnlinksProvidersAndForgetsSubjects(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "linked@example.com")
	// An unverified account cannot link one through the API (BUG-3348), but
	// rows written before that, or by an operator, can say so.
	if err := s.AddOAuthProvider(u.ID, "github"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindOAuthIdentity(u.ID, "github", "gh-squatter"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAccountByProvider(u.ID, "google", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	after, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.GetOAuthProviders(); len(got) != 1 || got[0] != "google" {
		t.Errorf("providers after the claim: %v, want only the claimant's google", got)
	}
	if owner, _ := s.OAuthIdentityOwner("github", "gh-squatter"); owner != "" {
		t.Error("the previous holder's provider account is still bound")
	}
}

// A claim whose provider account is already bound to someone else is
// refused WHOLE: the account it would have claimed is unchanged (codex
// round 1, TASK-3351).
func TestTASK3351_ClaimRefusedOnSubjectChangesNothing(t *testing.T) {
	s := testStore(t)
	other := createUnverifiedUser(t, s, "other@example.com")
	if err := s.LinkOAuthProvider(other.ID, "google", "g-taken"); err != nil {
		t.Fatal(err)
	}
	u := createUnverifiedUser(t, s, "squat@example.com")
	sess, err := s.CreateSession(u.ID, "web", "", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAccountByProvider(u.ID, "google", "g-taken"); !errors.Is(err, ErrOAuthSubjectMismatch) {
		t.Fatalf("claim with a subject bound elsewhere: %v", err)
	}
	after, _ := s.GetUser(u.ID)
	if after.IsEmailVerified() || after.CredentialEpoch != u.CredentialEpoch || len(after.GetOAuthProviders()) != 0 {
		t.Errorf("the refused claim changed the account: verified=%v epoch=%d providers=%v",
			after.IsEmailVerified(), after.CredentialEpoch, after.GetOAuthProviders())
	}
	if got, _ := s.ValidateSession(sess); got == nil {
		t.Error("the refused claim deleted the account's sessions")
	}
}

// A sign-in that read the link before an unlink committed cannot bind its
// provider account afterwards (codex round 1, TASK-3351).
func TestTASK3351_BindAfterUnlinkBindsNothing(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "linked2@example.com")
	if err := s.AddOAuthProvider(u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveOAuthProvider(u.ID, "google"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindOAuthIdentity(u.ID, "google", "g-stale"); !errors.Is(err, ErrOAuthProviderNotLinked) {
		t.Fatalf("bind after unlink: %v", err)
	}
	if owner, _ := s.OAuthIdentityOwner("google", "g-stale"); owner != "" {
		t.Error("a stale sign-in bound its subject after the unlink")
	}
}

// A provider claim and a verification consume racing on one account take
// their locks in the same order, so neither fails with a deadlock (codex
// round 1, TASK-3351). The consume starts while the claim holds the account
// lock: with the old order the claim then waited on the token the consume
// held, and Postgres aborted one of them.
func TestTASK3351_ClaimAndVerifyDoNotDeadlock(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "race@example.com")
	tok, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var verifyErr error
	claimAfterUserLockHook = func() {
		claimAfterUserLockHook = nil
		wg.Add(1)
		go func() { defer wg.Done(); _, verifyErr = s.ConsumeEmailVerification(tok) }()
		time.Sleep(300 * time.Millisecond) // let the consume reach its lock
	}
	t.Cleanup(func() { claimAfterUserLockHook = nil })
	_, claimErr := s.ClaimAccountByProvider(u.ID, "google", "")
	wg.Wait()
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	if verifyErr != nil {
		t.Fatalf("verify: %v", verifyErr)
	}
}
