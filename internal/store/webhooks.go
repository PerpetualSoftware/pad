package store

import (
	"database/sql"
	"fmt"

	"github.com/xarmian/pad/internal/models"
)

// CreateWebhook registers a new webhook for a workspace.
func (s *Store) CreateWebhook(workspaceID string, input models.WebhookCreate) (*models.Webhook, error) {
	id := newID()
	ts := now()

	evts := input.Events
	if evts == "" {
		evts = `["*"]`
	}

	_, err := s.db.Exec(s.q(`
		INSERT INTO webhooks (id, workspace_id, url, secret, events, active, created_at, updated_at, failure_count)
		VALUES (?, ?, ?, ?, ?, 1, ?, ?, 0)
	`), id, workspaceID, input.URL, input.Secret, evts, ts, ts)
	if err != nil {
		return nil, fmt.Errorf("insert webhook: %w", err)
	}

	return s.GetWebhook(id)
}

// GetWebhook retrieves a single webhook by ID.
func (s *Store) GetWebhook(id string) (*models.Webhook, error) {
	var wh models.Webhook
	var active bool
	var createdAt, updatedAt string
	var lastTriggeredAt *string

	err := s.db.QueryRow(s.q(`
		SELECT id, workspace_id, url, secret, events, active, created_at, updated_at, last_triggered_at, failure_count
		FROM webhooks
		WHERE id = ?
	`), id).Scan(
		&wh.ID, &wh.WorkspaceID, &wh.URL, &wh.Secret, &wh.Events,
		&active, &createdAt, &updatedAt, &lastTriggeredAt, &wh.FailureCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get webhook: %w", err)
	}

	wh.Active = active
	wh.CreatedAt = parseTime(createdAt)
	wh.UpdatedAt = parseTime(updatedAt)
	wh.LastTriggeredAt = parseTimePtr(lastTriggeredAt)
	return &wh, nil
}

// ListWebhooks returns all webhooks for a workspace.
func (s *Store) ListWebhooks(workspaceID string) ([]models.Webhook, error) {
	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, url, secret, events, active, created_at, updated_at, last_triggered_at, failure_count
		FROM webhooks
		WHERE workspace_id = ?
		ORDER BY created_at ASC
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
			&active, &createdAt, &updatedAt, &lastTriggeredAt, &wh.FailureCount,
		); err != nil {
			return nil, fmt.Errorf("scan webhook: %w", err)
		}
		wh.Active = active
		wh.CreatedAt = parseTime(createdAt)
		wh.UpdatedAt = parseTime(updatedAt)
		wh.LastTriggeredAt = parseTimePtr(lastTriggeredAt)
		result = append(result, wh)
	}
	return result, rows.Err()
}

// DeleteWebhook removes a webhook by ID.
func (s *Store) DeleteWebhook(id string) error {
	result, err := s.db.Exec(s.q("DELETE FROM webhooks WHERE id = ?"), id)
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
			    active = CASE WHEN failure_count + 1 >= 10 THEN 0 ELSE active END
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
