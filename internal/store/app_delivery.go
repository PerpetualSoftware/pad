package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// App webhook delivery: admission and the in-flight fence (SPEC-6 U10b,
// DOC-3371 §5; TASK-3408).
//
// Every ATTEMPT is admitted in one transaction that takes the install row
// FOR SHARE (FencedTx's first lock, so it orders against disable's phase-1
// FOR UPDATE like every fenced app write), re-checks the install, the hook
// and the event's visibility, and inserts an app_delivery_inflight row whose
// expiry is DATABASE time. The attempt deletes the row when it ends; disable
// phase 2 waits until the install has no unexpired row.

// appDeliveryFenceSeconds is how long an in-flight row lives: the attempt's
// 10 s deadline, created BEFORE admission, plus 2 s of slack. The attempt
// therefore always ends before its row expires, whatever the hosts' clocks
// say, because both intervals are relative.
//
// STATED RESIDUAL (codex r1 on U10b): the expiry is the DATABASE's wall
// clock, by design (DOC-3371 §5 rejects the application host's), and the
// attempt's deadline is the application's monotonic clock. A forward step of
// the database clock while an attempt is in flight can expire its row early,
// so a drain could return during that one send. A normal attempt does not
// rely on expiry at all (it deletes its own row when it ends); expiry only
// releases rows of attempts that crashed. Bounded by one delivery deadline.
const appDeliveryFenceSeconds = 12

// appDeliveryDrainBound caps the phase-2 wait: one fence lifetime plus
// slack. A row cannot outlive it, so a longer wait would mean the database's
// clock stood still.
var appDeliveryDrainBound = (appDeliveryFenceSeconds + 3) * time.Second

// appDeliveryDrainPoll is the drain's poll interval.
var appDeliveryDrainPoll = 250 * time.Millisecond

// AppHookTarget is an app hook that may want an event: read unlocked, for
// the outbox drain to decide whom to try. Admission re-decides.
type AppHookTarget struct {
	WebhookID string
	InstallID string
}

