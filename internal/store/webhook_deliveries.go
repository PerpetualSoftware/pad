package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// Per-endpoint webhook delivery state (SPEC-6 U10c, DOC-3371 §5; TASK-3408).
//
// Every decision is recorded for APP and OWNER hooks alike, and a terminal
// row is never retried (owner hooks since TASK-3409; U10c recorded only an
// owner's success, to skip re-sending while an app kept the event owed). A
// retry therefore goes only to endpoints still owed.

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
// A terminal row is never overwritten, the first terminal decision standing,
// with ONE exception: "delivered" replaces "dropped" and takes the drop back
// off the hook's count. A request admitted before another drainer dropped
// the event (its claim expired mid-request) did reach the app, and the
// owner must not see it as lost (codex r5 on U10c).
func (s *Store) RecordDelivery(eventID, webhookID, status, lastError string, attempts int) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	// Insert first: ON CONFLICT waits for a concurrent inserter (a drop) to
	// commit, so a row that exists afterwards can be locked and read, and an
	// absent one is simply ours (codex r6 on U10c: FOR UPDATE alone locks
	// nothing when the row does not exist yet).
	res, err := tx.Exec(s.q(`INSERT INTO webhook_deliveries (outbox_event_id, webhook_id, status, attempts, last_error, updated_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (outbox_event_id, webhook_id) DO NOTHING`),
		eventID, webhookID, status, attempts, nullIfEmpty(lastError), ts)
	if err != nil {
		return fmt.Errorf("record delivery: insert: %w", err)
	}
	if recordDeliveryHook != nil {
		recordDeliveryHook()
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return tx.Commit()
	}
	q := `SELECT status FROM webhook_deliveries WHERE outbox_event_id = ? AND webhook_id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR UPDATE`
	}
	var prev string
	if err := tx.QueryRow(s.q(q), eventID, webhookID).Scan(&prev); err != nil {
		return fmt.Errorf("record delivery: read: %w", err)
	}
	undrop := prev == DeliveryDropped && status == DeliveryDelivered
	if DeliveryTerminal(prev) && !undrop {
		return nil
	}
	if _, err := tx.Exec(s.q(`UPDATE webhook_deliveries SET status = ?, attempts = attempts + ?, last_error = ?, updated_at = ?
		WHERE outbox_event_id = ? AND webhook_id = ?`),
		status, attempts, nullIfEmpty(lastError), ts, eventID, webhookID); err != nil {
		return fmt.Errorf("record delivery: %w", err)
	}
	if undrop {
		if _, err := tx.Exec(s.q(`UPDATE webhooks SET dropped_count = dropped_count - 1 WHERE id = ? AND dropped_count > 0`), webhookID); err != nil {
			return fmt.Errorf("record delivery: undo drop: %w", err)
		}
	}
	return tx.Commit()
}

// OwnerDelivered reports whether an owner hook already received eventID.
func (s *Store) OwnerDelivered(eventID, webhookID string) (bool, error) {
	st, err := s.DeliveryStatus(eventID, webhookID)
	return st == DeliveryDelivered, err
}

// OwnerDeliveryStatus is the recorded status of eventID for an owner hook,
// "" when none (webhooks.OwnerDeliveryLedger, TASK-3409).
func (s *Store) OwnerDeliveryStatus(eventID, webhookID string) (string, error) {
	return s.DeliveryStatus(eventID, webhookID)
}

// RecordOwnerOutcome records an owner hook's outcome for eventID: delivered
// and permanent are terminal, transient stays owed (TASK-3409). The first
// terminal decision stands, as for app hooks (RecordDelivery).
func (s *Store) RecordOwnerOutcome(eventID, webhookID, status, lastError string, attempts int) error {
	return s.RecordDelivery(eventID, webhookID, status, lastError, attempts)
}

// recordDeliveryHook runs inside RecordDelivery after its first statement,
// and dropDecisionHook just before a drop decision writes its row: tests
// interleave the two there. Both nil in production.
var (
	recordDeliveryHook func()
	dropDecisionHook   func()
)

func nullIfEmpty(v string) any {
	if v == "" {
		return nil
	}
	return v
}
