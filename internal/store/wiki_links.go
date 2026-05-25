package store

import (
	"database/sql"
	"fmt"
	"sort"
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
	// Per-call cache for workspace slug → ID lookups. A body that
	// references the same foreign workspace multiple times
	// (`[[claude::TASK-1]]`, `[[claude::TASK-2]]`) only hits the
	// workspaces table once. Empty slug map is normal — most bodies
	// don't contain cross-workspace refs.
	resolvedWorkspaces := map[string]sql.NullString{}

	for _, link := range extracted {
		// HasDisplay (not Display != "") distinguishes "no pipe"
		// from "pipe with empty display." Mirrors the client
		// renderer's `displayOverride ?? title` semantics, which
		// preserve "". Codex round-12 P3 (Phase 1).
		displayText := sql.NullString{}
		if link.HasDisplay {
			displayText = sql.NullString{String: link.Display, Valid: true}
		}

		// Same-workspace fully-qualified refs: `[[<this-slug>::REF]]`.
		// The renderer's L472-481 short-circuit:
		//   - Same-workspace qualified form is treated like `[[REF]]`
		//     for the REF LOOKUP step.
		//   - BUT on ref miss, the renderer leaves the wiki-link
		//     verbatim (broken) — it does NOT fall through to title
		//     lookup like a bare `[[REF]]` would (L513).
		//
		// So we handle these inline as ref-kind rows WITHOUT the
		// title-fallback path: resolved → ref row, unresolved →
		// broken ref row. Skipping the switch ensures we never
		// reach the WikiLinkKindRef branch's title-fallback logic.
		// Codex round 5 P2 caught the round-4 normalization being
		// over-aggressive — it promoted to Kind=Ref and let title
		// fallback create ghost backlinks the renderer wouldn't
		// produce.
		if link.Kind == links.WikiLinkKindWorkspaceRef {
			cachedWS, hit := resolvedWorkspaces[link.WorkspaceSlug]
			if !hit {
				cachedWS = resolveWorkspaceSlugTx(tx, s, link.WorkspaceSlug)
				resolvedWorkspaces[link.WorkspaceSlug] = cachedWS
			}
			if cachedWS.Valid && cachedWS.String == workspaceID {
				canonicalRef := links.CanonicalizeRef(link.Ref)
				prefix, number, ok := splitRef(canonicalRef)
				if !ok {
					// parseBody vetted the shape — defensive skip.
					continue
				}
				key := refKey{prefix: prefix, number: number}
				targetID, cached := resolved[key]
				if !cached {
					targetID = resolveRefTx(tx, s, workspaceID, prefix, number)
					resolved[key] = targetID
				}
				// Insert as ref-kind row. NO title fallback here —
				// the renderer's same-ws qualified path doesn't
				// have one, so we mirror that (a row with
				// target_item_id=NULL persists as a broken ref).
				if _, err := tx.Exec(s.q(`
					INSERT INTO item_wiki_links (
						source_item_id, target_kind, target_workspace_id,
						target_item_id, target_ref, target_title,
						display_text, position
					) VALUES (?, ?, NULL, ?, ?, NULL, ?, ?)
				`), sourceItemID, string(links.WikiLinkKindRef),
					targetID, canonicalRef, displayText, link.Position); err != nil {
					return fmt.Errorf("insert wiki link (same-ws qualified ref): %w", err)
				}
				continue
			}
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
			if targetID.Valid {
				// Ref resolved — store as a ref-kind row.
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
				continue
			}
			// Ref didn't resolve — try title fallback. Mirrors the
			// renderer's "If the ref doesn't resolve we FALL THROUGH
			// to the legacy title path" at web/src/lib/utils/markdown.ts:513.
			// Order matches the renderer:
			//   (a) If HasDisplay, try FULL body (RawKey+"|"+Display)
			//       as a title — covers literal-pipe titles like
			//       "ISO-9001|Spec" matching `[[ISO-9001|Spec]]`.
			//       Codex round 7 P1.
			//   (b) Try the raw key string as a title — covers
			//       `[[ISO-9001]]` matching an item titled "ISO-9001".
			//       Codex round 6 P1.
			//
			// Use link.RawKey (untrimmed unescaped key) NOT link.Ref
			// (canonical trimmed) for title fallback so an item
			// literally titled `" TASK-5 "` resolves the same way
			// the renderer does. Codex round 10 P2.
			rawKey := link.RawKey
			if rawKey == "" {
				rawKey = link.Ref // defensive — shouldn't happen for ref kind
			}
			titleCandidates := []string{rawKey}
			storedTitleForFallback := rawKey
			storeDisplayForFallback := displayText
			if link.HasDisplay {
				fullBody := rawKey + "|" + link.Display
				titleCandidates = []string{fullBody, rawKey}
			}
			var titleHit sql.NullString
			for i, candidate := range titleCandidates {
				cached, ok := resolvedTitles[candidate]
				if !ok {
					cached = resolveTitleTx(tx, s, workspaceID, candidate)
					resolvedTitles[candidate] = cached
				}
				if cached.Valid {
					titleHit = cached
					storedTitleForFallback = candidate
					// If stage (a) matched (i==0 with pipe), the
					// display segment was absorbed into the title.
					if i == 0 && link.HasDisplay {
						storeDisplayForFallback = sql.NullString{}
					}
					break
				}
			}
			if titleHit.Valid {
				if _, err := tx.Exec(s.q(`
					INSERT INTO item_wiki_links (
						source_item_id, target_kind, target_workspace_id,
						target_item_id, target_ref, target_title,
						display_text, position
					) VALUES (?, ?, NULL, ?, NULL, ?, ?, ?)
				`), sourceItemID, string(links.WikiLinkKindTitle),
					titleHit, storedTitleForFallback, storeDisplayForFallback, link.Position); err != nil {
					return fmt.Errorf("insert wiki link (ref→title fallback): %w", err)
				}
				continue
			}
			// Both ref and title missed — store as broken ref-kind
			// row (the original interpretation). A future ref-item
			// creation will still find this row via the ref-shaped
			// target_ref; a future title-item creation won't
			// auto-retarget (target_kind='ref' so
			// resolveBrokenTitleLinks misses it), which is a known
			// limitation v3 can revisit.
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
			// Phase 2a (TASK-1595). Target_title is stored VERBATIM
			// in WHATEVER FORM RESOLVED so the rename cascade can
			// reconstruct the literal bracket string in source
			// content. Resolution mirrors the renderer's order at
			// web/src/lib/utils/markdown.ts:516-558:
			//
			//   (a) If a pipe was present (HasDisplay), try the
			//       FULL body (Title + "|" + Display) as a title.
			//       Handles legacy items whose title literally
			//       contains a `|` — `[[A|B]]` rendered as a link
			//       to the item titled "A|B" before falling back
			//       to the split interpretation. Codex round 3 P1
			//       (this PR) caught the parity gap.
			//   (b) The split key (Title alone) as a title.
			//
			// Stage (a)'s match stores target_title=fullBody and
			// drops the display override (the "display" segment
			// was actually part of the title). Stage (b) is the
			// common case: target_title=Title, display preserved.
			//
			// Within each stage, resolveTitleTx applies the
			// renderer's two-step lookup (full-key literal first,
			// `/`-split fallback on miss) for the SAME case-
			// insensitive correctness reasons.
			candidates := []string{link.Title}
			storeDisplay := displayText
			fullBody := link.Title // == split key when !HasDisplay
			if link.HasDisplay {
				// Stage (a) first, then stage (b).
				fullBody = link.Title + "|" + link.Display
				candidates = []string{fullBody, link.Title}
			}
			var resolved sql.NullString
			storedTitle := link.Title // default if nothing resolves
			for i, candidate := range candidates {
				cached, ok := resolvedTitles[candidate]
				if !ok {
					cached = resolveTitleTx(tx, s, workspaceID, candidate)
					resolvedTitles[candidate] = cached
				}
				if cached.Valid {
					resolved = cached
					storedTitle = candidate
					// If stage (a) matched (i==0 with a pipe in
					// the candidate), the "display" was consumed
					// into the title — don't double-store it.
					if i == 0 && link.HasDisplay {
						storeDisplay = sql.NullString{}
					}
					break
				}
			}
			// Codex round 4: when nothing resolved AND there was a
			// pipe, key the broken row on the FULL BODY rather than
			// the split key. The renderer's preferred interpretation
			// for `[[A|B]]` is "title A|B" (markdown.ts:516); if an
			// item literally titled "A|B" arrives later,
			// resolveBrokenTitleLinks needs target_title="A|B" to
			// find this row. Storing the split key would orphan it
			// forever (no path retargets "A" → "A|B"). The
			// remaining asymmetry — a broken row keyed on full body
			// won't pick up a future split-fallback resolution to a
			// new item titled "A" — is documented as a v3-promotable
			// limitation. The full-body path is the renderer's
			// preferred interpretation, so prioritizing it is the
			// right tradeoff in the rare case both paths apply.
			if !resolved.Valid && link.HasDisplay {
				storedTitle = fullBody
				storeDisplay = sql.NullString{}
			}
			if _, err := tx.Exec(s.q(`
				INSERT INTO item_wiki_links (
					source_item_id, target_kind, target_workspace_id,
					target_item_id, target_ref, target_title,
					display_text, position
				) VALUES (?, ?, NULL, ?, NULL, ?, ?, ?)
			`), sourceItemID, string(links.WikiLinkKindTitle),
				resolved, storedTitle, storeDisplay, link.Position); err != nil {
				return fmt.Errorf("insert wiki link (title): %w", err)
			}

		case links.WikiLinkKindWorkspaceRef:
			// Phase 2b (TASK-1597). Resolve the workspace slug to a
			// workspace ID; the ref is stored verbatim and resolution
			// to a target_item_id is INTENTIONALLY deferred to query
			// time (cross-workspace target resolution is the cross-ws
			// inbound query's responsibility — see GetCrossWorkspaceBacklinks
			// — because it has to honor the foreign workspace's ACLs
			// per the requesting user, which write-time can't
			// pre-compute).
			//
			// Unknown workspace slugs persist with target_workspace_id
			// = NULL — broken-link semantics, identical to the way
			// broken ref-form rows persist with target_item_id=NULL.
			// The renderer's resolver-route fallback (`/-/r/<ws>/<ref>`
			// at markdown.ts:485) returns 404 in that case; the index
			// matches that behavior.
			cachedWS, hit := resolvedWorkspaces[link.WorkspaceSlug]
			if !hit {
				cachedWS = resolveWorkspaceSlugTx(tx, s, link.WorkspaceSlug)
				resolvedWorkspaces[link.WorkspaceSlug] = cachedWS
			}
			if _, err := tx.Exec(s.q(`
				INSERT INTO item_wiki_links (
					source_item_id, target_kind, target_workspace_id,
					target_item_id, target_ref, target_title,
					display_text, position
				) VALUES (?, ?, ?, NULL, ?, NULL, ?, ?)
			`), sourceItemID, string(links.WikiLinkKindWorkspaceRef),
				cachedWS, link.Ref, displayText, link.Position); err != nil {
				return fmt.Errorf("insert wiki link (workspace_ref): %w", err)
			}

		default:
			// All recognized kinds are handled above; this default
			// branch catches any future kind added to the WikiLinkKind
			// enum without a corresponding store branch.
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

// resolveWorkspaceSlugTx looks up `slug` against the workspaces
// table and returns the workspace ID as a sql.NullString. Returns
// a NULL when the slug doesn't match any live workspace — the same
// broken-link persistence pattern resolveRefTx and resolveTitleTx
// use (PLAN-1593's "broken refs persisted" decision).
//
// Deleted workspaces are excluded (deleted_at IS NULL) so a recently
// deleted workspace's slug doesn't resolve to its stale ID. A real
// DB error returns NULL so the write doesn't abort — same
// conservative posture as the sister helpers.
func resolveWorkspaceSlugTx(tx *sql.Tx, s *Store, slug string) sql.NullString {
	if slug == "" {
		return sql.NullString{}
	}
	var id string
	err := tx.QueryRow(s.q(`
		SELECT id FROM workspaces
		WHERE slug = ? AND deleted_at IS NULL
	`), slug).Scan(&id)
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
//
// `excludeSelf` controls whether the renamed item's own body is part
// of the cascade:
//
//   - false (title-only rename) — INCLUDE self. The user didn't
//     touch content, so any pre-existing self-ref `[[Old Title]]`
//     would go stale without rewrite. Cascade refreshes it.
//   - true (title + content rename) — EXCLUDE self. The user is
//     actively rewriting content in the same call; their submission
//     is authoritative. Mirrors documents.go::updateLinksInTx's
//     pattern of leaving the renamed entity's own content alone.
//     If their new content contains `[[Old Title]]`, the trailing
//     replaceWikiLinks correctly stores it as broken — same as the
//     renderer would render it.
func (s *Store) cascadeTitleRename(tx *sql.Tx, renamedItemID, workspaceID, oldTitle, newTitle string, excludeSelf bool) error {
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

	// (1) Cascade content rewrites — POSITION-BASED. SELECT each
	// individual wl row (not DISTINCT sources) along with its
	// position + target_title. Each row tells us EXACTLY which
	// bracket in the source content corresponds to a link to the
	// renamed item; we rewrite only those brackets, leaving any
	// unrelated literal-pipe `[[OldTitle|alias]]` brackets that
	// happened to mention items B/C with overlapping titles alone.
	//
	// Codex round 7 finding 2 caught the prior broad-regex
	// approach corrupting unrelated brackets: items A "Old Title"
	// and B "Old Title|alias" both referenced from one source, A
	// renamed, RewriteWikiTitle's pattern (?i:Old Title) matched
	// both A's `[[Old Title]]` row AND B's `[[Old Title|alias]]`
	// bracket (even though B's wl row points at B, not A). The
	// position-based approach restricts the rewrite to exactly the
	// brackets the cascade SELECT actually returned.
	//
	// target_workspace_id IS NULL filter excludes Phase-2b cross-
	// workspace rows (TASK-1597 owns those). Self-references
	// INCLUDED so a renamed item's own body stays consistent
	// (Codex round 5 finding 2); GetBacklinks filters self-links
	// at query time so the panel behavior is unchanged.
	//
	// ORDER BY source_id, position DESC: descending position so
	// rewrites at later byte offsets don't shift the offsets of
	// earlier rows in the same source.
	selectQuery := `
		SELECT s.id, s.content, s.workspace_id, wl.position, wl.target_title
		FROM item_wiki_links wl
		JOIN items s ON s.id = wl.source_item_id
		WHERE wl.target_kind = 'title'
		  AND wl.target_workspace_id IS NULL
		  AND wl.target_item_id = ?
		  AND s.deleted_at IS NULL`
	queryArgs := []interface{}{renamedItemID}
	if excludeSelf {
		selectQuery += " AND s.id != ?"
		queryArgs = append(queryArgs, renamedItemID)
	}
	selectQuery += " ORDER BY s.id, wl.position DESC"
	rows, err := tx.Query(s.q(selectQuery), queryArgs...)
	if err != nil {
		return fmt.Errorf("cascade rename: scan sources: %w", err)
	}
	type rowInfo struct {
		position    int
		targetTitle string
	}
	type sourceWork struct {
		id, content, workspaceID string
		rows                     []rowInfo
	}
	var works []sourceWork
	cursor := -1
	for rows.Next() {
		var (
			id, content, workspaceID string
			position                 int
			targetTitle              string
		)
		if err := rows.Scan(&id, &content, &workspaceID, &position, &targetTitle); err != nil {
			rows.Close()
			return fmt.Errorf("cascade rename: scan source row: %w", err)
		}
		if cursor < 0 || works[cursor].id != id {
			works = append(works, sourceWork{id: id, content: content, workspaceID: workspaceID})
			cursor = len(works) - 1
		}
		works[cursor].rows = append(works[cursor].rows, rowInfo{position: position, targetTitle: targetTitle})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("cascade rename: iterate sources: %w", err)
	}
	rows.Close()

	// Per source: walk its rows (positions descending), rewrite each
	// bracket via links.RewriteBracketAt, then UPDATE the source's
	// content + re-parse to refresh the index.
	ts := now()
	for _, work := range works {
		newContent := work.content
		mutated := false
		for _, r := range work.rows {
			rewritten := links.RewriteBracketAt(newContent, r.position, r.targetTitle, newTitle, collSlug)
			if rewritten != newContent {
				mutated = true
				newContent = rewritten
			}
		}
		if !mutated {
			// No bracket matched the expected shape at the recorded
			// positions — possible if a previous content edit shifted
			// the offsets in a way replaceWikiLinks didn't catch, or
			// the bracket uses title-segment escape forms the rewriter
			// doesn't unescape (documented limitation, parallel to
			// RewriteWikiTitle's caveat). Re-parse on existing content
			// so the index converges; the row will flip to broken if
			// no clickable match remains, matching the renderer's
			// behavior on a body that would no longer render as a link.
			if err := s.replaceWikiLinks(tx, work.id, work.workspaceID, work.content); err != nil {
				return fmt.Errorf("cascade rename: reparse %s: %w", work.id, err)
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
		`), newContent, ts, ts, work.workspaceID, work.id); err != nil {
			return fmt.Errorf("cascade rename: update source %s: %w", work.id, err)
		}
		if err := s.replaceWikiLinks(tx, work.id, work.workspaceID, newContent); err != nil {
			return fmt.Errorf("cascade rename: reparse %s: %w", work.id, err)
		}
	}

	// (2) Flip newly-resolvable broken rows under the NEW title.
	return s.resolveBrokenTitleLinks(tx, renamedItemID, workspaceID, collSlug, newTitle)
}

// resolveBrokenTitleLinks flips item_wiki_links rows in the workspace
// that have target_kind='title' and a target_title matching the
// given (collSlug, title) — case-insensitive — to point at the
// supplied itemID. Called from two places:
//
//   - cascadeTitleRename after a title rename, to pick up any
//     pre-existing `[[newTitle]]` sources that were stored as broken
//     OR as qualified-fallback hits that should now resolve to a
//     literal-title match.
//   - tryCreateItem after a new item lands, so any pre-existing
//     `[[Title]]` sources resolve immediately rather than waiting
//     for a backfill run or a content rewrite.
//
// Three UPDATEs cover three distinct cases:
//
//	(1) Plain literal flip — target_title=newTitle, target_item_id IS NULL.
//	    Just resolves previously-broken plain references.
//	(2) Qualified literal flip — target_title=collSlug/newTitle,
//	    target_item_id IS NULL. Resolves previously-broken qualified
//	    references (no item with this title-in-collection existed).
//	(3) Literal-arrival retarget (only when newTitle contains `/`) —
//	    target_title=newTitle, target_item_id non-NULL and not us.
//	    Handles the arrival-order case Codex round 2 caught: if a
//	    row was previously resolved to a stage-2 qualified-fallback
//	    target and now a literal-title match exists, the renderer's
//	    stage-1 always wins, so the row flips to us.
//
// Why (3) is gated on title containing `/`: only `[[<slug>/Title]]`
// rows could have been resolved via stage-2 fallback (the qualified
// form REQUIRES a `/`). A row with target_title="Foo" (no slash) was
// resolved via stage 1 — if a second item titled "Foo" arrives, the
// renderer's Array.find() is order-dependent (we can't predict which
// the UI shows), so we leave the row pointing at the original
// resolution rather than churning. Codex round 3 P2 caught the
// previous broader UPDATE silently stealing such rows.
func (s *Store) resolveBrokenTitleLinks(tx *sql.Tx, itemID, workspaceID, collSlug, title string) error {
	plainTitleNorm := strings.ToLower(title)
	qualifiedTitleNorm := strings.ToLower(collSlug + "/" + title)

	// "Broken-in-practice" predicate: a row is eligible for flip
	// when target_item_id IS NULL (never resolved) OR its current
	// target points at a soft-deleted (or otherwise gone) item.
	// The renderer hides deleted-target links from clicks, so the
	// index must follow. Codex round 8 P2 caught the prior NULL-only
	// constraint missing the soft-delete-then-create-same-title
	// case. Subquery against items.deleted_at via NOT EXISTS so the
	// row qualifies when its target is missing entirely (defensive
	// against hard-delete; FK only cascades on source, not target).
	brokenPredicate := `(
		target_item_id IS NULL
		OR NOT EXISTS (
			SELECT 1 FROM items t
			WHERE t.id = item_wiki_links.target_item_id
			  AND t.deleted_at IS NULL
		)
	)`

	// (1) Plain literal flip — broken-in-practice rows.
	if _, err := tx.Exec(s.q(`
		UPDATE item_wiki_links
		SET target_item_id = ?
		WHERE target_kind = 'title'
		  AND target_workspace_id IS NULL
		  AND `+brokenPredicate+`
		  AND LOWER(target_title) = ?
		  AND source_item_id IN (
		      SELECT id FROM items WHERE workspace_id = ? AND deleted_at IS NULL
		  )
	`), itemID, plainTitleNorm, workspaceID); err != nil {
		return fmt.Errorf("resolve broken plain titles: %w", err)
	}

	// (2) Qualified literal flip — broken-in-practice rows.
	if _, err := tx.Exec(s.q(`
		UPDATE item_wiki_links
		SET target_item_id = ?
		WHERE target_kind = 'title'
		  AND target_workspace_id IS NULL
		  AND `+brokenPredicate+`
		  AND LOWER(target_title) = ?
		  AND source_item_id IN (
		      SELECT id FROM items WHERE workspace_id = ? AND deleted_at IS NULL
		  )
	`), itemID, qualifiedTitleNorm, workspaceID); err != nil {
		return fmt.Errorf("resolve broken qualified titles: %w", err)
	}

	// (3) Literal-arrival retarget. Only meaningful when our title
	// contains a `/` — then a row with this exact target_title might
	// have been resolved via stage-2 qualified fallback to a
	// different item, and our arrival makes the renderer prefer us
	// via stage 1.
	//
	// Codex round 5 finding 1: the broad UPDATE could steal rows
	// resolved via stage-1 literal match if a SECOND item with the
	// same slash-containing title arrives. Distinguish stage-1
	// (literal) from stage-2 (qualified-fallback) resolution by
	// checking the CURRENT target's title:
	//   - If target's title equals our literal title (case-
	//     insensitive), the row resolved via stage 1 to a "twin"
	//     item — don't steal it.
	//   - Otherwise the row resolved via stage 2 (target's title is
	//     just the trailing segment, not the whole `slug/title`) —
	//     stage 1 now wins, flip the row to us.
	if strings.Contains(title, "/") {
		if _, err := tx.Exec(s.q(`
			UPDATE item_wiki_links
			SET target_item_id = ?
			WHERE target_kind = 'title'
			  AND target_workspace_id IS NULL
			  AND target_item_id IS NOT NULL
			  AND target_item_id != ?
			  AND LOWER(target_title) = ?
			  AND source_item_id IN (
			      SELECT id FROM items WHERE workspace_id = ? AND deleted_at IS NULL
			  )
			  AND EXISTS (
			      SELECT 1 FROM items t
			      WHERE t.id = item_wiki_links.target_item_id
			        AND LOWER(t.title) != ?
			  )
		`), itemID, itemID, plainTitleNorm, workspaceID, plainTitleNorm); err != nil {
			return fmt.Errorf("retarget qualified-fallback to literal: %w", err)
		}
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

	// TargetParentID, when non-nil and non-empty, suppresses the
	// target's own parent from the backlinks result set. Set by the
	// handler from items.ParentID. Rationale: the parent already
	// appears in the "Parent: …" header on the target's page, so a
	// child's wiki-link mention of its parent only duplicates UI
	// already on screen. Symmetric with the always-on children-
	// suppression in GetBacklinks/CountBacklinks. TASK-1607 / IDEA-1601.
	TargetParentID *string
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
// Children-suppression is always-on for the same UI-duplication
// reason: a source item whose parent_id == targetItemID is a direct
// child, and children are already listed above the backlinks panel
// in the Child Items section. The child's body almost always
// references the parent (`[[Parent Title]]` somewhere in the
// narrative), which would otherwise dominate the backlinks list
// for any plan with many children. Parent-suppression is the
// symmetric case but opt-in via vis.TargetParentID: when the caller
// passes the target's parent_id, that one source row (the parent's
// mention of the target) is dropped because the parent appears in
// the target's "Parent: …" header. TASK-1607 / IDEA-1601.
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

	// Relational-suppression clause. Always-on children-suppression
	// (s.parent_id != targetItemID) mirrors the self-link filter
	// (s.id != targetItemID) two lines down: both hide rows that
	// already appear elsewhere in the target's page UI (Children
	// section above the backlinks panel) and so carry no novel
	// information. NULL-safe form because parent_id is nullable —
	// orphan items have parent_id IS NULL and would otherwise be
	// erroneously dropped by `s.parent_id != ?`. Parent-suppression
	// is opt-in (only when vis.TargetParentID is set) because callers
	// that don't have the target's parent_id handy can skip it
	// without changing existing semantics. TASK-1607 / IDEA-1601.
	relClause := " AND (s.parent_id IS NULL OR s.parent_id != ?)"
	args := []interface{}{targetItemID, workspaceID, targetItemID, targetItemID}
	if vis.TargetParentID != nil && *vis.TargetParentID != "" {
		relClause += " AND s.id != ?"
		args = append(args, *vis.TargetParentID)
	}

	// Build the visibility predicate. Unrestricted → omit. Otherwise
	// `collection_id IN (...)` OR `id IN (...)` — either branch alone
	// is acceptable so a granted-item-only access still resolves; an
	// empty IN-list is replaced with a sentinel "IN (NULL)" so the
	// predicate evaluates to FALSE for that branch without breaking
	// Postgres's empty-list rejection.
	visClause := ""
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
		  AND s.id != ?`+relClause+visClause+`
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

// CountBacklinks returns the total number of same-workspace
// backlinks the requester would see for a given target, applying
// the same visibility filter as GetBacklinks. Used by the backlinks
// handler to correctly compute pagination offsets across the
// same-ws / cross-ws union — without a count, paginating past the
// first page can't know where the cross-ws "tier" of results
// begins.
//
// Same predicate shape as GetBacklinks but with SELECT COUNT(*)
// instead of the row projection. PLAN-1593 / TASK-1597.
func (s *Store) CountBacklinks(targetItemID, workspaceID string, vis BacklinksVisibility) (int, error) {
	if !vis.Unrestricted && len(vis.FullCollectionIDs) == 0 && len(vis.GrantedItemIDs) == 0 {
		return 0, nil
	}
	// Relational-suppression clause — must mirror GetBacklinks
	// exactly. The handler's same-ws/cross-ws pagination math
	// depends on CountBacklinks returning the same number of rows
	// GetBacklinks would yield under identical vis; a drift here
	// would let suppressed rows consume LIMIT slots and shrink
	// pages silently. TASK-1607 / IDEA-1601.
	relClause := " AND (s.parent_id IS NULL OR s.parent_id != ?)"
	args := []interface{}{targetItemID, workspaceID, targetItemID, targetItemID}
	if vis.TargetParentID != nil && *vis.TargetParentID != "" {
		relClause += " AND s.id != ?"
		args = append(args, *vis.TargetParentID)
	}
	visClause := ""
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
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*)
		FROM item_wiki_links wl
		JOIN items s       ON s.id = wl.source_item_id
		JOIN collections c ON c.id = s.collection_id
		WHERE wl.target_item_id = ?
		  AND s.workspace_id = ?
		  AND s.deleted_at IS NULL
		  AND s.id != ?`+relClause+visClause+`
	`), args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count backlinks: %w", err)
	}
	return n, nil
}

// GetCrossWorkspaceBacklinks returns inbound `[[ws::REF]]` backlinks
// pointing at the queried target (`targetWorkspaceID`, `targetRef`)
// from EVERY OTHER workspace the requester has access to. Same-ws
// backlinks are GetBacklinks's responsibility — this method only
// surfaces the cross-workspace tier.
//
// PLAN-1593 / TASK-1597. The approach:
//
//  1. Enumerate workspaces the requester can see via
//     Store.GetUserWorkspaces (which already unions members +
//     grant-only guest workspaces — Codex round of the planning
//     phase identified this as broader than a membership query).
//  2. For each non-target workspace, compute per-workspace
//     visibility via Store.ResolveBacklinksVisibility (the request-
//     independent ACL helper — required because cross-ws traversal
//     can't read workspaceRole from a request scope).
//  3. Query that workspace's source rows with the per-ws visibility
//     predicate inline. Empty-visibility workspaces short-circuit.
//  4. Merge, sort by updated_at DESC, paginate.
//
// Pagination shape: sort in Go after collecting all matching rows.
// Cross-workspace backlinks are typically sparse (most items don't
// have cross-ws inbound links), so in-memory sort over a few
// hundred rows beats UNION ALL's verbosity. If profiling shows
// per-workspace queries dominating, the trivial optimization is a
// per-workspace LIMIT (offset+limit) as a safety cap — already
// applied below.
//
// `targetRef` matches case-insensitively (mirrors the renderer's
// resolver-route handling at markdown.ts:485 which forwards
// ref-shapes verbatim to the server-side resolver, which is itself
// case-insensitive).
//
// `allowedWorkspaceSlugs` is the OAuth/MCP token's workspace allow-
// list (TASK-952). A nil slice means "no token-level gate" (PAT or
// pre-consent token, allow all enumerated workspaces). A slice
// containing "*" is wildcard consent (allow all). Otherwise only
// workspaces whose slug appears in the list contribute source rows
// — preserves consent scope across the cross-ws boundary (Codex
// round 2 P1 caught the prior bypass: an MCP token consented for
// workspace A still leaked cross-ws backlinks from workspace B
// because the cross-ws path enumerated via the user's full
// workspace list).
//
// No parent↔child suppression here (the GetBacklinks pair adds
// that for same-ws): item.parent_id is workspace-scoped, so a
// cross-ws source row can never be the target's parent or child
// by construction. TASK-1607.
func (s *Store) GetCrossWorkspaceBacklinks(targetWorkspaceID, targetRef, requesterUserID string, allowedWorkspaceSlugs []string, limit, offset int) ([]models.Backlink, error) {
	if limit <= 0 || limit > 300 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}

	// Workspace enumeration depends on the requester's user role.
	// Admins have implicit access to every non-deleted workspace
	// (matches RequireWorkspaceAccess line 481), so for them we
	// list everything; non-admins use GetUserWorkspaces which
	// unions memberships + grant-only guest workspaces. Codex
	// round 1 P2 caught the prior membership-only path silently
	// hiding cross-ws backlinks from admin users.
	user, err := s.GetUser(requesterUserID)
	if err != nil {
		return nil, fmt.Errorf("lookup requester user: %w", err)
	}
	if user == nil {
		// Stale user ID — return empty rather than err so the
		// handler's UX surface stays usable.
		return nil, nil
	}
	var workspaces []models.Workspace
	if user.Role == "admin" {
		workspaces, err = s.ListWorkspaces()
		if err != nil {
			return nil, fmt.Errorf("admin enumerate workspaces: %w", err)
		}
	} else {
		workspaces, err = s.GetUserWorkspaces(requesterUserID)
		if err != nil {
			return nil, fmt.Errorf("enumerate accessible workspaces: %w", err)
		}
	}

	// Apply token allow-list filter. Pre-compute a small set for
	// constant-time slug membership tests. nil allowlist == no
	// gate (allow all); a list containing "*" is wildcard.
	allowAll := allowedWorkspaceSlugs == nil
	wildcard := false
	allowSet := make(map[string]bool, len(allowedWorkspaceSlugs))
	for _, slug := range allowedWorkspaceSlugs {
		if slug == "*" {
			wildcard = true
		}
		allowSet[slug] = true
	}

	// Per-workspace cap is offset+limit because, worst case, all
	// matching rows come from a single workspace and the global
	// paginate slice needs at least that many rows from that
	// workspace. Codex round 1 P2 caught the prior 1000-row
	// ceiling silently breaking pagination beyond offset>=1000;
	// the slice math is what bounds the result, not this cap.
	perWsCap := offset + limit

	var collected []models.Backlink
	for _, ws := range workspaces {
		if ws.ID == targetWorkspaceID {
			// Same-ws backlinks are GetBacklinks's responsibility.
			continue
		}
		// Token allow-list gate. PAT auth / no-token shapes pass
		// allowAll=true. Wildcard ("*") consent passes wildcard=true.
		// Otherwise the source workspace's slug must be explicitly
		// allowed.
		if !allowAll && !wildcard && !allowSet[ws.Slug] {
			continue
		}
		fullCollIDs, grantedItemIDs, err := s.ResolveBacklinksVisibility(requesterUserID, ws.ID, false)
		if err != nil {
			return nil, fmt.Errorf("resolve visibility for workspace %s: %w", ws.ID, err)
		}
		// Unrestricted iff both lists nil (admin user or full-access
		// member). When restricted with no resources, skip this
		// workspace entirely — no rows could be visible.
		unrestricted := fullCollIDs == nil && grantedItemIDs == nil
		if !unrestricted && len(fullCollIDs) == 0 && len(grantedItemIDs) == 0 {
			continue
		}
		rows, err := s.queryCrossWorkspaceBacklinksForWorkspace(
			targetWorkspaceID, targetRef, ws, unrestricted,
			fullCollIDs, grantedItemIDs, perWsCap,
		)
		if err != nil {
			return nil, err
		}
		collected = append(collected, rows...)
	}

	// Sort by updated_at descending; tiebreak by source ID for
	// deterministic ordering when timestamps collide (same-second
	// writes).
	sort.Slice(collected, func(i, j int) bool {
		if collected[i].UpdatedAt != collected[j].UpdatedAt {
			return collected[i].UpdatedAt > collected[j].UpdatedAt
		}
		return collected[i].SourceItemID < collected[j].SourceItemID
	})

	// Paginate.
	if offset >= len(collected) {
		return nil, nil
	}
	end := offset + limit
	if end > len(collected) {
		end = len(collected)
	}
	return collected[offset:end], nil
}

// queryCrossWorkspaceBacklinksForWorkspace fetches up to `cap` rows
// from a single source workspace whose item_wiki_links point at the
// target via target_workspace_id + target_ref. The visibility
// predicate is the same shape as GetBacklinks's — collection_id IN
// (...) OR id IN (...) — with the unrestricted variant skipping the
// predicate.
//
// Ref matching is dual: the exact `target_ref` AND a number-only
// LIKE fallback. The fallback mirrors resolveRefTx's cross-prefix-
// move logic — `[[other-ws::OLD-42]]` written before the target
// moved from OLD to NEW collection still resolves in the renderer
// via item_number, so the cross-ws index query must too. The LIKE
// pattern `%-<N>` matches any prefix-N ref because Pad prefixes are
// alphanumeric (no internal `-`), so trailing `-N` uniquely
// identifies the suffix. Codex round 1 P2.
//
// Each returned Backlink has SourceWorkspaceSlug populated so the
// caller can route the link to the correct workspace prefix.
func (s *Store) queryCrossWorkspaceBacklinksForWorkspace(
	targetWorkspaceID, targetRef string,
	sourceWs models.Workspace,
	unrestricted bool, fullCollIDs, grantedItemIDs []string,
	cap int,
) ([]models.Backlink, error) {
	// Derive the number-only LIKE pattern. If targetRef doesn't have
	// a `-N` suffix (defensive — shouldn't happen for canonical
	// PREFIX-N inputs), fall back to exact-only matching.
	likeFallback := ""
	if dash := strings.LastIndexByte(targetRef, '-'); dash > 0 && dash < len(targetRef)-1 {
		likeFallback = "%-" + targetRef[dash+1:]
	}

	refClause := "LOWER(wl.target_ref) = LOWER(?)"
	args := []interface{}{targetWorkspaceID}
	if likeFallback != "" {
		refClause = "(LOWER(wl.target_ref) = LOWER(?) OR LOWER(wl.target_ref) LIKE LOWER(?))"
		args = append(args, targetRef, likeFallback)
	} else {
		args = append(args, targetRef)
	}
	args = append(args, sourceWs.ID)

	// Build the visibility predicate same as GetBacklinks.
	visClause := ""
	if !unrestricted {
		collClause := "FALSE"
		if len(fullCollIDs) > 0 {
			placeholders := make([]string, len(fullCollIDs))
			for i, cid := range fullCollIDs {
				placeholders[i] = "?"
				args = append(args, cid)
			}
			collClause = "s.collection_id IN (" + strings.Join(placeholders, ",") + ")"
		}
		itemClause := "FALSE"
		if len(grantedItemIDs) > 0 {
			placeholders := make([]string, len(grantedItemIDs))
			for i, iid := range grantedItemIDs {
				placeholders[i] = "?"
				args = append(args, iid)
			}
			itemClause = "s.id IN (" + strings.Join(placeholders, ",") + ")"
		}
		visClause = " AND (" + collClause + " OR " + itemClause + ")"
	}
	args = append(args, cap)

	rows, err := s.db.Query(s.q(`
		SELECT s.id, c.prefix, s.item_number, s.title, c.slug, c.icon,
		       s.content, wl.position, wl.display_text, s.updated_at
		FROM item_wiki_links wl
		JOIN items s       ON s.id = wl.source_item_id
		JOIN collections c ON c.id = s.collection_id
		WHERE wl.target_kind = 'workspace_ref'
		  AND wl.target_workspace_id = ?
		  AND `+refClause+`
		  AND s.workspace_id = ?
		  AND s.deleted_at IS NULL`+visClause+`
		ORDER BY s.updated_at DESC, wl.position ASC
		LIMIT ?
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("query cross-ws backlinks (workspace %s): %w", sourceWs.ID, err)
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
			return nil, fmt.Errorf("scan cross-ws backlink row: %w", err)
		}
		bl := models.Backlink{
			SourceItemID:         sourceID,
			SourceRef:            formatRef(prefix, itemNumber),
			SourceTitle:          title,
			SourceCollectionSlug: collSlug,
			SourceCollectionIcon: collIcon,
			Snippet:              snippetAround(content, position),
			UpdatedAt:            updatedAt,
			SourceWorkspaceSlug:  sourceWs.Slug,
		}
		if displayText.Valid {
			ds := displayText.String
			bl.DisplayText = &ds
		}
		out = append(out, bl)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cross-ws backlinks: %w", err)
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