// ListAppWebhookTargets returns the workspace's deliverable-looking app hooks
// subscribed to event (any event when it is ""): install active, secret
// delivered. Unlocked; nothing is decided here.
func (s *Store) ListAppWebhookTargets(workspaceID, event string) ([]AppHookTarget, error) {
	rows, err := s.db.Query(s.q(`SELECT w.id, w.app_install_id, w.events FROM webhooks w
		JOIN app_installs i ON i.id = w.app_install_id
		WHERE w.workspace_id = ? AND w.app_install_id IS NOT NULL AND w.secret_delivered_at IS NOT NULL
		  AND i.state = 'active'
		ORDER BY w.id`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list app webhook targets: %w", err)
	}
	defer rows.Close()
	var out []AppHookTarget
	for rows.Next() {
		var t AppHookTarget
		var events string
		if err := rows.Scan(&t.WebhookID, &t.InstallID, &events); err != nil {
			return nil, err
		}
		if appHookSubscribes(events, event, "") {
			out = append(out, t)
		}
	}
	return out, rows.Err()
}

// appHookSubscribes reports whether a stored subscription list names event,
// and, when collectionID is not empty, names it for that collection.
func appHookSubscribes(eventsJSON, event, collectionID string) bool {
	var subs []appWebhookSubscription
	if err := json.Unmarshal([]byte(eventsJSON), &subs); err != nil {
		return false
	}
	for _, sub := range subs {
		if event != "" && sub.Name != event {
			continue
		}
		if collectionID == "" {
			return true
		}
		for _, id := range sub.CollectionIDs {
			if id == collectionID {
				return true
			}
		}
	}
	return false
}

// AppDeliveryAdmission is what an admitted attempt sends with.
type AppDeliveryAdmission struct {
	URL       string
	Secret    string
	InstallID string
}

// AppDeliveryRefusedError: admission refused the attempt. Nothing is owed:
// the install is not active, the hook is gone or held, or the event is not
// visible to the app. Reason names which, for logs and metrics.
type AppDeliveryRefusedError struct{ Reason string }

func (e *AppDeliveryRefusedError) Error() string { return "app delivery refused: " + e.Reason }

// AdmitAppDelivery admits one attempt of event, whose app-projection block
// names collectionID, to the app hook webhookID, recording deliveryID as in
// flight. *AppDeliveryRefusedError when it must not be sent; any other error
// is the store's and the event is still owed.
//
// occurredAt is the event's outbox occurred_at (RFC3339, seconds). An event
// that occurred before the hook's deliver_from is refused: the app skipped
// it while held, disabled or not subscribed, and an owner hook's failure
// keeping the row pending must not deliver it later (codex r5). Both stamps
// are whole seconds, so an event in the same second as the redeem or
// re-enable is admitted: a one-second stated residual.
func (s *Store) AdmitAppDelivery(webhookID, event, collectionID, occurredAt, deliveryID string) (*AppDeliveryAdmission, error) {
	// The hook's install never changes (app_install_id is written once), so
	// it is read before the lock to know which install row to take.
	var installID sql.NullString
	err := s.db.QueryRow(s.q(`SELECT app_install_id FROM webhooks WHERE id = ?`), webhookID).Scan(&installID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !installID.Valid) {
		return nil, &AppDeliveryRefusedError{Reason: "hook_gone"}
	}
	if err != nil {
		return nil, fmt.Errorf("admit app delivery: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("admit app delivery: begin: %w", err)
	}
	defer tx.Rollback()

	q := `SELECT state, workspace_id FROM app_installs WHERE id = ?`
	if s.dialect.Driver() == DriverPostgres {
		q += ` FOR SHARE`
	}
	var state, workspaceID string
	err = tx.QueryRow(s.q(q), installID.String).Scan(&state, &workspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &AppDeliveryRefusedError{Reason: "install_gone"}
	}
	if err != nil {
		return nil, fmt.Errorf("admit app delivery: lock install: %w", err)
	}
	if state != InstallActive {
		return nil, &AppDeliveryRefusedError{Reason: "install_" + state}
	}
	// A soft-deleted workspace keeps its installs and collections, so its
	// pending events must be refused here (codex r1 on U10b; the owner path's
	// WorkspaceLive check, BUG-3340). A plain read: deletion that races this
	// serializes as "delivered, then deleted", the BUG-3340 residual.
	var live int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM workspaces WHERE id = ? AND deleted_at IS NULL`), workspaceID).Scan(&live); err != nil {
		return nil, fmt.Errorf("admit app delivery: workspace: %w", err)
	}
	if live == 0 {
		return nil, &AppDeliveryRefusedError{Reason: "workspace_deleted"}
	}

	// Under the install lock: an upgrade (which changes the URL and the
	// subscriptions) and uninstall (which deletes the hook) both hold this
	// row FOR UPDATE, so what is read here is what the owner last consented.
	var url, secret, events string
	var delivered, deliverFrom sql.NullString
	err = tx.QueryRow(s.q(`SELECT url, secret, events, secret_delivered_at, deliver_from FROM webhooks WHERE id = ?`), webhookID).
		Scan(&url, &secret, &events, &delivered, &deliverFrom)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, &AppDeliveryRefusedError{Reason: "hook_gone"}
	}
	if err != nil {
		return nil, fmt.Errorf("admit app delivery: read hook: %w", err)
	}
	if !delivered.Valid || delivered.String == "" {
		return nil, &AppDeliveryRefusedError{Reason: "hook_held"}
	}
	if !deliverFrom.Valid || occurredAt < deliverFrom.String {
		return nil, &AppDeliveryRefusedError{Reason: "before_deliverable"}
	}
	if !appHookSubscribes(events, event, collectionID) {
		return nil, &AppDeliveryRefusedError{Reason: "not_subscribed"}
	}
	// The ceiling: the event's collection must still be this install's
	// companion. A released or deleted companion is no longer the app's.
	var n int
	if err := tx.QueryRow(s.q(`SELECT COUNT(*) FROM collections WHERE id = ? AND via_app = ? AND deleted_at IS NULL`),
		collectionID, installID.String).Scan(&n); err != nil {
		return nil, fmt.Errorf("admit app delivery: ceiling: %w", err)
	}
	if n == 0 {
		return nil, &AppDeliveryRefusedError{Reason: "not_visible"}
	}

	insert := `INSERT INTO app_delivery_inflight (delivery_id, install_id, expires_at)
		VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now', '+12 seconds'))`
	if s.dialect.Driver() == DriverPostgres {
		insert = `INSERT INTO app_delivery_inflight (delivery_id, install_id, expires_at)
			VALUES (?, ?, now() + interval '12 seconds')`
	}
	if _, err := tx.Exec(s.q(insert), deliveryID, installID.String); err != nil {
		return nil, fmt.Errorf("admit app delivery: in-flight row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("admit app delivery: commit: %w", err)
	}
	plain, err := s.decrypt(secret)
	if err != nil {
		// Admitted but unsendable: release the row now rather than make the
		// drain wait for it.
		_ = s.EndAppDelivery(deliveryID)
		return nil, fmt.Errorf("admit app delivery: decrypt secret: %w", err)
	}
	return &AppDeliveryAdmission{URL: url, Secret: plain, InstallID: installID.String}, nil
}

// EndAppDelivery removes an attempt's in-flight row. Safe to repeat.
func (s *Store) EndAppDelivery(deliveryID string) error {
	if _, err := s.db.Exec(s.q(`DELETE FROM app_delivery_inflight WHERE delivery_id = ?`), deliveryID); err != nil {
		return fmt.Errorf("end app delivery: %w", err)
	}
	return nil
}

// liveAppDeliveries counts installID's unexpired in-flight rows, in
// database time.
func (s *Store) liveAppDeliveries(installID string) (int, error) {
	q := `SELECT COUNT(*) FROM app_delivery_inflight WHERE install_id = ? AND expires_at > strftime('%Y-%m-%dT%H:%M:%fZ', 'now')`
	if s.dialect.Driver() == DriverPostgres {
		q = `SELECT COUNT(*) FROM app_delivery_inflight WHERE install_id = ? AND expires_at > now()`
	}
	var n int
	if err := s.db.QueryRow(s.q(q), installID).Scan(&n); err != nil {
		return 0, fmt.Errorf("live app deliveries: %w", err)
	}
	return n, nil
}
