package store

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
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
	if _, err := s.ClaimAccountByProvider(u.ID, "google", "", ""); err != nil {
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
	if _, err := s.ClaimAccountByProvider(u.ID, "google", "g-taken", ""); !errors.Is(err, ErrOAuthSubjectMismatch) {
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
	_, claimErr := s.ClaimAccountByProvider(u.ID, "google", "", "")
	wg.Wait()
	if claimErr != nil {
		t.Fatalf("claim: %v", claimErr)
	}
	if verifyErr != nil {
		t.Fatalf("verify: %v", verifyErr)
	}
}

// Account deletion takes the account, then its tokens. A verification
// consume and an account claim that start while it holds the account must
// wait for it, not hold a token it needs (codex round 2, TASK-3351): every
// path now locks the account before its tokens.
func TestTASK3351_DeletionClaimAndVerifyDoNotDeadlock(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "gone@example.com")
	tok, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var verifyErr, claimErr error
	deleteAccountAfterUserLockHook = func() {
		deleteAccountAfterUserLockHook = nil
		wg.Add(2)
		go func() { defer wg.Done(); _, verifyErr = s.ConsumeEmailVerification(tok) }()
		go func() { defer wg.Done(); _, claimErr = s.ClaimAccountByProvider(u.ID, "google", "", "") }()
		time.Sleep(300 * time.Millisecond) // let both reach their locks
	}
	t.Cleanup(func() { deleteAccountAfterUserLockHook = nil })
	if err := s.DeleteAccountAtomic(u.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	wg.Wait()
	if verifyErr != nil {
		t.Fatalf("verify: %v", verifyErr)
	}
	if claimErr != nil && !errors.Is(claimErr, ErrClaimNotEligible) {
		t.Fatalf("claim: %v", claimErr)
	}
}

// A link claim that waits for the account lock past its token's expiry
// claims nothing (codex round 3, TASK-3351): expiry is checked when the
// token is spent, not when it was found.
func TestTASK3351_LinkClaimChecksExpiryAfterTheWait(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "late@example.com")
	expires := time.Now().UTC().Add(time.Second).Format(time.RFC3339)
	insertVerificationToken(t, s, u.ID, "padver_late", expires, nil)
	claimAfterUserLockHook = func() {
		claimAfterUserLockHook = nil
		time.Sleep(2100 * time.Millisecond) // the token expires while the claim waits
	}
	t.Cleanup(func() { claimAfterUserLockHook = nil })
	if _, err := s.ClaimAccountByVerification("padver_late"); !errors.Is(err, ErrClaimNotEligible) {
		t.Fatalf("claim after expiry: %v, want ErrClaimNotEligible", err)
	}
	if after, _ := s.GetUser(u.ID); after.IsEmailVerified() {
		t.Error("an expired token claimed the account")
	}
}

// A claim resets the identity the registrant chose (lead, on #1760): the
// username and display name can impersonate ("support", a staff name), and
// nothing live depends on them, since the claim deleted the workspaces the
// account owned. The username is regenerated from the claimant's name, else
// the address's local part, and stays unique.
func TestTASK3351_ClaimResetsTheRegistrantsIdentity(t *testing.T) {
	s := testStore(t)
	// Someone already holds the name the claimant's would generate.
	if _, err := s.CreateUser(models.UserCreate{Email: "taken@example.com", Name: "Real Owner", Username: "real-owner", Password: "password123"}); err != nil {
		t.Fatal(err)
	}
	squat := func(email string) *models.User {
		u, err := s.CreateUser(models.UserCreate{Email: email, Name: "Pad Support", Username: "support-" + email[:2], Password: "password123", Unverified: true})
		if err != nil {
			t.Fatal(err)
		}
		return u
	}

	// Provider claim: the provider's name.
	u := squat("social@example.com")
	if _, err := s.ClaimAccountByProvider(u.ID, "google", "", "Real Owner"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.GetUser(u.ID)
	if after.Name != "Real Owner" || after.Username != "real-owner-2" {
		t.Errorf("provider claim: name=%q username=%q, want Real Owner / real-owner-2", after.Name, after.Username)
	}

	// Link claim: no name to go on, so the address's local part.
	v := squat("mailbox@example.com")
	tok, err := s.CreateEmailVerification(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAccountByVerification(tok); err != nil {
		t.Fatal(err)
	}
	after, _ = s.GetUser(v.ID)
	if after.Name != "mailbox" || after.Username != "mailbox" {
		t.Errorf("link claim: name=%q username=%q, want mailbox / mailbox", after.Name, after.Username)
	}
}
