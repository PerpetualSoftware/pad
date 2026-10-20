package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// GetNotificationSettings returns userID's push settings, or the defaults when
// they have never saved any (TASK-3937). A missing row is not an error.
func (s *Store) GetNotificationSettings(userID string) (models.NotificationSettings, error) {
	var out models.NotificationSettings
	err := s.db.QueryRow(s.q(`
		SELECT push_enabled, notify_assigned, notify_comments, notify_replies,
		       notify_reminders, notify_mentions, include_agent_activity
		FROM user_notification_settings WHERE user_id = ?`), userID).Scan(
		&out.PushEnabled, &out.NotifyAssigned, &out.NotifyComments, &out.NotifyReplies,
		&out.NotifyReminders, &out.NotifyMentions, &out.IncludeAgentActivity)
	if errors.Is(err, sql.ErrNoRows) {
		return models.DefaultNotificationSettings(), nil
	}
	if err != nil {
		return models.NotificationSettings{}, fmt.Errorf("get notification settings: %w", err)
	}
	return out, nil
}

// PutNotificationSettings replaces userID's settings wholesale: a field left
// false is stored false, so a client that turns one off and sends the rest
// unchanged cannot lose the choice (TASK-3937).
func (s *Store) PutNotificationSettings(userID string, in models.NotificationSettings) error {
	b := s.dialect.BoolToInt
	_, err := s.db.Exec(s.q(`
		INSERT INTO user_notification_settings
			(user_id, push_enabled, notify_assigned, notify_comments, notify_replies,
			 notify_reminders, notify_mentions, include_agent_activity, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET
			push_enabled = excluded.push_enabled,
			notify_assigned = excluded.notify_assigned,
			notify_comments = excluded.notify_comments,
			notify_replies = excluded.notify_replies,
			notify_reminders = excluded.notify_reminders,
			notify_mentions = excluded.notify_mentions,
			include_agent_activity = excluded.include_agent_activity,
			updated_at = excluded.updated_at`),
		userID, b(in.PushEnabled), b(in.NotifyAssigned), b(in.NotifyComments), b(in.NotifyReplies),
		b(in.NotifyReminders), b(in.NotifyMentions), b(in.IncludeAgentActivity), now())
	if err != nil {
		return fmt.Errorf("put notification settings: %w", err)
	}
	return nil
}
