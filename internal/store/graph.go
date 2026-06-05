package store

import (
	"fmt"
)

// GraphLink is one typed edge in the workspace graph
// (GET /workspaces/{ws}/graph — PLAN-1730 / TASK-1731). Source and
// target are item IDs; the handler maps them to refs and applies
// visibility filtering before they leave the server.
type GraphLink struct {
	SourceID string
	TargetID string
	Type     string // 'parent' | 'blocks' | 'implements' | 'related' | 'wiki-link'
}

// ListWorkspaceGraphLinks returns every edge for the workspace graph view:
// all item_links rows (parent / blocks / implements / related) plus
// resolved same-workspace wiki-links from the item_wiki_links reverse
// index (PLAN-1593).
//
// Both queries exclude edges whose source or target item is soft-deleted,
// matching GetParentMap's posture (BUG-734) — a dangling edge to an
// archived item would render as a node the items query never returned.
//
// Wiki-link rows are deduplicated per (source, target) pair — an item
// that mentions [[TASK-5]] three times is still one edge — and
// self-links are dropped (an item linking to itself adds noise, not
// structure). Unresolved rows (target_item_id IS NULL) and
// cross-workspace rows are excluded: the graph renders one workspace.
func (s *Store) ListWorkspaceGraphLinks(workspaceID string) ([]GraphLink, error) {
	links := []GraphLink{}

	rows, err := s.db.Query(s.q(`
		SELECT il.source_id, il.target_id, il.link_type
		FROM item_links il
		JOIN items src ON src.id = il.source_id AND src.deleted_at IS NULL
		JOIN items tgt ON tgt.id = il.target_id AND tgt.deleted_at IS NULL
		WHERE il.workspace_id = ?
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list graph item links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var l GraphLink
		if err := rows.Scan(&l.SourceID, &l.TargetID, &l.Type); err != nil {
			return nil, fmt.Errorf("scan graph item link: %w", err)
		}
		links = append(links, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Wiki-link edges. Scoped by the SOURCE item's workspace —
	// item_wiki_links has no workspace_id column of its own, and
	// target_workspace_id is only set on cross-workspace rows (which
	// are excluded here anyway).
	wikiRows, err := s.db.Query(s.q(`
		SELECT DISTINCT wl.source_item_id, wl.target_item_id
		FROM item_wiki_links wl
		JOIN items src ON src.id = wl.source_item_id AND src.deleted_at IS NULL
		JOIN items tgt ON tgt.id = wl.target_item_id AND tgt.deleted_at IS NULL
		WHERE src.workspace_id = ?
		  AND wl.target_item_id IS NOT NULL
		  AND wl.target_workspace_id IS NULL
		  AND wl.source_item_id != wl.target_item_id
	`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list graph wiki links: %w", err)
	}
	defer wikiRows.Close()
	for wikiRows.Next() {
		l := GraphLink{Type: "wiki-link"}
		if err := wikiRows.Scan(&l.SourceID, &l.TargetID); err != nil {
			return nil, fmt.Errorf("scan graph wiki link: %w", err)
		}
		links = append(links, l)
	}
	return links, wikiRows.Err()
}
