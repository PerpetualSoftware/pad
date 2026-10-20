package models

// NotificationSettings are a person's recipient-side push filters (TASK-3937).
// The dispatcher (TASK-3939) is to apply them; the enqueue does not, so
// changing one never rewrites the queue.
type NotificationSettings struct {
	PushEnabled          bool `json:"push_enabled"`
	NotifyAssigned       bool `json:"notify_assigned"`
	NotifyComments       bool `json:"notify_comments"`
	NotifyReplies        bool `json:"notify_replies"`
	NotifyReminders      bool `json:"notify_reminders"`
	NotifyMentions       bool `json:"notify_mentions"`
	IncludeAgentActivity bool `json:"include_agent_activity"`
}

// DefaultNotificationSettings is what a user with no settings row has: every
// kind on, agent activity off.
func DefaultNotificationSettings() NotificationSettings {
	return NotificationSettings{
		PushEnabled:     true,
		NotifyAssigned:  true,
		NotifyComments:  true,
		NotifyReplies:   true,
		NotifyReminders: true,
		NotifyMentions:  true,
	}
}
