package store

import (
	"database/sql"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/diff"
)

// Bounds of RemapAttachmentReferencesInWorkspace (TASK-4278).
const (
	// remapStatementTimeout and remapLockTimeout are each chunk's
	// per-transaction bounds. The statement bound stays generous because one
	// item's chain can be large and a chunk never splits an item; the lock
	// bound is how long a chunk waits for the workspace seq lock.
	remapStatementTimeout = 30 * time.Minute
	remapLockTimeout      = 1 * time.Minute

	// remapChunkBudget is how long one chunk may hold the workspace seq lock,
	// measured from the moment it has the lock to just before it commits. The
	// target is at most about 1 s: this leaves 300 ms for the
	// item being written when the clock is read, the commit, and a slow box.
	// Measured: about 12 ms per rewritten item on the 4,079-item fixture.
	remapChunkBudget = 700 * time.Millisecond

	// remapChunkMaxItems is the hard cap on rewritten items per chunk, so a
	// clock that misbehaves cannot put the whole workspace in one chunk. At
	// the measured rate it is about 0.8 s of writes.
	remapChunkMaxItems = 64

	// remapScanPage is how many items one planning read takes. It is also
	// capped by the chunk's item cap so planning never overshoots it by much.
	remapScanPage = 32
)

// remapTuning is the TEST seam of RemapAttachmentReferencesInWorkspace
// (Store.remapTune, nil in production). Zero fields keep the production value.
type remapTuning struct {
	budget   time.Duration
	maxItems int
	// now is the clock the budget is measured on, so a test can drive the
	// chunk boundary deterministically instead of racing the wall clock.
	now func() time.Time
	// afterChunk runs after a chunk COMMITS, with the seq lock released, so it
	// may write to the workspace from the same goroutine. A non-nil return
	// stops the remap there, exactly as a failing chunk would. It is called for
	// the final chunk too (info.Final), where its error is ignored: everything
	// is already done.
	afterChunk func(info RemapChunkInfo) error
}

// RemapChunkInfo describes one committed chunk of a remap.
type RemapChunkInfo struct {
	Chunk    int // 1-based
	Items    int // whole items rewritten in this chunk
	Comments int
	Versions int
	// Hold is the clock time from having the workspace seq lock to just before
	// COMMIT: the figure the 1 s bound is about.
	Hold  time.Duration
	Final bool
}

// RemapPartialError is what RemapAttachmentReferencesInWorkspace returns when
// it fails or is stopped AFTER at least one chunk committed. The items of the
// committed chunks are fully rewritten; every other item is exactly as it was.
// Its text is plain on purpose: it ends up in the import's 500 body, and the
// cause (which can carry SQL) is only reachable through Unwrap, for the log.
type RemapPartialError struct {
	Chunks int // chunks committed before the stop
	Items  int // items rewritten in them
	Cause  error
}

func (e *RemapPartialError) Error() string {
	return fmt.Sprintf("stopped partway, after %d chunk(s) and %d item(s); the other items still point at the source workspace's files, so delete this partial workspace and import the bundle again",
		e.Chunks, e.Items)
}

func (e *RemapPartialError) Unwrap() error { return e.Cause }

func (s *Store) remapSettings() remapTuning {
	t := remapTuning{budget: remapChunkBudget, maxItems: remapChunkMaxItems, now: time.Now}
	if o := s.remapTune; o != nil {
		if o.budget > 0 {
			t.budget = o.budget
		}
		if o.maxItems > 0 {
			t.maxItems = o.maxItems
		}
		if o.now != nil {
			t.now = o.now
		}
		t.afterChunk = o.afterChunk
	}
	return t
}

