package models

// UIDismissalKeys are the one-time UI suggestions a user can dismiss
// (TASK-3452). The set is closed: the store and the handler refuse any other
// key, so users.ui_dismissals stays a short list of known words.
var UIDismissalKeys = []string{
	// The first-time tutorial card on /console's empty state (Pad Cloud).
	"tutorials.console",
	// The first-time tutorial card on a workspace's setup launchpad.
	"tutorials.launchpad",
}

// IsUIDismissalKey reports whether key is in UIDismissalKeys.
func IsUIDismissalKey(key string) bool {
	for _, k := range UIDismissalKeys {
		if k == key {
			return true
		}
	}
	return false
}
