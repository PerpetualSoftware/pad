package store

import "testing"

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
	if _, err := s.ClaimAccountByProvider(u.ID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	after, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.GetOAuthProviders()) != 0 {
		t.Errorf("providers survived the claim: %v", after.GetOAuthProviders())
	}
	if owner, _ := s.OAuthIdentityOwner("github", "gh-squatter"); owner != "" {
		t.Error("the previous holder's provider account is still bound")
	}
}
