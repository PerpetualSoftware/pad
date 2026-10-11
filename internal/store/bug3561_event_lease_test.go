package store_test

import (
	"sync"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3561: the processed-event marker is a claim with a lease, not a final
// mark. A handler that dies after its claim no longer loses the event: the
// next delivery after the lease takes it over. Both dialects, because the
// takeover is a conditional UPDATE racing others and the lease compares as
// text.
func TestStripeEventClaimLease(t *testing.T) {
	for _, b := range contentBackends() {
		t.Run(b.name, func(t *testing.T) {
			s := b.open(t)

			// A new event is claimed; a second delivery inside the lease is in
			// progress (which an old sidecar still reads as "already processed").
			st, tok, err := s.ClaimStripeEvent("evt_live", store.StripeEventLease)
			if err != nil || st != store.StripeEventClaimed || tok == "" {
				t.Fatalf("first claim: %q %q %v", st, tok, err)
			}
			if st, _, _ := s.ClaimStripeEvent("evt_live", store.StripeEventLease); st != store.StripeEventInProgress {
				t.Fatalf("a delivery inside the lease = %q, want in_progress", st)
			}
			if already, _, _ := s.MarkStripeEventProcessed("evt_live"); !already {
				t.Fatal("the old-sidecar answer for an event in progress must be already_processed")
			}

			// Completed, it is a duplicate.
			if ok, err := s.CompleteStripeEvent("evt_live", tok); err != nil || !ok {
				t.Fatalf("complete: %v %v", ok, err)
			}
			if st, _, _ := s.ClaimStripeEvent("evt_live", store.StripeEventLease); st != store.StripeEventDone {
				t.Fatalf("after complete = %q, want done", st)
			}

			// THE BUG: a handler claims, then dies (no complete, no unmark). The
			// lease runs out, so the next delivery takes the event over, once,
			// with a new token, even with several deliveries racing.
			_, deadTok, err := s.ClaimStripeEvent("evt_crash", -time.Minute) // a lease already expired
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(1100 * time.Millisecond) // a new second, so the takeover's token differs
			var wg sync.WaitGroup
			results := make(chan string, 4)
			for i := 0; i < 4; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					st, _, err := s.ClaimStripeEvent("evt_crash", store.StripeEventLease)
					if err != nil {
						t.Error(err)
					}
					results <- st
				}()
			}
			wg.Wait()
			close(results)
			claimed := 0
			for st := range results {
				if st == store.StripeEventClaimed {
					claimed++
				}
			}
			if claimed != 1 {
				t.Fatalf("racing deliveries after an expired lease: %d claimed, want exactly 1", claimed)
			}

			// The dead handler's late complete holds the old token: a no-op,
			// so it cannot complete the event for the handler that took over.
			if ok, err := s.CompleteStripeEvent("evt_crash", deadTok); err != nil || ok {
				t.Fatalf("a stale-token complete changed the row: %v %v", ok, err)
			}
			if st, _, _ := s.ClaimStripeEvent("evt_crash", store.StripeEventLease); st != store.StripeEventInProgress {
				t.Fatalf("after a stale complete the event must still be in progress under the new lease; got %q", st)
			}
		})
	}
}
