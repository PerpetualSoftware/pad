package store

// ChildLinkTypes is the exported, read-only view of childLinkTypes
// (items.go) — the link types GetChildItems walks to find an item's
// children.
//
// It exists because callers OUTSIDE this package have to classify a
// link_type the same way GetChildItems does, and the only alternative is
// a second hardcoded list that drifts silently the day someone adds a
// third child link type. PLAN-2357's copy preflight is the first such
// caller: it partitions an item's links into hierarchy edges (reported as
// child_count / dropped_parent) and dependency edges (reported as
// outgoing/incoming link counts), and an edge that falls out of both
// partitions is a relationship the user is never told they are losing.
//
// Returns a copy; the underlying slice is package state.
func ChildLinkTypes() []string {
	return append([]string(nil), childLinkTypes...)
}
