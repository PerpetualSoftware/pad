package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3382: a password write is fenced on the credential_epoch its authority
// was checked under, so one that started before a claim cannot overwrite the
// claim's reset afterwards.

func TestBUG3382_ResetSpentBeforeAClaimWritesNothingAfterIt(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "squat@example.com")

	resetTok, err := s.CreatePasswordReset(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	spent, err := s.ConsumePasswordReset(resetTok)
	if err != nil || spent == nil {
		t.Fatalf("consume reset: %v %v", spent, err)
	}

	// The mailbox owner claims between the reset's spend and its write.
	vTok, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ClaimAccountByVerification(vTok); err != nil {
		t.Fatalf("claim: %v", err)
	}

	pwd := "squatters-new-password-1"
	_, err = s.UpdateUser(u.ID, models.UserUpdate{Password: &pwd, ExpectedEpoch: &spent.CredentialEpoch})
	if !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("stale reset write: got %v, want ErrCredentialsChanged", err)
	}
	if got, _ := s.ValidatePassword(u.Email, pwd); got != nil {
		t.Fatal("the stale reset's password was stored over the claim")
	}
}

func TestBUG3382_FencedPasswordWriteReturnsItsOwnEpoch(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "own@example.com")

	resetTok, err := s.CreatePasswordReset(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	spent, err := s.ConsumePasswordReset(resetTok)
	if err != nil || spent == nil {
		t.Fatalf("consume reset: %v %v", spent, err)
	}
	pwd := "a-fresh-password-2"
	updated, err := s.UpdateUser(u.ID, models.UserUpdate{Password: &pwd, ExpectedEpoch: &spent.CredentialEpoch})
	if err != nil {
		t.Fatalf("fenced write: %v", err)
	}
	if updated.CredentialEpoch != spent.CredentialEpoch+1 {
		t.Fatalf("epoch = %d, want %d", updated.CredentialEpoch, spent.CredentialEpoch+1)
	}
	if got, _ := s.ValidatePassword(u.Email, pwd); got == nil {
		t.Fatal("the fenced write did not store the password")
	}
}

// The claim lands INSIDE the spend, after the token is marked used and before
// the user is read back: the epoch the reset is fenced on must be the one from
// the spend, not the claim's.
func TestBUG3382_ClaimInsideTheResetSpendStillFencesIt(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "inside@example.com")
	resetTok, err := s.CreatePasswordReset(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	vTok, err := s.CreateEmailVerification(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed := false
	passwordResetSpentHook = func(string) {
		if _, err := s.ClaimAccountByVerification(vTok); err != nil {
			t.Errorf("claim: %v", err)
			return
		}
		claimed = true
	}
	t.Cleanup(func() { passwordResetSpentHook = nil })

	spent, err := s.ConsumePasswordReset(resetTok)
	if err != nil || spent == nil {
		t.Fatalf("consume reset: %v %v", spent, err)
	}
	if !claimed {
		t.Fatal("control: the claim did not run inside the spend")
	}
	pwd := "squatters-new-password-3"
	_, err = s.UpdateUser(u.ID, models.UserUpdate{Password: &pwd, ExpectedEpoch: &spent.CredentialEpoch})
	if !errors.Is(err, ErrCredentialsChanged) {
		t.Fatalf("reset spent before the claim: got %v, want ErrCredentialsChanged", err)
	}
}