// RemapAttachmentReferencesInWorkspace rewrites every
// "pad-attachment:OLD" reference in items.content + items.fields,
// in comment bodies (BUG-2615) AND in version history (BUG-3655) to
// "pad-attachment:NEW" for every (old, new) pair in the map. Run after a bundle import has
// rehydrated attachments so the imported content points at the new
// attachment ids instead of the source workspace's ids.
//
// CALLERS. Two, both in internal/server/handlers_import_bundle.go and both on
// a workspace that very import just created, which no other session can reach
// yet, and both inside the bundle-import request (so a user's request, though
// not one that touches a workspace anyone else uses):
//   - the deferred retry (~:701) that runs on every exit KEEPING a partial
//     workspace after some attachments were rehydrated (BUG-3656);
//   - the phase-3 remap closure (~:1120), the one behind the
//     importBundleRemapFault test seam.
//
// CALLER PRECONDITION — read this before adding a third caller.
// Every surface here is read-modify-write across a scan and a later
// UPDATE, with no row locks and no old-value predicate. Within a chunk the
// workspace seq lock closes the window against item writers, but comment
// writers do not take that lock, and the chunks are not one atomic change: a
// reader of a LIVE workspace would see it part-rewritten. Both are safe for the
// callers above for a reason that is about the callers, not this function: the
// workspace is brand new and unreachable, so there are no concurrent readers or
// writers.
//
// CHUNKS (TASK-4278). The work used to be one transaction under the workspace
// seq lock, which every item writer in the workspace takes: about 12 ms per
// rewritten item, so a large import held every writer off for seconds and a
// big enough one past lock_timeout. It is now a series of transactions, each
// holding the lock for about remapChunkBudget (700 ms) at most and releasing it
// before the next, so item writers interleave. A chunk is a run of WHOLE
// ITEMS in id order: an item's content, fields, comments, and version chain
// are planned together and written in the one transaction, never split. Items
// that need no rewrite cost only their scan.
//
//   - A part-done remap is consistent. Every item is either fully rewritten
//     (content, fields, comments and its whole version chain, in one commit) or
//     byte-for-byte as it was, and an unprocessed item's references dangle
//     exactly as they did before the remap began (they name the SOURCE
//     workspace's ids, which resolve to nothing here). The attachment
//     clones are not touched by a chunk that does not reference them.
//   - The stamp (stampAttachmentRefsTx) is PER CHUNK, inside the chunk's own
//     transaction and before its writes. A chunk therefore stamps exactly the
//     ids its own rewrites now point at, and the stamp, the text and the GC's
//     claim serialize in that one transaction as they did for the whole
//     remap. Stamping once up front would instead refresh clones nothing has
//     been rewritten to reference yet, hold their row locks across the whole
//     remap, and still not be tied to any chunk's commit. A clone whose only
//     references are in later chunks carries only its creation time until its
//     chunk runs; the orphan GC's grace is far longer than the remap.
//   - It is idempotent. A rewritten reference no longer contains the old id,
//     so rerunning over done items changes nothing and stamps nothing; a rerun
//     simply completes the items that were left.
//
// WHEN IT STOPS BETWEEN CHUNKS (a chunk fails, the process dies, or the
// afterChunk test seam says stop), the function returns *RemapPartialError
// (use errors.As). The bundle import handler then KEEPS the workspace, marks
// it partial (visible through the import status, `pad workspace list`), and
// answers the non-retryable 500 import_partially_remapped with that error's
// plain text (a failure in the FIRST chunk, which may have rewritten none or part
// of its items if the COMMIT outcome is unknown, is the same 500 under
// import_remap_failed). There is NO route
// that reruns only the remap: the remedy is to delete the partial workspace
// and import the bundle again. If the process dies between chunks the same is
// true, with the partial marker already recorded by the import's error arm or
// absent if the crash beat it; the workspace is never half of an item.
//
// Search needs no separate pass: on SQLite each UPDATE fires the
// items_fts_update / comments_fts_update triggers inside the
// transaction, and on Postgres search_vector is trigger-maintained. A
// post-commit rebuild that used to follow re-inserted every item and
// double-indexed it (BUG-2758).
func (s *Store) RemapAttachmentReferencesInWorkspace(workspaceID string, oldToNew map[string]string) error {
	if len(oldToNew) == 0 {
		return nil
	}
	tune := s.remapSettings()
	cursor := ""
	var chunks, items int
	var maxHold time.Duration
	started := tune.now()
	for {
		res, err := s.remapChunk(workspaceID, oldToNew, cursor, tune, chunks+1)
		if err != nil {
			if chunks > 0 {
				slog.Error("attachment remap stopped between chunks",
					"workspace_id", workspaceID, "chunks_committed", chunks, "items_rewritten", items, "error", err)
				return &RemapPartialError{Chunks: chunks, Items: items, Cause: err}
			}
			return err
		}
		chunks++
		items += res.info.Items
		if res.info.Hold > maxHold {
			maxHold = res.info.Hold
		}
		if tune.afterChunk != nil {
			if herr := tune.afterChunk(res.info); herr != nil && !res.info.Final {
				return &RemapPartialError{Chunks: chunks, Items: items, Cause: herr}
			}
		}
		if res.info.Final {
			break
		}
		cursor = res.cursor
	}
	slog.Info("attachment remap done", "workspace_id", workspaceID, "chunks", chunks,
		"items_rewritten", items, "max_lock_hold", maxHold, "duration", tune.now().Sub(started))
	return nil
}

