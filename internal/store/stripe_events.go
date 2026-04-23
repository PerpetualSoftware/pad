package store

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// MarkStripeEventProcessed records a Stripe webhook event ID in
// stripe_processed_events if it hasn't been seen before. Returns true when the
// event was ALREADY recorded (i.e. the caller is seeing a duplicate Stripe
// retry and should skip re-processing). Returns false for first-time events
// (caller should run its handler logic).
//
// Implementation uses INSERT ... ON CONFLICT DO NOTHING and inspects the
// RowsAffected count: 1 means we inserted (new), 0 means the row was already
// present (duplicate). Both cases are success — only real DB errors propagate.
func (s *Store) MarkStripeEventProcessed(eventID string) (alreadyProcessed bool, err error) {
	if eventID == "" {
		return false, fmt.Errorf("mark stripe event: event_id is required")
	}

	res, err := s.db.Exec(
		s.q(`INSERT INTO stripe_processed_events (event_id, processed_at) VALUES (?, ?) ON CONFLICT (event_id) DO NOTHING`),
		eventID, now(),
	)
	if err != nil {
		return false, fmt.Errorf("mark stripe event %s: %w", eventID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		// Some drivers don't report RowsAffected reliably after ON CONFLICT;
		// in that case we can't tell whether this was the first insert. Treat
		// as "not already processed" and let the caller run its handler (the
		// event handler itself is idempotent at the application level — e.g.
		// set_plan is an UPSERT).
		return false, nil
	}

	// 1 row affected = we inserted a new row = first-time event.
	// 0 rows affected = ON CONFLICT DO NOTHING fired = duplicate.
	return n == 0, nil
}

// PruneStripeProcessedEvents deletes stripe_processed_events rows older than
// maxAge. Returns the number of rows removed. Intended to be called on a
// schedule (or opportunistically — see MarkStripeEventProcessed callers);
// Stripe retries events for up to 72h, so a 7-day retention gives a safe
// buffer while keeping the table bounded.
func (s *Store) PruneStripeProcessedEvents(maxAge time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-maxAge).Format(time.RFC3339)
	res, err := s.db.Exec(
		s.q(`DELETE FROM stripe_processed_events WHERE processed_at < ?`),
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("prune stripe_processed_events: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// ShouldPruneStripeEvents returns true with a ~1% chance, using crypto/rand.
// Used to opportunistically prune stripe_processed_events without a dedicated
// background goroutine — every ~100 handler invocations triggers a DELETE of
// expired rows, which is plenty frequent for low-volume webhook traffic.
// Exposed for the handler that decides whether to fire a cleanup.
func ShouldPruneStripeEvents() bool {
	var b [2]byte
	if _, err := rand.Read(b[:]); err != nil {
		return false
	}
	return binary.BigEndian.Uint16(b[:])%100 == 0
}
