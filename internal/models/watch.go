package models

import "time"

// Watch is a durable, server-side subscription created by `pad watch <ref>`
// (TASK-2533, per DOC-2479's subscription-table design). Unlike an SSE
// connection or a plugin-monitor process, a Watch survives both — it is the
// thing that makes "the monitor restarts every session" not lose track of
// what a user asked to be told about.
type Watch struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	ItemID      string `json:"item_id"`
	// Predicate is the raw `--until field=value` string (e.g. "status=done"),
	// or "" for an unconditional watch that fires on any matching
	// notification for this item. DOC-2479 specs only this single
	// field=value grammar — no boolean combinators.
	Predicate string    `json:"predicate,omitempty"`
	CreatedAt time.Time `json:"created_at"`

	// Populated by joins (not stored) — the CLI's `pad watch list` and
	// the event-stream handler's summary text both want the item's
	// human-facing identity without a second lookup.
	ItemRef       string `json:"item_ref,omitempty"`
	ItemTitle     string `json:"item_title,omitempty"`
	ItemSlug      string `json:"item_slug,omitempty"`
	WorkspaceSlug string `json:"workspace_slug,omitempty"`
	// ItemCollectionID is the watched item's collection ID (populated by
	// the same join as the fields above). Internal-only (`json:"-"`,
	// like models.Item.LastMutation) — not part of the wire contract,
	// consumed only by server.filterWatchesByCurrentAccess (TASK-2533,
	// codex round 1 finding 1) to re-check the caller's CURRENT
	// visibility into the watched item's collection before delivering or
	// listing a watch, so a workspace membership or grant revoked after
	// the watch was created can't keep leaking item metadata/events
	// through a stale row.
	ItemCollectionID string `json:"-"`
}
