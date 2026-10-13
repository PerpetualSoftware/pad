package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3679: changing a password by any path invalidates every outstanding
// reset link for that user, as issuing a new link already does
// (CreatePasswordReset).

func TestBUG3679_PasswordChangeKillsOutstandingResetLink(t *testing.T) {
	cases := []struct {
		name   string
		fenced bool
	}{
		// Admin temp password and local reset write unfenced.
		{"unfenced", false},
		// The profile change writes fenced on the epoch the current
		// password was checked under.
		{"fenced", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			u := createUnverifiedUser(t, s, "bug3679-"+tc.name+"@example.com")

			link, err := s.CreatePasswordReset(u.ID)
			if err != nil {
				t.Fatal(err)
			}

			pwd := "owners-new-password-1"
			update := models.UserUpdate{Password: &pwd}
			if tc.fenced {
				cur, err := s.GetUser(u.ID)
				if err != nil {
					t.Fatal(err)
				}
				update.ExpectedEpoch = &cur.CredentialEpoch
			}
			if _, err := s.UpdateUser(u.ID, update); err != nil {
				t.Fatalf("password change: %v", err)
			}

			if got, err := s.LookupPasswordReset(link); err != nil || got != nil {
				t.Fatalf("lookup after password change: user=%v err=%v, want no user", got, err)
			}
			if got, err := s.ConsumePasswordReset(link); err != nil || got != nil {
				t.Fatalf("consume after password change: user=%v err=%v, want no user", got, err)
			}
		})
	}
}

// A write that changes no password leaves the link alone.
func TestBUG3679_NonPasswordUpdateKeepsResetLink(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "bug3679-name@example.com")
	link, err := s.CreatePasswordReset(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	name := "Renamed"
	if _, err := s.UpdateUser(u.ID, models.UserUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ConsumePasswordReset(link); err != nil || got == nil {
		t.Fatalf("consume after rename: user=%v err=%v, want the user", got, err)
	}
}

// A fenced write that loses its fence changes nothing, so it must not spend
// the link either.
func TestBUG3679_RefusedPasswordWriteKeepsResetLink(t *testing.T) {
	s := testStore(t)
	u := createUnverifiedUser(t, s, "bug3679-refused@example.com")
	link, err := s.CreatePasswordReset(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	cur, err := s.GetUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := cur.CredentialEpoch - 1
	pwd := "never-stored-password-1"
	if _, err := s.UpdateUser(u.ID, models.UserUpdate{Password: &pwd, ExpectedEpoch: &stale}); err == nil {
		t.Fatal("stale fenced write succeeded")
	}
	if got, err := s.ConsumePasswordReset(link); err != nil || got == nil {
		t.Fatalf("consume after refused write: user=%v err=%v, want the user", got, err)
	}
}
