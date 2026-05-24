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
	// Same caching story for title resolution — a doc mentioning
	// `[[Project Goals]]` six times only hits the DB once. Key is
	// the verbatim target_title (case-preserved) because
	// resolveTitleTx already collapses case at SQL time via LOWER();
	// caching by lowercase would also work but slightly mismatches
	// the verbatim-storage contract that the rename hook relies on.
	resolvedTitles := map[string]sql.NullString{}

	for _, link := range extracted {
		// HasDisplay (not Display != "") distinguishes "no pipe"
		// from "pipe with empty display." Mirrors the client
		// renderer's `displayOverride ?? title` semantics, which
		// preserve "". Codex round-12 P3 (Phase 1).
		displayText := sql.NullString{}
		if link.HasDisplay {
			displayText = sql.NullString{String: link.Display, Valid: true}
		}

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

		case links.WikiLinkKindTitle:
			// Phase 2a (TASK-1595). Target_title is stored
			// VERBATIM (case preserved, `/` preserved) so the
			// rename cascade can reconstruct the literal bracket
			// string that's in the source content. Resolution to
			// target_item_id is case-insensitive (mirrors the
			// renderer's `.toLowerCase()` comparison at
			// markdown.ts:543) and lookup order is:
			//   1. full-key match against items.title
			//   2. on miss, split on `/`: collection_slug + title
			// per Codex finding #3 — the renderer tries the
			// whole-key title first, then treats `collection/Title`
			// as a qualified-form fallback only when the whole-key
			// match misses. An item literally titled "tasks/Foo"
			// must resolve before the qualified-form interpretation
			// ever fires.
			cached, ok := resolvedTitles[link.Title]
			if !ok {
				cached = resolveTitleTx(tx, s, workspaceID, link.Title)
				resolvedTitles[link.Title] = cached
			}
			if _, err := tx.Exec(s.q(`
				INSERT INTO item_wiki_links (
					source_item_id, target_kind, target_workspace_id,
					target_item_id, target_ref, target_title,
					display_text, position
				) VALUES (?, ?, NULL, ?, NULL, ?, ?, ?)
			`), sourceItemID, string(links.WikiLinkKindTitle),
				cached, link.Title, displayText, link.Position); err != nil {
				return fmt.Errorf("insert wiki link (title): %w", err)
			}

		default:
			// Phase 2a still skips workspace_ref kinds. The parser's
			// gate prevents them from arriving here; this default
			// branch is a safety net for the day TASK-1597 lifts
			// the gate.
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

// resolveTitleTx mirrors the renderer's title-resolution logic
// (web/src/lib/utils/markdown.ts:541–558) inside the parse-time
// transaction. Two-stage lookup:
//
//  1. Exact case-insensitive match on items.title against the full
//     verbatim title (e.g. "docs/Setup" matches an item literally
//     titled "docs/Setup").
//  2. On miss, if the title contains `/`, split into
//     (collection_slug, remainder) and try the collection-qualified
//     form (e.g. "docs/Setup" → collection slug "docs", title
//     "Setup").
//
// The order matters for items whose titles legitimately contain `/`
// — they must match in stage 1 before stage 2's split-then-lookup
// could ever take a different interpretation. Codex caught this on
// the planning round (finding #3).
//
// Unresolved titles return NULL so the row persists as a broken
// title-link, matching the same broken-row semantics resolveRefTx
// uses for ref-form lookups. Broken titles are intentional — a
// future broken-links report uses them and the rename hook can
// flip them to resolved as items appear / get renamed.
//
// LOWER() is portable across SQLite and Postgres for ASCII case
// folding. For non-ASCII titles both engines fall back to bytewise
// behavior — matching the renderer's `.toLowerCase()`, which is
// also locale-naive (JS default-locale toLowerCase on the V8
// runtime in production targets is effectively ASCII for our
// data). If a future requirement demands Unicode-aware folding,
// it lands as a single helper change here + a matching renderer
// fix; the cross-engine baseline doesn't pretend to do more than
// it does.
func resolveTitleTx(tx *sql.Tx, s *Store, workspaceID, title string) sql.NullString {
	// Stage 1: full-key exact match. LIMIT 1 because the renderer
	// uses Array.find() (first match wins) — we mirror that
	// non-determinism rather than introducing our own ordering.
	var id string
	err := tx.QueryRow(s.q(`
		SELECT id FROM items
		WHERE workspace_id = ?
		  AND deleted_at IS NULL
		  AND LOWER(title) = LOWER(?)
		LIMIT 1
	`), workspaceID, title).Scan(&id)
	if err == nil {
		return sql.NullString{String: id, Valid: true}
	}
	if err != sql.ErrNoRows {
		// Real DB error — return NULL so the row persists as
		// unresolved (same conservative posture as resolveRefTx).
		return sql.NullString{}
	}

	// Stage 2: collection-qualified fallback. Only applies when the
	// title contains a `/`. Split on the FIRST `/` because an item's
	// title can legitimately contain additional slashes after the
	// collection delimiter (e.g. "docs/api/auth-flow"). The renderer
	// at markdown.ts:548 uses `key.split('/')` then `rest.join('/')`
	// — equivalent to "split once, keep the rest verbatim."
	slash := strings.IndexByte(title, '/')
	if slash <= 0 || slash >= len(title)-1 {
		// No `/`, or empty side — no qualified form to try.
		return sql.NullString{}
	}
	collSlug := title[:slash]
	titleRest := title[slash+1:]
	err = tx.QueryRow(s.q(`
		SELECT i.id FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.workspace_id = ?
		  AND c.slug = ?
		  AND i.deleted_at IS NULL
		  AND LOWER(i.title) = LOWER(?)
		LIMIT 1
	`), workspaceID, collSlug, titleRest).Scan(&id)
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: id, Valid: true}
}

