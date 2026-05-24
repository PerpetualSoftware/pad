package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/links"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// snippetRadius is how many bytes either side of a link's position
// the snippet captures. ~80 chars total feels right for a one-line
// "Mentioned in" panel row; the renderer can trim further or expand
// on click.
const snippetRadius = 40

// replaceWikiLinks deletes prior wiki-link rows for `sourceItemID`
// and inserts fresh ones extracted from `content`. Resolution
// against the workspace's items happens here (Phase 1: ref-kind
// only) so the backlinks query is a single index hit.
//
// Must run inside `tx` — the caller owns transactionality. Callers
// from CreateItem / UpdateItem are already inside their write
// transactions; the backfill caller wraps its own per-source tx.
//
// Idempotent: calling with the same (sourceItemID, content) twice
// yields the same final row set.
func (s *Store) replaceWikiLinks(tx *sql.Tx, sourceItemID, workspaceID, content string) error {
	// Delete first so callers don't need to pre-clear. This is the
	// canonical "re-parse this item" path; an empty content body
	// correctly leaves zero rows behind.
	if _, err := tx.Exec(s.q(`DELETE FROM item_wiki_links WHERE source_item_id = ?`), sourceItemID); err != nil {
		return fmt.Errorf("delete prior wiki links: %w", err)
	}
	if content == "" {
		return nil
	}

	extracted := links.ExtractWikiLinks(content)
	if len(extracted) == 0 {
		return nil
	}

	// Resolve refs to target_item_id within the same workspace. We
	// build a per-(prefix, number) cache so repeat references in
	// the same body (`[[TASK-5]]` mentioned three times) don't
	// re-query. The cache scope is one call to replaceWikiLinks
	// since target item IDs can't change during one source-item
	// write transaction in any way that would matter for
	// resolution.
	type refKey struct {
		prefix string
		number int
	}
	resolved := map[refKey]sql.NullString{}

	for _, link := range extracted {
		switch link.Kind {
		case links.WikiLinkKindRef:
			prefix, number, ok := splitRef(link.Ref)
			if !ok {
				// parseBody already vetted the shape but defensive
				// in case the constants ever drift.
				continue
			}
			key := refKey{prefix: prefix, number: number}
			targetID, cached := resolved[key]
			if !cached {
				targetID = resolveRefTx(tx, s, workspaceID, prefix, number)
				resolved[key] = targetID
			}
			// HasDisplay (not Display != "") distinguishes "no
			// pipe" from "pipe with empty display." Mirrors the
			// client renderer's `displayOverride ?? title`
			// semantics, which preserve "". Codex round-12 P3.
			displayText := sql.NullString{}
			if link.HasDisplay {
				displayText = sql.NullString{String: link.Display, Valid: true}
			}
			if _, err := tx.Exec(s.q(`
				INSERT INTO item_wiki_links (
					source_item_id, target_kind, target_workspace_id,
					target_item_id, target_ref, target_title,
					display_text, position
				) VALUES (?, ?, NULL, ?, ?, NULL, ?, ?)
			`), sourceItemID, string(links.WikiLinkKindRef),
				targetID, link.Ref, displayText, link.Position); err != nil {
				return fmt.Errorf("insert wiki link: %w", err)
			}
		default:
			// Phase 1 skips title and workspace_ref kinds. The
			// parser's gate should already prevent them from
			// arriving here; this default branch is a safety
			// net for the day Phase 2 lifts the gate.
			continue
		}
	}
	return nil
}

// resolveRefTx looks up a (prefix, number) pair to an item ID within
// a workspace. Returns a NULL sql.NullString if the ref doesn't
// resolve — broken refs intentionally persist as unresolved rows
// (PLAN-1593's "broken refs persisted" decision), so they're easy
// to surface in a future broken-links report.
//
// Tries the exact prefix+number match first, then falls back to a
// number-only match (matching GetItemByRef's behavior so an item
// that has been moved between collections still resolves under its
// old prefix).
func resolveRefTx(tx *sql.Tx, s *Store, workspaceID, prefix string, number int) sql.NullString {
	var id string
	err := tx.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ? AND c.prefix = ? AND i.item_number = ?
		  AND i.deleted_at IS NULL
	`), workspaceID, prefix, number).Scan(&id)
	if err == nil {
		return sql.NullString{String: id, Valid: true}
	}
	if err != sql.ErrNoRows {
		// A real DB error during resolution is rare and not worth
		// failing the whole item write over — log-and-skip would
		// be ideal but we don't have a logger handle here. Return
		// NULL so the row persists as unresolved; if the error
		// repeats on every write the broken-links report (Phase 3+)
		// will surface it.
		return sql.NullString{}
	}
	// Number-only fallback for the cross-collection-move case.
	err = tx.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ? AND item_number = ? AND deleted_at IS NULL
	`), workspaceID, number).Scan(&id)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: id, Valid: true}
}

