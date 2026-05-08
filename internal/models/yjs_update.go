package models

import "time"

// YjsUpdate is a single appended Yjs binary update for an item.
//
// Rows live in the `item_yjs_updates` op-log added in PLAN-1248. The
// dumb-relay WebSocket server (TASK-1254) appends one row per peer
// update; on reconnect, clients replay rows since their last known ID
// and resume seamlessly. UpdateData is the raw output of Yjs's
// encodeStateAsUpdate / mergeUpdates — opaque to the server. SchemaVersion
// is stamped from the client at append time and drives TASK-1268's
// snapshot-and-rebuild flow when a Tiptap minor bump changes the
// underlying ProseMirror schema between releases.
type YjsUpdate struct {
	ID            int64     `json:"id"`
	ItemID        string    `json:"item_id"`
	UpdateData    []byte    `json:"update_data"`
	SchemaVersion string    `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
}