// remapUnit is one item's whole share of a remap: its content and fields, its
// comments and its version chain. A unit is planned together and written in one
// transaction.
type remapUnit struct {
	itemID       string
	collectionID string
	itemChanged  bool
	content      string
	fields       string
	oldContent   string
	rechained    bool
	comments     []remapCommentUpdate
	versions     []versionRemap
}

type remapCommentUpdate struct{ id, body string }

type remapChunkResult struct {
	cursor string // the last item id this chunk is finished with
	info   RemapChunkInfo
}

// remapChunk is one transaction of the remap: it takes the workspace seq lock,
// plans whole items after `after` until its item cap or half its time budget,
// stamps what those rewrites point at, writes them until its time budget is
// spent (always at least one item, so a slow item cannot stall the remap), and
// commits.
func (s *Store) remapChunk(workspaceID string, oldToNew map[string]string, after string, tune remapTuning, chunkNo int) (remapChunkResult, error) {
	var res remapChunkResult
	tx, err := s.db.Begin()
	if err != nil {
		return res, fmt.Errorf("begin remap tx: %w", err)
	}
	defer tx.Rollback()

	// Long-write override (TASK-4278): raise the runtime pool's bounds for this
	// transaction only.
	if err := s.setTxTimeouts(tx, remapStatementTimeout, remapLockTimeout); err != nil {
		return res, err
	}

	// Serialise against concurrent item writes in this workspace
	// (PLAN-2857). This function SCANS items' fields and content, then writes
	// the snapshot back — so without the lock a concurrent update can commit
	// newer fields in between and this overwrites them. The lock is held from
	// here to the commit, per chunk.
	//
	// That lost update predates the relation index and is not caused by it;
	// what U5 adds is that the same stale snapshot is now also used to rebuild
	// the reverse index, so the staleness reaches a second place. One lock
	// closes both.
	//
	// It is the same advisory lock ITEM writers take, and taking it first here
	// preserves their order (workspace, then rows) so the two cannot invert.
	// Comment writers do not take it at all — which is safe, because a lock
	// nobody else in that path holds cannot participate in a cycle, and this
	// transaction takes no lock a comment writer could be waiting on.
	if err := s.acquireWorkspaceSeqLock(tx, workspaceID); err != nil {
		return res, err
	}
	start := tune.now()

	// ORDER BY id (BUG-2778 class sweep): this gathers rows and then UPDATEs
	// them one by one inside a transaction, so the scan's order IS the lock
	// order. Two concurrent remaps in one workspace would otherwise be free to
	// take the same row locks in different orders — Postgres does not promise
	// a stable seq-scan order. Items are walked by id, and each item's
	// comments and versions are written with it, so the order is by item.
	//
	// SCOPE OF THE CLAIM: this makes the row updates take their locks in one
	// order for every caller. It does not order the attachment rows
	// stampAttachmentRefsTx locks, so it is not "the whole deadlock class
	// removed" — it removes the cycle between two concurrent remaps' own row
	// updates, which is the one the sweep found.
	page := remapScanPage
	if tune.maxItems < page {
		page = tune.maxItems
	}
	var units []*remapUnit
	cursor := after
	exhausted := false
	for {
		n, last, err := s.planRemapPage(tx, workspaceID, oldToNew, cursor, page, &units)
		if err != nil {
			return res, err
		}
		if n > 0 {
			cursor = last
		}
		if n < page {
			exhausted = true
			break
		}
		if len(units) >= tune.maxItems || tune.now().Sub(start) >= tune.budget/2 {
			break
		}
	}
	trimmed := false
	if len(units) > tune.maxItems {
		units = units[:tune.maxItems]
		trimmed = true
	}

	// Both reads are done and nothing is written yet: the lost-update window
	// the workspace seq lock above closes (BUG-2797).
	if s.afterRemapScan != nil {
		s.afterRemapScan(workspaceID)
	}

	// Stamp what the rewrites now point AT, inside this same transaction.
	// The import stamped each comment body at insert time, but the body still
	// held the SOURCE ids then, so those stamps landed on nothing that ends up
	// referenced here. Without this the destination's clones are freshly
	// created, referenced only by text this transaction writes, and carrying
	// no stamp — the exact shape the orphan GC's never-attached claim
	// reclaims (BUG-2615).
	//
	// ORDERING: this runs BEFORE the content UPDATEs, per
	// stampAttachmentRefsTx's own contract and for the same two reasons every
	// other writer follows it. On Postgres the stamp row-locks the attachment
	// rows for the rest of the transaction, so a concurrent GC claim blocks
	// and re-evaluates against the fresh stamp; stamping last would let a
	// claim delete the target while the rewritten text sat uncommitted, and
	// the stamp would then match zero rows and commit a dangling reference.
	// It also keeps this path's lock order identical to the item and comment
	// writers (attachments first, then content rows) — the reverse order
	// deadlocks against them. All of the chunk's stamps therefore happen before
	// any of its writes, not unit by unit.
	//
	// The REWRITTEN TEXTS are passed rather than every id in oldToNew, so only
	// ids something actually references get stamped. A unit the time budget
	// cuts from this chunk was stamped here and is rewritten by the next:
	// stamping what is about to be referenced is harmless.
	var stampTexts []string
	for _, u := range units {
		if u.itemChanged {
			stampTexts = append(stampTexts, u.content, u.fields)
		}
		for _, c := range u.comments {
			stampTexts = append(stampTexts, c.body)
		}
	}
	if err := stampAttachmentRefsTx(tx, s, workspaceID, stampTexts...); err != nil {
		return res, fmt.Errorf("stamp remapped refs: %w", err)
	}

	written := 0
	for i, u := range units {
		if i > 0 && tune.now().Sub(start) >= tune.budget {
			break
		}
		if err := s.writeRemapUnit(tx, workspaceID, u); err != nil {
			return res, err
		}
		written++
		res.info.Comments += len(u.comments)
		res.info.Versions += len(u.versions)
	}
	// An item that needed only its comments or versions rewritten still counts
	// as an item of the chunk.
	res.info.Items = written

	complete := written == len(units) && !trimmed
	res.info.Final = complete && exhausted
	if complete {
		res.cursor = cursor
	} else {
		res.cursor = units[written-1].itemID
	}
	res.info.Chunk = chunkNo
	res.info.Hold = tune.now().Sub(start)

	// The UPDATEs above fired items_fts_update, which already reindexed the
	// rewritten content; there is no separate FTS pass (BUG-2758).
	if err := tx.Commit(); err != nil {
		return res, fmt.Errorf("commit remap chunk: %w", err)
	}
	return res, nil
}