// splitRef parses "TASK-5" into ("TASK", 5). The trailing -<number>
// is required; everything before the last '-' is the prefix.
// Returns ok=false on shapes ExtractWikiLinks shouldn't produce
// (defensive — keeps the helper robust to future parser changes).
func splitRef(ref string) (prefix string, number int, ok bool) {
	dash := strings.LastIndexByte(ref, '-')
	if dash <= 0 || dash >= len(ref)-1 {
		return "", 0, false
	}
	prefix = ref[:dash]
	n, err := strconv.Atoi(ref[dash+1:])
	if err != nil {
		return "", 0, false
	}
	return prefix, n, true
}

// BacklinksVisibility is the per-call visibility scope GetBacklinks
// applies in SQL. Mirrors the (fullCollIDs, grantedItemIDs) shape
// `Server.guestResourceFilter` returns so callers can pass the
// primitives straight through.
//
// Semantics:
//
//   - Unrestricted == true: no visibility filter; the caller has
//     full read access to the workspace (admin / full-access
//     member / root-scoped token). FullCollectionIDs and
//     GrantedItemIDs are ignored.
//
//   - Unrestricted == false: a source row is visible iff
//     `source.collection_id IN FullCollectionIDs` OR
//     `source.id IN GrantedItemIDs`. Both empty means "see nothing"
//     — the query short-circuits to an empty result.
//
// Pushing both lists into SQL (rather than post-filtering in Go) is
// what makes LIMIT/OFFSET pagination correct for guests / restricted
// members. Codex review of TASK-1594 round-1 P1 + round-2 P1.
type BacklinksVisibility struct {
	Unrestricted      bool
	FullCollectionIDs []string
	GrantedItemIDs    []string
}