// cascadeTitleRename keeps title-form backlinks consistent when an
// item's title changes. Two effects to maintain inside the rename tx:
//
//  1. Sources that ALREADY point at the renamed item via a title-form
//     link have `[[oldTitle]]` (or `[[<coll>/oldTitle]]`) literal in
//     their content. The renderer would no longer resolve those after
//     the rename, breaking the user's click target. Rewrite each
//     source's content via links.ReplaceTitle (matches the document
//     rename behavior — see documents.go::updateLinksInTx), re-stamp
//     updated_at + content_flushed_at, bump the workspace seq, and
//     re-run replaceWikiLinks so the source's own index rows refresh
//     with the new literal AND the new target_item_id resolution.
//
//  2. Sources that ALREADY contain `[[newTitle]]` (or
//     `[[<coll>/newTitle]]`) in their content but whose index rows
//     were stored as broken (target_item_id IS NULL, because at parse
//     time no item had that title) need to flip to RESOLVED. No
//     content change required — only an UPDATE of item_wiki_links.
//
// Runs inside the rename tx so a single failure rolls back the whole
// rename: either every dependent is consistent with the new title or
// the rename never happened. Self-references (the renamed item links
// to itself by its own title) are filtered out — the renamed item's
// own content gets updated by the surrounding UPDATE statement, not
// by this cascade.
//
// PLAN-1593 / TASK-1595.
func (s *Store) cascadeTitleRename(tx *sql.Tx, renamedItemID, workspaceID, oldTitle, newTitle string) error {
	if oldTitle == newTitle {
		return nil
	}

	// Look up the renamed item's collection_slug so the qualified-form
	// matches (`[[<slug>/oldTitle]]`) get the same cascade treatment.
	// A collection-move during a title rename is impossible in the
	// API (UpdateItem doesn't move collections), so reading the slug
	// from the current row is safe — it's the slug both before and
	// after the rename.
	var collSlug string
	if err := tx.QueryRow(s.q(`
		SELECT c.slug FROM items i
		JOIN collections c ON c.id = i.collection_id
		WHERE i.id = ?
	`), renamedItemID).Scan(&collSlug); err != nil {
		return fmt.Errorf("cascade rename: lookup collection slug: %w", err)
	}

	// (1) Cascade content rewrites. Pull every source currently
	// pointing at the renamed item via a title-form link. The
	// target_workspace_id IS NULL filter excludes Phase-2b
	// cross-workspace rows once those exist — cross-ws cascade is a
	// separate concern (TASK-1597 owns it). source_item_id !=
	// renamedItemID drops self-references; the renamed item's own
	// content rewrite is handled by the outer UPDATE in items.go.
	rows, err := tx.Query(s.q(`
		SELECT DISTINCT s.id, s.content, s.workspace_id
		FROM item_wiki_links wl
		JOIN items s ON s.id = wl.source_item_id
		WHERE wl.target_kind = 'title'
		  AND wl.target_workspace_id IS NULL
		  AND wl.target_item_id = ?
		  AND s.deleted_at IS NULL
		  AND s.id != ?
	`), renamedItemID, renamedItemID)
	if err != nil {
		return fmt.Errorf("cascade rename: scan sources: %w", err)
	}
	type source struct {
		id, content, workspaceID string
	}
	var sources []source
	for rows.Next() {
		var src source
		if err := rows.Scan(&src.id, &src.content, &src.workspaceID); err != nil {
			rows.Close()
			return fmt.Errorf("cascade rename: scan source row: %w", err)
		}
		sources = append(sources, src)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("cascade rename: iterate sources: %w", err)
	}
	rows.Close()

	// Rewrite each source's content via links.RewriteWikiTitle, which
	// handles all four title-form shapes — plain / aliased / qualified
	// / qualified+aliased — with case-insensitive title matching that
	// mirrors resolveTitleTx's resolution rule. Codex round 1 P1
	// against this PR caught that a literal ReplaceAll on `[[Old]]`
	// silently dropped `[[Old|alias]]` and `[[old]]` (mixed case)
	// rows: they were selected via target_item_id (correctly) but
	// the rewrite missed them, and the trailing re-parse then sees
	// the same body, fails to resolve under the new title, and
	// converts the row to broken — exactly the regression the
	// cascade exists to prevent.
	//
	// The rewriter does NOT distinguish code regions from prose, so
	// occurrences inside fenced/inline code get rewritten too. The
	// extractor ignores code regions on indexing, but for rename-
	// rewrite the code-aware alternative doubles the surface area
	// for a corner case (user mentioning the renamed item inside a
	// code sample). Matches the document-rename path's behavior;
	// acceptable for v2.
	ts := now()
	for _, src := range sources {
		newContent := links.RewriteWikiTitle(src.content, oldTitle, newTitle, collSlug)
		if newContent == src.content {
			// Source had a target_item_id row pointing at us but
			// the rewriter found no literal occurrences — possible
			// if a stale row was inserted by an earlier-version
			// parser, or the body uses a title-escape form
			// (RewriteWikiTitle's documented limitation). Re-parse
			// without touching content so the index converges; the
			// row will flip to broken if no clickable match
			// remains, matching the renderer's behavior on a body
			// that would no longer render as a link.
			if err := s.replaceWikiLinks(tx, src.id, src.workspaceID, src.content); err != nil {
				return fmt.Errorf("cascade rename: reparse %s: %w", src.id, err)
			}
			continue
		}
		if _, err := tx.Exec(s.q(`
			UPDATE items
			SET content = ?,
			    updated_at = ?,
			    content_flushed_at = ?,
			    seq = `+nextWorkspaceSeqSubquery+`
			WHERE id = ?
		`), newContent, ts, ts, src.workspaceID, src.id); err != nil {
			return fmt.Errorf("cascade rename: update source %s: %w", src.id, err)
		}
		if err := s.replaceWikiLinks(tx, src.id, src.workspaceID, newContent); err != nil {
			return fmt.Errorf("cascade rename: reparse %s: %w", src.id, err)
		}
	}

	// (2) Flip newly-resolvable broken rows under the NEW title.
	return s.resolveBrokenTitleLinks(tx, renamedItemID, workspaceID, collSlug, newTitle)
}

