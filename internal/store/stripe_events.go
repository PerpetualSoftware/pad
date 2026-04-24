package store

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// MarkStripeEventProcessed records a Stripe webhook event ID in
// stripe_processed_events if it hasn't been seen before. Returns
// alreadyProcessed=true when the event was ALREADY recorded (i.e. the caller
// is seeing a duplicate Stripe retry and should skip re-processing). Returns
// false for first-time events (caller should run its handler logic).
//
// Always returns processedAt — either the timestamp we just inserted (on a
// fresh mark) or the existing row's timestamp (on a duplicate). Sidecars
// MUST persist this string and pass it back to UnmarkStripeEventProcessed if
// a best-effort rollback is later needed (TASK-736): the unmark uses
// (event_id, processed_at) as a composite key, so a stale unmark from an
// earlier attempt can't delete the fresh marker left by a successful retry.
//
// Implementation uses INSERT ... ON CONFLICT DO NOTHING. On fresh insert we
// return the wall-clock we just wrote; on conflict we SELECT the existing
// row to recover its timestamp. Both cases are success — only real DB
// errors propagate.
func (s *Store) MarkStripeEventProcessed(eventID string) (alreadyProcessed bool, processedAt string, err error) {
	if eventID == "" {
		return false, "", fmt.Errorf("mark stripe event: event_id is required")
	}

	ts := now()
	res, err := s.db.Exec(
		s.q(`INSERT INTO stripe_processed_events (event_id, processed_at) VALUES (?, ?) ON CONFLICT (event_id) DO NOTHING`),
		eventID, ts,
	)
	if err != nil {
		return false, "", fmt.Errorf("mark stripe event %s: %w", eventID, err)
	}

	n, rowsErr := res.RowsAffected()
	if rowsErr == nil && n == 1 {
		// Fresh insert — ts is authoritative and the unmark token.
		return false, ts, nil
	}

	// Conflict (or driver couldn't report RowsAffected). Recover the
	// existing row's processed_at so the caller has a token matching the
	// row currently in the table — without this a retried-and-successful
	// handler has no way for its future unmark call to target the right
	// marker.
	var existing string
	selErr := s.db.QueryRow(
		s.q(`SELECT processed_at FROM stripe_processed_events WHERE event_id = ?`),
		eventID,
	).Scan(&existing)
	if selErr != nil {
		// The row really isn't there (possible under a driver that didn't
		// report RowsAffected AND a concurrent delete) — treat as fresh
		// so the handler can still run. processedAt=ts may not match the
		// actual DB row in this unusual case; that's acceptable because
		// the sidecar handler is idempotent at the application level (set
		// plan is an UPSERT, webhook handlers are defensively safe).
		return false, ts, nil
	}
	return true, existing, nil
}

// UnmarkStripeEventProcessed deletes a row from stripe_processed_events,
// allowing Stripe retries of the same event ID to be handled again (TASK-736).
// Used by the sidecar as a best-effort rollback when the webhook handler
// fails AFTER MarkStripeEventProcessed succeeded.
//
// Race protection: the delete is scoped to the specific (event_id,
// processed_at) pair the caller was handed by MarkStripeEventProcessed.
// A delayed unmark from an earlier failed attempt can NOT delete a fresh
// marker left by a successful retry, because the retry wrote a new
// processed_at timestamp that no longer matches the stale token — so the
// unmark becomes a safe no-op instead of reopening the retry window
// after the event has already been handled. The sidecar must therefore
// persist the processed_at it received from Mark and pass it here.
//
// Returns true when a row was actually deleted, false when nothing
// matched (missing row OR a timestamp mismatch — both safe as no-ops).
// Only real DB failures propagate.
func (s *Store) UnmarkStripeEventProcessed(eventID, processedAt string) (unmarked bool, err error) {
	if eventID == "" {
		return false, fmt.Errorf("unmark stripe event: event_id is required")
	}
	if processedAt == "" {
		// Refuse to delete without the processed_at token. Otherwise a
		// caller that misplaces the token would fall back to deleting by
		// event_id alone, defeating the race-protection contract.
		return false, fmt.Errorf("unmark stripe event: processed_at is required (race protection)")
	}

	res, err := s.db.Exec(
		s.q(`DELETE FROM stripe_processed_events WHERE event_id = ? AND processed_at = ?`),
		eventID, processedAt,
	)
	if err != nil {
		return false, fmt.Errorf("unmark stripe event %s: %w", eventID, err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		// Drivers that don't report RowsAffected reliably — treat as
		// success without asserting whether we actually deleted anything.
		return false, nil
	}
	return n > 0, nil
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