// GetBacklinks returns the items in `workspaceID` that contain a
// resolved `[[...]]` reference to `targetItemID`. Phase 1 only
// returns ref-kind backlinks (the only kind the parser indexes); the
// query JOINs against items.deleted_at IS NULL so soft-deleted
// sources don't surface.
//
// Self-links are filtered out at query time — an item that mentions
// its own title in its own body shouldn't appear in its own
// "Mentioned in" panel (PLAN-1593 behavior decision). The row stays
// in the index for completeness; the filter is purely cosmetic.
//
// Ordering: most-recently-updated source first; within an updated_at
// tie (e.g. two backlinks land in the same second), break by
// position ASC so the order is at least deterministic.
//
// `limit` and `offset` paginate. A limit <=0 is normalized to a
// hard cap (300) so a buggy caller can't ask for unbounded results.
//
// `vis` constrains source visibility in SQL — see BacklinksVisibility.
// Filtering in SQL is essential for pagination correctness under
// restricted access: LIMIT/OFFSET counts visible rows, not raw rows.
func (s *Store) GetBacklinks(targetItemID, workspaceID string, limit, offset int, vis BacklinksVisibility) ([]models.Backlink, error) {
	if limit <= 0 || limit > 300 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	// Restricted + nothing-to-see → empty up-front. Avoids issuing
	// a `WHERE … IN ()` query (Postgres rejects the empty-list
	// form; SQLite tolerates it but matches nothing anyway).
	if !vis.Unrestricted && len(vis.FullCollectionIDs) == 0 && len(vis.GrantedItemIDs) == 0 {
		return nil, nil
	}

	// Build the visibility predicate. Unrestricted → omit. Otherwise
	// `collection_id IN (...)` OR `id IN (...)` — either branch alone
	// is acceptable so a granted-item-only access still resolves; an
	// empty IN-list is replaced with a sentinel "IN (NULL)" so the
	// predicate evaluates to FALSE for that branch without breaking
	// Postgres's empty-list rejection.
	visClause := ""
	args := []interface{}{targetItemID, workspaceID, targetItemID}
	if !vis.Unrestricted {
		collClause := "FALSE"
		if len(vis.FullCollectionIDs) > 0 {
			placeholders := make([]string, len(vis.FullCollectionIDs))
			for i, cid := range vis.FullCollectionIDs {
				placeholders[i] = "?"
				args = append(args, cid)
			}
			collClause = "s.collection_id IN (" + strings.Join(placeholders, ",") + ")"
		}
		itemClause := "FALSE"
		if len(vis.GrantedItemIDs) > 0 {
			placeholders := make([]string, len(vis.GrantedItemIDs))
			for i, iid := range vis.GrantedItemIDs {
				placeholders[i] = "?"
				args = append(args, iid)
			}
			itemClause = "s.id IN (" + strings.Join(placeholders, ",") + ")"
		}
		visClause = " AND (" + collClause + " OR " + itemClause + ")"
	}
	args = append(args, limit, offset)

	rows, err := s.db.Query(s.q(`
		SELECT s.id, c.prefix, s.item_number, s.title, c.slug, c.icon,
		       s.content, wl.position, wl.display_text, s.updated_at
		FROM item_wiki_links wl
		JOIN items s       ON s.id = wl.source_item_id
		JOIN collections c ON c.id = s.collection_id
		WHERE wl.target_item_id = ?
		  AND s.workspace_id = ?
		  AND s.deleted_at IS NULL
		  AND s.id != ?`+visClause+`
		ORDER BY s.updated_at DESC, wl.position ASC
		LIMIT ? OFFSET ?
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("query backlinks: %w", err)
	}
	defer rows.Close()

	var out []models.Backlink
	for rows.Next() {
		var (
			sourceID, prefix, title, collSlug, collIcon, content, updatedAt string
			itemNumber                                                      int
			position                                                        int
			displayText                                                     sql.NullString
		)
		if err := rows.Scan(&sourceID, &prefix, &itemNumber, &title, &collSlug, &collIcon, &content, &position, &displayText, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan backlink row: %w", err)
		}
		bl := models.Backlink{
			SourceItemID:         sourceID,
			SourceRef:            formatRef(prefix, itemNumber),
			SourceTitle:          title,
			SourceCollectionSlug: collSlug,
			SourceCollectionIcon: collIcon,
			Snippet:              snippetAround(content, position),
			UpdatedAt:            updatedAt,
		}
		if displayText.Valid {
			// Pointer-typed so the JSON wire shape preserves the
			// nil-vs-empty-string distinction even when the
			// override is "". Codex round-13 P2.
			s := displayText.String
			bl.DisplayText = &s
		}
		out = append(out, bl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate backlinks: %w", err)
	}
	return out, nil
}

// formatRef rebuilds "PREFIX-NUMBER" from its parts. Kept inline
// rather than reaching for a models package helper to avoid an
// import cycle from store→models→store via the dialect helpers.
func formatRef(prefix string, number int) string {
	return prefix + "-" + strconv.Itoa(number)
}

// snippetAround returns roughly `snippetRadius*2` bytes of `content`
// centered on `position` (the start of the `[[`). Caller doesn't
// need the snippet to be exact — the UI usually further trims to
// fit a single line — but we DO trim at rune boundaries so a
// multi-byte rune at the cut point doesn't produce invalid UTF-8.
//
// Empty content yields empty snippet.
func snippetAround(content string, position int) string {
	if content == "" {
		return ""
	}
	n := len(content)
	if position < 0 {
		position = 0
	}
	if position > n {
		position = n
	}
	start := position - snippetRadius
	if start < 0 {
		start = 0
	}
	end := position + snippetRadius
	if end > n {
		end = n
	}
	// Trim back to a rune boundary at the start so we don't slice
	// mid-codepoint. UTF-8 continuation bytes have the bit pattern
	// 10xxxxxx (i.e. (b & 0xC0) == 0x80); advance past them to land
	// on a leading byte (or ASCII).
	for start < n && (content[start]&0xC0) == 0x80 {
		start++
	}
	// Same treatment for `end`: if the cut lands inside a multi-byte
	// rune, advance forward past the continuation bytes so we don't
	// emit invalid UTF-8. Going forward (not backward) keeps the
	// snippet anchored slightly past the link rather than slightly
	// before it. Codex round-8 P3.
	for end < n && (content[end]&0xC0) == 0x80 {
		end++
	}
	snippet := content[start:end]
	// Collapse internal newlines so the one-line UI doesn't have to.
	snippet = strings.ReplaceAll(snippet, "\n", " ")
	// Add an ellipsis hint when we truncated on either side.
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < n {
		snippet = snippet + "…"
	}
	return snippet
}
