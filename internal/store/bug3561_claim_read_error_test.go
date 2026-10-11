package store_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// Retro CONVE-735 finding on BUG-3561: after the insert conflicts and no
// takeover applies, ClaimStripeEvent reads the existing row. Only a row that
// is really gone (sql.ErrNoRows) may be claimed as new; any other read
// failure says nothing about the row (another handler may hold a live lease,
// or the event may be done), so it must fail instead of claiming.
func TestStripeEventClaimReadErrorIsNotAClaim(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)
			if st, _, err := s.ClaimStripeEvent("evt_held", store.StripeEventLease); err != nil || st != store.StripeEventClaimed {
				t.Fatalf("first claim: %q %v", st, err)
			}

			restore := s.SetStripeEventReadFailureHookForTesting(func(string) error { return errors.New("driver: bad connection") })
			st, _, err := s.ClaimStripeEvent("evt_held", store.StripeEventLease)
			restore()
			if err == nil || st == store.StripeEventClaimed {
				t.Fatalf("a failed read while a live lease is held must not claim: state %q err %v", st, err)
			}

			restore = s.SetStripeEventReadFailureHookForTesting(func(string) error { return sql.ErrNoRows })
			st, _, err = s.ClaimStripeEvent("evt_held", store.StripeEventLease)
			restore()
			if err != nil || st != store.StripeEventClaimed {
				t.Fatalf("a row that vanished is still claimed as new: state %q err %v", st, err)
			}

			if st, _, err := s.ClaimStripeEvent("evt_held", store.StripeEventLease); err != nil || st != store.StripeEventInProgress {
				t.Fatalf("with the hook gone the live lease reads in progress: %q %v", st, err)
			}
		})
	}
}