// planRemapPage reads up to `limit` items after `after` (all of them, live or
// soft-deleted, because a soft-deleted item's comments are still rewritten)
// together with their comments and version chains, appends a unit for every
// item with something to rewrite, and returns how many items it read and the
// last id. A soft-deleted item's own body, fields and versions are left alone,
// as before.
func (s *Store) planRemapPage(tx *sql.Tx, workspaceID string, oldToNew map[string]string, after string, limit int, units *[]*remapUnit) (int, string, error) {
	rows, err := tx.Query(s.q(`SELECT id, collection_id, CASE WHEN deleted_at IS NULL THEN 1 ELSE 0 END, content, fields
		FROM items WHERE workspace_id = ? AND id > ? ORDER BY id LIMIT ?`), workspaceID, after, limit)
	if err != nil {
		return 0, "", fmt.Errorf("scan items for remap: %w", err)
	}
	type pageItem struct {
		id, collectionID, content, fields string
		live                              bool
	}
	var page []pageItem
	for rows.Next() {
		var it pageItem
		var live int
		if err := rows.Scan(&it.id, &it.collectionID, &live, &it.content, &it.fields); err != nil {
			rows.Close()
			return 0, "", fmt.Errorf("scan item: %w", err)
		}
		it.live = live == 1
		page = append(page, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, "", fmt.Errorf("iterate items for remap: %w", err)
	}
	rows.Close()
	if len(page) == 0 {
		return 0, "", nil
	}

	ids := make([]string, len(page))
	liveIDs := make([]string, 0, len(page))
	for i, it := range page {
		ids[i] = it.id
		if it.live {
			liveIDs = append(liveIDs, it.id)
		}
	}

	// Comment bodies carry `pad-attachment:` references exactly as item
	// content does, and a bundle import re-inserts them (export.go's comment
	// import), so walking items alone left every comment pointing at the
	// SOURCE workspace's ids — which resolve to nothing in the destination,
	// while the freshly-cloned rows they should have pointed at end up
	// referenced by nothing and eventually reclaimed (BUG-2615). Comments have
	// no deleted_at column, hence no live filter; they belong to their item's
	// unit whether or not it is live.
	commentsOf := map[string][]remapCommentUpdate{}
	crows, err := tx.Query(s.q(`SELECT item_id, id, body FROM comments WHERE workspace_id = ? AND item_id IN (`+remapPlaceholders(len(ids))+`) ORDER BY item_id, id`),
		append([]any{workspaceID}, anySlice(ids)...)...)
	if err != nil {
		return 0, "", fmt.Errorf("scan comments for remap: %w", err)
	}
	for crows.Next() {
		var itemID, id, body string
		if err := crows.Scan(&itemID, &id, &body); err != nil {
			crows.Close()
			return 0, "", fmt.Errorf("scan comment: %w", err)
		}
		if newBody := remapAttachmentRefs(body, oldToNew); newBody != body {
			commentsOf[itemID] = append(commentsOf[itemID], remapCommentUpdate{id: id, body: newBody})
		}
	}
	if err := crows.Err(); err != nil {
		crows.Close()
		return 0, "", fmt.Errorf("iterate comments for remap: %w", err)
	}
	crows.Close()

	// Version history carries references too (BUG-3655). The chain of every
	// live item on the page is read newest first, the order it resolves in
	// (ListItemVersionsPage), and planned against that item's body as it stands.
	chains := map[string][]versionRow{}
	if len(liveIDs) > 0 {
		vrows, err := tx.Query(s.q(`SELECT item_id, id, content, is_diff, COALESCE(body_sha256, '') FROM item_versions
			WHERE item_id IN (`+remapPlaceholders(len(liveIDs))+`) ORDER BY item_id, created_at DESC, version_seq DESC`), anySlice(liveIDs)...)
		if err != nil {
			return 0, "", fmt.Errorf("scan versions for remap: %w", err)
		}
		for vrows.Next() {
			var itemID string
			var r versionRow
			if err := vrows.Scan(&itemID, &r.id, &r.stored, &r.isDiff, &r.hash); err != nil {
				vrows.Close()
				return 0, "", fmt.Errorf("scan version: %w", err)
			}
			chains[itemID] = append(chains[itemID], r)
		}
		if err := vrows.Err(); err != nil {
			vrows.Close()
			return 0, "", fmt.Errorf("iterate versions: %w", err)
		}
		vrows.Close()
	}

	for _, it := range page {
		u := &remapUnit{itemID: it.id, collectionID: it.collectionID, comments: commentsOf[it.id]}
		if it.live {
			newContent := remapAttachmentRefs(it.content, oldToNew)
			newFields := remapAttachmentRefs(it.fields, oldToNew)
			if newContent != it.content || newFields != it.fields {
				u.itemChanged, u.content, u.fields, u.oldContent = true, newContent, newFields, it.content
			}
			u.versions, u.rechained = planVersionChainRemap(workspaceID, it.id, it.content, chains[it.id], oldToNew)
		}
		if u.itemChanged || len(u.comments) > 0 || len(u.versions) > 0 {
			*units = append(*units, u)
		}
	}
	return len(page), page[len(page)-1].id, nil
}

// writeRemapUnit writes one item's whole share in the chunk's transaction.
func (s *Store) writeRemapUnit(tx *sql.Tx, workspaceID string, u *remapUnit) error {
	if u.itemChanged {
		// BUMPS seq (BUG-3037). This write changes `content` and `fields` — the
		// state `expected_seq` exists to protect — and the scan is workspace-wide,
		// so it can reach a row a client is holding a token for. Today its only
		// callers run on a workspace the import just created, where nobody holds
		// a token; the bump is here anyway so that stays true of the CODE rather
		// than of the caller list, which is the kind of claim that goes stale
		// unread. The workspace seq lock this transaction already took is the one
		// the subquery needs, so this costs an indexed MAX(seq) per row and no new
		// locking.
		// NOT a decision enqueue door (TASK-3117 decision 3). This is a system
		// fan-out rewrite: one import remap touches every matching item, so an enqueue
		// per row would be an unbounded provider bill for one action. The rewrite
		// DOES change hashed decision state, so each touched item's stored
		// answers read current=false until its next direct write re-evaluates.
		if _, err := tx.Exec(s.q(`UPDATE items SET content = ?, fields = ?, seq = `+nextWorkspaceSeqSubquery+` WHERE id = ?`),
			u.content, u.fields, workspaceID, u.itemID); err != nil {
			return fmt.Errorf("update item %s: %w", u.itemID, err)
		}
		// The body changed with no version, so the newest version's reverse
		// patch is rebased onto it (BUG-3745). A chain rewritten whole already
		// encodes its newest version against the new body.
		if u.content != u.oldContent && !u.rechained {
			if err := s.rebaseNewestVersionPatch(tx, u.itemID, u.oldContent, u.content); err != nil {
				return fmt.Errorf("rebase newest version of %s: %w", u.itemID, err)
			}
		}
		// This rewrite maps `pad-attachment:` ids and cannot change a relation
		// VALUE — but it does write the blob, and the hook is a pure function
		// of (blob, schema), so calling it is a no-op while OMITTING it would
		// be a standing claim about what remapAttachmentRefs can touch. The
		// collection id rides along on the scan rather than costing a lookup per
		// item (PLAN-2857 U5).
		if _, err := s.replaceRelationLinks(tx, u.itemID, workspaceID, u.collectionID, u.fields); err != nil {
			return fmt.Errorf("index relation links for item %s: %w", u.itemID, err)
		}
	}
	for _, c := range u.comments {
		if _, err := tx.Exec(s.q(`UPDATE comments SET body = ? WHERE id = ?`), c.body, c.id); err != nil {
			return fmt.Errorf("update comment %s: %w", c.id, err)
		}
	}
	for _, v := range u.versions {
		if _, err := tx.Exec(s.q(`UPDATE item_versions SET content = ?, is_diff = ?, body_sha256 = ? WHERE id = ?`),
			v.content, s.dialect.BoolToInt(v.isDiff), nullIfEmpty(v.bodySHA256), v.id); err != nil {
			return fmt.Errorf("update version %s: %w", v.id, err)
		}
	}
	return nil
}

func remapPlaceholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anySlice(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// versionRemap is one item_versions row's rewritten storage.
type versionRemap struct {
	id      string
	content string
	isDiff  bool
	// bodySHA256 is the row's body_sha256 after the rewrite ("" = NULL):
	// the remapped body's hash when the old body was verified against one,
	// otherwise the row's hash as it was (BUG-3820).
	bodySHA256 string
}

// versionRow is one stored item_versions row as the chain walk reads it.
type versionRow struct {
	id, stored, body, hash string
	isDiff                 bool
}

// planVersionChainRemap plans the item_versions rewrite of ONE item for the
// remap (BUG-3655), from the item's body and its chain (newest first, as read).
// Versions are reverse patches resolved from the item's body backwards, so a
// reference in history cannot be rewritten in place: the chain is resolved
// against the body as it stands (the remap has not written yet), every
// resolved body is remapped, and each row is re-encoded against its remapped
// successor. A full row stays full; a patch row is re-diffed and kept only
// while smaller than the body, the rule every version writer uses. Only rows
// whose body or successor changed are rewritten. Line counts are left alone: a
// remap swaps one id for another and changes no line.
//
// It is per item so a chunk plans an item's chain in the same transaction that
// writes it: the chain is resolved against the body the SAME chunk rewrites.
//
// It returns the rewrites and whether the item's newest row was re-encoded
// against the remapped body, so the caller skips its newest-only rebase. A patch
// that fails to apply has no body; its text is remapped as written, the rows
// below it resolve past it as the reader's walk does, and the item is logged.
func planVersionChainRemap(workspaceID, itemID, itemContent string, chain []versionRow, oldToNew map[string]string) ([]versionRemap, bool) {
	if len(chain) == 0 {
		return nil, false
	}
	// The walk ListItemVersionsResolvedPage does, so every body here is
	// the one a reader is shown: a patch that fails to apply, or whose
	// result does not match the row's body_sha256 (BUG-3820), is skipped,
	// and the row below it resolves against the same successor.
	known := make([]bool, len(chain))
	successor := itemContent
	for i := range chain {
		if !chain[i].isDiff {
			chain[i].body, known[i] = chain[i].stored, true
		} else if body, err := diff.ApplyPatch(successor, chain[i].stored); err == nil {
			chain[i].body, known[i] = body, true
		}
		if known[i] && chain[i].hash != "" && versionBodySHA256(chain[i].body) != chain[i].hash {
			chain[i].body, known[i] = "", false
		}
		if known[i] {
			successor = chain[i].body
		}
	}
	rechained := known[0]

	var out []versionRemap
	oldSucc, newSucc := itemContent, remapAttachmentRefs(itemContent, oldToNew)
	broken := false
	for i, r := range chain {
		if !known[i] {
			// No body to re-diff. Remap the patch text itself: an id it
			// carries whole is replaced by one of the same length, so the
			// hunk counts still hold. It resolved no better at the source.
			// The successors stay as they are, as in the walk above.
			broken = true
			if t := remapAttachmentRefs(r.stored, oldToNew); t != r.stored {
				out = append(out, versionRemap{id: r.id, content: t, isDiff: r.isDiff, bodySHA256: r.hash})
			}
			continue
		}
		newBody := remapAttachmentRefs(r.body, oldToNew)
		if newBody != r.body || (r.isDiff && newSucc != oldSucc) {
			stored, isDiff := newBody, false
			if r.isDiff {
				if patch, ok := diff.ReversePatch(newBody, newSucc); ok && diff.IsDiffSmaller(patch, newBody) {
					stored, isDiff = patch, true
				}
			}
			// A hash is re-minted only for a body that was verified
			// against one: an unhashed row's body was reconstructed, not
			// verified, and stays unhashed (BUG-3820).
			hash := r.hash
			if hash != "" {
				hash = versionBodySHA256(newBody)
			}
			if stored != r.stored || isDiff != r.isDiff || hash != r.hash {
				out = append(out, versionRemap{id: r.id, content: stored, isDiff: isDiff, bodySHA256: hash})
			}
		}
		oldSucc, newSucc = r.body, newBody
	}
	if broken {
		slog.Warn("attachment remap: part of an item's version history does not resolve; its patch text was remapped as written",
			"workspace_id", workspaceID, "item_id", itemID)
	}
	return out, rechained
}
