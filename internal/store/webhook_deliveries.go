package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Per-endpoint webhook delivery state (SPEC-6 U10c, DOC-3371 §5; TASK-3408).
//
// For APP hooks every decision is recorded, and a terminal row is never
// retried. For OWNER hooks only "delivered" is recorded, and used only to
// skip re-sending an event an owner endpoint already received when the
// event stays owed to someone else (an app's rate limit or outage must not
// duplicate deliveries to people who never installed it; lead ruling, day
// 87). Owner failure, retry and ack are otherwise unchanged; full owner
// per-endpoint state is TASK-3409.

// Delivery statuses. Terminal ones end the endpoint's part in the event.
const (
	DeliveryDelivered   = "delivered"
	DeliveryPermanent   = "permanent"
	DeliveryRefused     = "refused"
	DeliverySkipped     = "skipped"
	DeliveryDropped     = "dropped"
	DeliveryTransient   = "transient"
	DeliveryDeferred    = "deferred"
	DeliveryRateLimited = "rate_limited"
)

// DeliveryTerminal reports whether a status ends the endpoint's part.
func DeliveryTerminal(status string) bool {
	switch status {
	case DeliveryDelivered, DeliveryPermanent, DeliveryRefused, DeliverySkipped, DeliveryDropped:
		return true
	}
	return false
}

// DeliveryStatus returns the recorded status of eventID for webhookID, ""
// when nothing is recorded.
func (s *Store) DeliveryStatus(eventID, webhookID string) (string, error) {
	var st string
	err := s.db.QueryRow(s.q(`SELECT status FROM webhook_deliveries WHERE outbox_event_id = ? AND webhook_id = ?`), eventID, webhookID).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("delivery status: %w", err)
	}
	return st, nil
}

// RecordDelivery stores status for (eventID, webhookID), adding attempts
// requests actually sent (a refusal or a rate-limited deferral sends none).
// A terminal row is never overwritten: the first terminal decision stands.
func (s *Store) RecordDelivery(eventID, webhookID, status, lastError string, attempts int) error {
	inc := attempts
	ts := now()
	_, err := s.db.Exec(s.q(`INSERT INTO webhook_deliveries (outbox_event_id, webhook_id, status, attempts, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (outbox_event_id, webhook_id) DO UPDATE SET
			status = excluded.status, attempts = webhook_deliveries.attempts + excluded.attempts,
			last_error = excluded.last_error, updated_at = excluded.updated_at
		WHERE webhook_deliveries.status NOT IN ('delivered', 'permanent', 'refused', 'skipped', 'dropped')`),
		eventID, webhookID, status, inc, nullIfEmpty(lastError), ts)
	if err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	return nil
}

// DropDelivery marks a still-owed delivery dropped and counts it on the hook,
// in one transaction. A no-op when the row is already terminal.
func (s *Store) DropDelivery(eventID, webhookID, reason string) (bool, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	ts := now()
	res, err := tx.Exec(s.q(`INSERT INTO webhook_deliveries (outbox_event_id, webhook_id, status, attempts, last_error, updated_at)
		VALUES (?, ?, 'dropped', 0, ?, ?)
		ON CONFLICT (outbox_event_id, webhook_id) DO UPDATE SET status = 'dropped', last_error = excluded.last_error, updated_at = excluded.updated_at
		WHERE webhook_deliveries.status NOT IN ('delivered', 'permanent', 'refused', 'skipped', 'dropped')`),
		eventID, webhookID, reason, ts)
	if err != nil {
		return false, fmt.Errorf("drop delivery: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(s.q(`UPDATE webhooks SET dropped_count = dropped_count + 1 WHERE id = ?`), webhookID); err != nil {
		return false, fmt.Errorf("drop delivery: count: %w", err)
	}
	return true, tx.Commit()
}

// OwnerDelivered reports whether an owner hook already received eventID.
func (s *Store) OwnerDelivered(eventID, webhookID string) (bool, error) {
	st, err := s.DeliveryStatus(eventID, webhookID)
	return st == DeliveryDelivered, err
}

// RecordOwnerDelivered records that an owner hook received eventID. Only
// success is recorded for owner hooks (see the file comment).
func (s *Store) RecordOwnerDelivered(eventID, webhookID string) error {
	return s.RecordDelivery(eventID, webhookID, DeliveryDelivered, "", 1)
}

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
