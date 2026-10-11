package store

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"time"
)

// MarkStripeEventProcessed is ClaimStripeEvent with the pre-BUG-3561 answer:
// alreadyProcessed is false only for a claim, and true for a done event and for
// one in progress under another lease, which is what a sidecar that predates
// the lease must keep seeing. processedAt is the claim's token (or the stored
// one); pass it to CompleteStripeEvent, or to UnmarkStripeEventProcessed on a
// handler failure (TASK-736).
func (s *Store) MarkStripeEventProcessed(eventID string) (alreadyProcessed bool, processedAt string, err error) {
	state, processedAt, err := s.ClaimStripeEvent(eventID, StripeEventLease)
	return state != StripeEventClaimed, processedAt, err
}

// StripeEventLease is how long a claimed event belongs to the sidecar that
// claimed it (BUG-3561). Well above a handler's worst case (the sidecar bounds
// each handler with a shorter deadline) and well below Stripe's retry spacing.
const StripeEventLease = 5 * time.Minute

// Outcomes of ClaimStripeEvent.
const (
	// StripeEventClaimed: run the handler, then CompleteStripeEvent with the token.
	StripeEventClaimed = "claimed"
	// StripeEventDone: a handler already succeeded; acknowledge the duplicate.
	StripeEventDone = "done"
	// StripeEventInProgress: another handler holds a live lease. Do not run;
	// fail the delivery so Stripe retries after the lease.
	StripeEventInProgress = "in_progress"
)

// ClaimStripeEvent takes a Stripe event for handling (BUG-3561). The marker
// used to be final the moment it was written, BEFORE the handler ran, so a
// sidecar that died mid-handler (or whose unmark failed) left the event
// "processed" forever and every retry was acknowledged as a duplicate. Now:
//
//   - a new event is inserted 'processing' with a lease, and is claimed;
//   - a 'done' event is a duplicate;
//   - a 'processing' event whose lease has EXPIRED is taken over by one
//     conditional UPDATE (a new token and a new lease), so of two concurrent
//     retries exactly one wins, and the earlier handler's late complete or
//     unmark, holding the old token, changes nothing;
//   - a 'processing' event with a live lease is in progress elsewhere.
//
// processedAt is the token: CompleteStripeEvent and UnmarkStripeEventProcessed
// act only on the row still carrying it. Timestamps are UTC RFC3339 at second
// resolution, a fixed width, so the lease compares lexically on both dialects.
func (s *Store) ClaimStripeEvent(eventID string, lease time.Duration) (state, processedAt string, err error) {
	if eventID == "" {
		return "", "", fmt.Errorf("claim stripe event: event_id is required")
	}
	t := time.Now().UTC()
	ts := t.Format(time.RFC3339)
	until := t.Add(lease).Format(time.RFC3339)

	res, err := s.db.Exec(
		s.q(`INSERT INTO stripe_processed_events (event_id, processed_at, state, lease_until) VALUES (?, ?, 'processing', ?) ON CONFLICT (event_id) DO NOTHING`),
		eventID, ts, until,
	)
	if err != nil {
		return "", "", fmt.Errorf("claim stripe event %s: %w", eventID, err)
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n == 1 {
		return StripeEventClaimed, ts, nil
	}

	// Take over an expired lease. The token must change, or the earlier
	// handler could still complete or unmark the row it no longer owns; a
	// takeover inside the same second as the stored token is impossible,
	// since the lease is minutes long.
	res, err = s.db.Exec(
		s.q(`UPDATE stripe_processed_events SET processed_at = ?, lease_until = ? WHERE event_id = ? AND state = 'processing' AND (lease_until IS NULL OR lease_until < ?)`),
		ts, until, eventID, ts,
	)
	if err != nil {
		return "", "", fmt.Errorf("take over stripe event %s: %w", eventID, err)
	}
	if n, rerr := res.RowsAffected(); rerr == nil && n == 1 {
		return StripeEventClaimed, ts, nil
	}

	var st, existing string
	if err := s.db.QueryRow(
		s.q(`SELECT state, processed_at FROM stripe_processed_events WHERE event_id = ?`), eventID,
	).Scan(&st, &existing); err != nil {
		// The row vanished between the statements (a concurrent unmark or
		// prune). Claim it as new: every handler re-syncs from current Stripe
		// state, so a rerun is safe.
		return StripeEventClaimed, ts, nil
	}
	if st == "done" {
		return StripeEventDone, existing, nil
	}
	return StripeEventInProgress, existing, nil
}

// CompleteStripeEvent marks a claimed event done after its handler succeeded
// (BUG-3561). It acts only on the row still carrying the claim's token, so a
// handler whose lease was taken over cannot complete the event for the one
// that took it. Returns whether the row changed.
func (s *Store) CompleteStripeEvent(eventID, processedAt string) (bool, error) {
	if eventID == "" || processedAt == "" {
		return false, fmt.Errorf("complete stripe event: event_id and processed_at are required")
	}
	res, err := s.db.Exec(
		s.q(`UPDATE stripe_processed_events SET state = 'done', lease_until = NULL WHERE event_id = ? AND processed_at = ? AND state = 'processing'`),
		eventID, processedAt,
	)
	if err != nil {
		return false, fmt.Errorf("complete stripe event %s: %w", eventID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil
	}
	return n == 1, nil
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