// resolveBrokenTitleLinks flips item_wiki_links rows in the workspace
// that have target_kind='title', target_item_id IS NULL, and a
// target_title matching the given (collSlug, title) — case-insensitive
// — to point at the supplied itemID. Called from two places:
//
//   - cascadeTitleRename after a title rename, to pick up any
//     pre-existing `[[newTitle]]` sources that were stored as broken
//     at the time their author wrote them.
//   - tryCreateItem after a new item lands, so any pre-existing
//     `[[Title]]` sources that were waiting for an item with this
//     title resolve immediately (rather than waiting for a backfill
//     run or a content rewrite).
//
// Two UPDATEs (one per shape — plain vs collection-qualified) instead
// of one with OR so the partial indexes on target_title can each be
// used independently. Both queries usually update 0 rows; the second
// is essentially free for items whose collection slug doesn't appear
// in any source body.
func (s *Store) resolveBrokenTitleLinks(tx *sql.Tx, itemID, workspaceID, collSlug, title string) error {
	plainTitleNorm := strings.ToLower(title)
	qualifiedTitleNorm := strings.ToLower(collSlug + "/" + title)
	if _, err := tx.Exec(s.q(`
		UPDATE item_wiki_links
		SET target_item_id = ?
		WHERE target_kind = 'title'
		  AND target_workspace_id IS NULL
		  AND target_item_id IS NULL
		  AND LOWER(target_title) = ?
		  AND source_item_id IN (
		      SELECT id FROM items WHERE workspace_id = ? AND deleted_at IS NULL
		  )
	`), itemID, plainTitleNorm, workspaceID); err != nil {
		return fmt.Errorf("resolve broken plain titles: %w", err)
	}
	if _, err := tx.Exec(s.q(`
		UPDATE item_wiki_links
		SET target_item_id = ?
		WHERE target_kind = 'title'
		  AND target_workspace_id IS NULL
		  AND target_item_id IS NULL
		  AND LOWER(target_title) = ?
		  AND source_item_id IN (
		      SELECT id FROM items WHERE workspace_id = ? AND deleted_at IS NULL
		  )
	`), itemID, qualifiedTitleNorm, workspaceID); err != nil {
		return fmt.Errorf("resolve broken qualified titles: %w", err)
	}
	return nil
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
