package models

// NeedsSnoozeMarker is what a person had seen of an item when they dismissed
// it from their needs-you inbox (TASK-3918, IDEA-3906): the newest comment
// by anyone else, the ids of the comments at that same second, and the
// item's latest status-transition seq. The server clamps a client's marker
// to its own values, so a marker can only ever claim LESS was seen.
type NeedsSnoozeMarker struct {
	SeenCommentAt     string   `json:"seen_comment_at"`
	SeenCommentIDs    []string `json:"seen_comment_ids"`
	SeenTransitionSeq int64    `json:"seen_transition_seq"`
}

// NeedsSnooze is one person's dismissal or deferral of one item.
type NeedsSnooze struct {
	ItemID      string            `json:"item_id"`
	WorkspaceID string            `json:"workspace_id"`
	Kind        string            `json:"kind"` // NeedsSnoozeDismiss or NeedsSnoozeDefer
	DeferUntil  string            `json:"defer_until,omitempty"`
	Marker      NeedsSnoozeMarker `json:"marker"`
	CreatedAt   string            `json:"created_at"`
	// Holds reports whether the snooze still hides the item, computed at
	// read time: a deferral until its date, a dismissal until something
	// happens on the item after the marker.
	Holds bool `json:"holds"`
}

const (
	NeedsSnoozeDismiss = "dismiss"
	NeedsSnoozeDefer   = "defer"
)
