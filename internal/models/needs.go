package models

// NeedsSnoozeMarker is what a person had seen of an item when they dismissed
// it from their needs-you inbox (TASK-3918, IDEA-3906): the newest comment
// by anyone else, the ids of the comments at that same second, and the
// item's latest status-transition seq. The server clamps a client's marker
// to its own values, so a marker can only ever claim LESS was seen.
//
// SeenAt, SeenCollectionID and SeenAssigneeID are read by "not mine"
// (TASK-4643) alone: when the person looked, the item's collection then, and
// its assignee then. A dismissal ignores them.
type NeedsSnoozeMarker struct {
	SeenCommentAt     string   `json:"seen_comment_at"`
	SeenCommentIDs    []string `json:"seen_comment_ids"`
	SeenTransitionSeq int64    `json:"seen_transition_seq"`
	SeenAt            string   `json:"seen_at,omitempty"`
	SeenCollectionID  string   `json:"seen_collection_id,omitempty"`
	SeenAssigneeID    string   `json:"seen_assignee_id,omitempty"`
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

// NeedsNotMine is one person's "not mine" on one item (TASK-4643). It hides
// the item from their needs-you inbox until the item comes back to them (see
// migration 186 for the release rule, which the inbox evaluates at read time).
type NeedsNotMine struct {
	ItemID      string            `json:"item_id"`
	WorkspaceID string            `json:"workspace_id"`
	Marker      NeedsSnoozeMarker `json:"marker"`
	// Source is how the request that wrote it authenticated: NeedsSourceWeb
	// or NeedsSourceCLI.
	Source    string `json:"source"`
	CreatedAt string `json:"created_at"`
}

const (
	NeedsSourceWeb = "web"
	NeedsSourceCLI = "cli"
)
