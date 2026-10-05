package store

import (
	"database/sql"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// CreateWebhook registers a new webhook for a workspace.
//
// With WithPlanLimit() the workspace's webhooks cap is counted in the insert's
// transaction, under acquirePlanLimitLock, and a reached cap refuses with
// *PlanLimitError (BUG-2808).
func (s *Store) CreateWebhook(workspaceID string, input models.WebhookCreate, opts ...MintOption) (*models.Webhook, error) {
	mint := resolveMintOptions(opts)
	id := newID()
	ts := now()

	evts := input.Events
	if evts == "" {
		evts = `["*"]`
	}

	// Encrypt the HMAC secret at rest. With no encryption key configured
	// (common on self-host) encrypt() returns the plaintext unchanged, so
	// this stays a no-op fallback rather than a hard requirement.
	encSecret, err := s.encrypt(input.Secret)
	if err != nil {
		return nil, fmt.Errorf("encrypt webhook secret: %w", err)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("insert webhook: %w", err)
	}
	defer tx.Rollback()

	if mint.planLimit {
		if err := s.acquirePlanLimitLock(tx, workspaceID, "webhooks"); err != nil {
			return nil, err
		}
		if err := s.enforceWorkspaceLimitTx(tx, workspaceID, "webhooks"); err != nil {
			return nil, err
		}
	}

	_, err = tx.Exec(s.q(`
		INSERT INTO webhooks (id, workspace_id, url, secret, events, active, created_at, updated_at, failure_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
	`), id, workspaceID, input.URL, encSecret, evts, s.dialect.BoolToInt(true), ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert webhook: %w", err)
	}
	// Read back INSIDE the transaction, before the commit (TASK-3406, the
	// BUG-3405 shape): a deletion landing after the commit (an account
	// deletion, a revoke, a cascade) cannot turn a successful write into
	// a nil result.
	out, err := s.getWebhookQ(tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("insert webhook: %w", err)
	}
	afterCommitReadback("webhook", id)
	return out, nil
}

// GetWebhook retrieves a single webhook by ID.
func (s *Store) GetWebhook(id string) (*models.Webhook, error) {
	return s.getWebhookQ(s.db, id)
}

func (s *Store) getWebhookQ(q Queryer, id string) (*models.Webhook, error) {
	var wh models.Webhook
	var active bool
	var createdAt, updatedAt string
	var lastTriggeredAt *string

	err := q.QueryRow(s.q(`
		SELECT id, workspace_id, url, secret, events, active, created_at, updated_at, last_triggered_at, failure_count, dropped_count
		FROM webhooks
		WHERE id = ?
	`), id).Scan(
		&wh.ID, &wh.WorkspaceID, &wh.URL, &wh.Secret, &wh.Events,
		&active, &createdAt, &updatedAt, &lastTriggeredAt, &wh.FailureCount, &wh.DroppedCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get webhook: %w", err)
	}

	// Decrypt the secret for internal use (the dispatcher signs with the
	// plaintext). Pre-encryption rows lack the "enc:" prefix and pass through
	// unchanged, so existing plaintext secrets keep working.
	if wh.Secret, err = s.decrypt(wh.Secret); err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	wh.HasSecret = wh.Secret != ""
	wh.Active = active
	wh.CreatedAt = parseTime(createdAt)
	wh.UpdatedAt = parseTime(updatedAt)
	wh.LastTriggeredAt = parseTimePtr(lastTriggeredAt)
	return &wh, nil
}

// GetWebhookScoped retrieves a single webhook by ID, but only if it belongs to
// the given workspace. The workspace_id predicate is applied in SQL BEFORE the
// secret is decrypted, so a foreign (or nonexistent) ID returns (nil, nil)
// without touching the ciphertext — this both closes the cross-workspace
// existence oracle (TASK-266) and avoids a decrypt-failure 500 leaking that a
// foreign webhook exists. Returns (nil, nil) when no webhook matches both.
func (s *Store) GetWebhookScoped(id, workspaceID string) (*models.Webhook, error) {
	var wh models.Webhook
	var active bool
	var createdAt, updatedAt string
	var lastTriggeredAt *string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, url, secret, events, active, created_at, updated_at, last_triggered_at, failure_count, dropped_count
		FROM webhooks
		WHERE id = ? AND workspace_id = ? AND app_install_id IS NULL
	`), id, workspaceID).Scan(
		&wh.ID, &wh.WorkspaceID, &wh.URL, &wh.Secret, &wh.Events,
		&active, &createdAt, &updatedAt, &lastTriggeredAt, &wh.FailureCount, &wh.DroppedCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get webhook: %w", err)
	}

	if wh.Secret, err = s.decrypt(wh.Secret); err != nil {
		return nil, fmt.Errorf("decrypt webhook secret: %w", err)
	}
	wh.HasSecret = wh.Secret != ""
	wh.Active = active
	wh.CreatedAt = parseTime(createdAt)
	wh.UpdatedAt = parseTime(updatedAt)
	wh.LastTriggeredAt = parseTimePtr(lastTriggeredAt)
	return &wh, nil
}

// ListWebhooks returns all webhooks for a workspace.
// WorkspaceLive reports whether a workspace exists and is not soft-deleted.
// The webhook dispatcher re-checks it before every send (BUG-3340).
func (s *Store) WorkspaceLive(workspaceID string) (bool, error) {
	var one int
	err := s.db.QueryRow(s.q(`SELECT 1 FROM workspaces WHERE id = ? AND deleted_at IS NULL`), workspaceID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("workspace live: %w", err)
	}
	return true, nil
}

// ListWebhooks lists a workspace's webhooks. A soft-deleted workspace has
// none (BUG-3340): the dispatcher's two delivery paths list through here, so
// its events are dropped rather than delivered during the restore window, and
// a restore brings the webhooks back.
func (s *Store) ListWebhooks(workspaceID string) ([]models.Webhook, error) {
	rows, err := s.db.Query(s.q(`
		SELECT wh.id, wh.workspace_id, wh.url, wh.secret, wh.events, wh.active, wh.created_at, wh.updated_at, wh.last_triggered_at, wh.failure_count, wh.dropped_count
		FROM webhooks wh
		JOIN workspaces w ON w.id = wh.workspace_id
		WHERE wh.workspace_id = ? AND w.deleted_at IS NULL AND wh.app_install_id IS NULL
		ORDER BY wh.created_at ASC
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer rows.Close()

	var result []models.Webhook
	for rows.Next() {
		var wh models.Webhook
		var active bool
		var createdAt, updatedAt string
		var lastTriggeredAt *string

		if err := rows.Scan(
			&wh.ID, &wh.WorkspaceID, &wh.URL, &wh.Secret, &wh.Events,
			&active, &createdAt, &updatedAt, &lastTriggeredAt, &wh.FailureCount, &wh.DroppedCount,
		); err != nil {
			return nil, fmt.Errorf("scan webhook: %w", err)
		}
		if wh.Secret, err = s.decrypt(wh.Secret); err != nil {
			return nil, fmt.Errorf("decrypt webhook secret: %w", err)
		}
		wh.HasSecret = wh.Secret != ""
		wh.Active = active
		wh.CreatedAt = parseTime(createdAt)
		wh.UpdatedAt = parseTime(updatedAt)
		wh.LastTriggeredAt = parseTimePtr(lastTriggeredAt)
		result = append(result, wh)
	}
	return result, rows.Err()
}

// DeleteWebhookScoped removes a webhook by ID, verifying it belongs to the given
// workspace. Prevents cross-workspace deletion (TASK-266): an owner of one
// workspace must not be able to delete another workspace's webhook by ID. The
// scoped DELETE is also atomic and avoids decrypting the secret just to delete —
// a rotated/missing encryption key would otherwise leave a broken webhook
// undeletable. Returns sql.ErrNoRows when no webhook matches both id and workspace.
func (s *Store) DeleteWebhookScoped(id, workspaceID string) error {
	// An app's hook is the install's (TASK-3408): never an owner's to delete.
	result, err := s.db.Exec(s.q("DELETE FROM webhooks WHERE id = ? AND workspace_id = ? AND app_install_id IS NULL"), id, workspaceID)
	if err != nil {
		return fmt.Errorf("delete webhook: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateWebhookFailure increments or resets the failure count for a webhook.
// If failed is true, the failure_count is incremented. If it reaches the
// threshold of 10, the webhook is auto-deactivated.
// If failed is false, the failure_count is reset to 0 and last_triggered_at is updated.
func (s *Store) UpdateWebhookFailure(id string, failed bool) error {
	ts := now()
	if failed {
		_, err := s.db.Exec(s.q(`
			UPDATE webhooks
			SET failure_count = failure_count + 1,
			    updated_at = ?,
			    active = CASE WHEN failure_count + 1 >= 10 THEN FALSE ELSE active END
			WHERE id = ?
		`), ts, id)
		if err != nil {
			return fmt.Errorf("update webhook failure: %w", err)
		}
	} else {
		_, err := s.db.Exec(s.q(`
			UPDATE webhooks
			SET failure_count = 0,
			    last_triggered_at = ?,
			    updated_at = ?
			WHERE id = ?
		`), ts, ts, id)
		if err != nil {
			return fmt.Errorf("update webhook success: %w", err)
		}
	}
	return nil
}
