package store

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// GraphLink is one typed edge in the workspace graph
// (GET /workspaces/{ws}/graph — PLAN-1730 / TASK-1731). Source and
// target are item IDs; the handler maps them to refs and applies
// visibility filtering before they leave the server.
type GraphLink struct {
	SourceID string
	TargetID string
	Type     string // hyphenated graph edge type — see graphEdgeType
}

// graphEdgeType maps a stored item_links.link_type to the hyphenated
// edge vocabulary the graph API advertises. Stored values are first
// canonicalized through models.NormalizeItemLinkType (handles aliases
// and casing); anything it rejects — possible via the import path,
// which inserts link types without a DB CHECK — degrades to 'related'
// so the advertised enum stays closed while the edge still renders as
// a generic relationship. Synthesized wiki-link edges from
// item_wiki_links use the same 'wiki-link' spelling as stored
// wiki_link rows so the client sees one type.
func graphEdgeType(linkType string) string {
	canonical, err := models.NormalizeItemLinkType(linkType)
	if err != nil {
		return models.ItemLinkTypeRelated
	}
	switch canonical {
	case models.ItemLinkTypeWikiLink:
		return "wiki-link"
	case models.ItemLinkTypeSplitFrom:
		return "split-from"
	default:
		return canonical
	}
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
		l.Type = graphEdgeType(l.Type)
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
	if err := wikiRows.Err(); err != nil {
		return nil, err
	}

	// Dedupe (source, target, type) — a stored wiki_link item_links row
	// and a parsed [[...]] mention of the same pair both normalize to
	// 'wiki-link' and would otherwise emit twice.
	seen := make(map[GraphLink]bool, len(links))
	deduped := links[:0]
	for _, l := range links {
		if seen[l] {
			continue
		}
		seen[l] = true
		deduped = append(deduped, l)
	}
	return deduped, nil
}
