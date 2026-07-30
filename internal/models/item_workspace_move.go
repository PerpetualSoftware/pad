package models

// ItemWorkspaceMove is one durable record of an item being copied — or moved,
// which is a copy plus an archive of the source — from one workspace to
// another. Written in the SAME transaction as the copy, so the provenance can
// never disagree with the data (PLAN-2357 DR-2).
//
// It backs exactly two lookups:
//
//   - forward, by SourceItemID: "where did this item go?" One source can be
//     copied into several workspaces, so the forward lookup is a SET.
//   - back, by TargetItemID: "where did this item come from?" A destination
//     item is created by exactly one copy, so this is at most one row.
//
// ArchivedSource distinguishes a move from a plain copy, and only a move
// produces a "moved to" pointer on the archived source (DR-2a). A source
// copied three times and then moved has four rows, and only the archived one
// says where it *went*.
type ItemWorkspaceMove struct {
	ID                string `json:"id"`
	SourceWorkspaceID string `json:"source_workspace_id"`
	SourceItemID      string `json:"source_item_id"`
	TargetWorkspaceID string `json:"target_workspace_id"`
	TargetItemID      string `json:"target_item_id"`

	// ArchivedSource is true when the source was archived as part of the
	// operation (a move) and false for a plain copy.
	ArchivedSource bool `json:"archived_source"`

	// SourceSeq is the workspace-scoped `seq` the source workspace assigned
	// when it archived the source. It exists only to order one source's
	// moves deterministically: CreatedAt is second-precision RFC3339, so two
	// moves inside the same second tie and the "moved to" lookup could
	// otherwise return an arbitrary destination.
	//
	// nil for plain copies, which never archive. Since the "moved to" lookup
	// reads only ArchivedSource rows, nil values never participate in the
	// ordering.
	SourceSeq *int64 `json:"source_seq,omitempty"`

	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}
