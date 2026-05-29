package models

// StatusTransition is one structured record of an item moving from one
// status value to another. Unlike the activity feed — which is
// debounce-coalesced and stores changes as a human-readable
// `metadata.changes` string — every status change writes its own row here,
// in the same transaction as the item update. That makes it the canonical
// source for "when did item X enter status Y", which the Reports
// aggregation needs for the completed-throughput and cycle-time series.
//
// FromStatus is empty when the prior status was unset (e.g. a transition
// recorded from a field that didn't previously carry a status value).
//
// PLAN-1628 / TASK-1637.
type StatusTransition struct {
	ID           string `json:"id"`
	ItemID       string `json:"item_id"`
	WorkspaceID  string `json:"workspace_id"`
	CollectionID string `json:"collection_id"`
	FromStatus   string `json:"from_status"`
	ToStatus     string `json:"to_status"`
	CreatedAt    string `json:"created_at"`
}
