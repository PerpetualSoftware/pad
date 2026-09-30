package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Op-log recovery (TASK-2198 U4): the store half.
//
// When a tab dies uncleanly its last typing exists only in item_yjs_updates:
// content-bearing rows above items.content_flushed_op_log_id, which
// content_state reports as applied_pending_flush. The server materializes
// those rows into markdown in a worker process (internal/materialize) and
// writes the result here. This file holds the reads that build the job and the
// write that lands it; internal/server/materialize_recovery.go holds the
// triggers and the worker, and is the only caller.
//
// THE WRITE IS A FLUSH, NOT AN EDIT. It carries the same proof a tab's
// collab-snapshot flush carries — the markdown was rebuilt from every op-log
// row up to a cursor, and the cursor is still MAX(op-log id) — so it advances
// the watermark to that cursor and to nothing beyond it. It never prunes the
// op-log: the next tab to open the item replays the op-log, which already holds
// everything this write stored.

// MaterializeInput is what a recovery job is built from, read in ONE statement
// so the rows and the cursor describe the same op-log.
type MaterializeInput struct {
	ItemID      string
	WorkspaceID string
	// Rows are the item's CONTENT-BEARING op-log rows, oldest first, over the
	// WHOLE op-log, not only those above the watermark: replay rebuilds the
	// document from nothing, as a tab's replay (since=0) does. Rows marked
	// non-content-bearing provably cannot change the document (BUG-3124), so
	// leaving them out gives the same document (U2's parity set).
	Rows [][]byte
	// Cursor is MAX(id) over ALL of the item's op-log rows, content-bearing or
	// not, from the same statement as Rows.
	Cursor int64
	// SchemaVersions are the distinct schema_version stamps over all rows.
	SchemaVersions []string
	// Pending is how many content-bearing rows sit above the watermark.
	Pending int
	// SetAside is how many rows a schema rebuild set aside (BUG-3244).
	SetAside int
}

// LoadMaterializeInput reads the item's op-log for a recovery job. It returns
// nil (no error) when the item is missing, soft-deleted, or has no op-log.
func (s *Store) LoadMaterializeInput(itemID string) (*MaterializeInput, error) {
	if itemID == "" {
		return nil, errors.New("LoadMaterializeInput: itemID is required")
	}
	var workspaceID string
	if err := s.db.QueryRow(s.q(`SELECT workspace_id FROM items WHERE id = ? AND deleted_at IS NULL`), itemID).Scan(&workspaceID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("materialize input (item): %w", err)
	}

	// ONE statement: the rows, their stamps and the watermark come from the
	// same snapshot on both dialects, so Cursor can never name a row whose
	// bytes are not in Rows. Non-content-bearing rows contribute their id and
	// stamp but not their bytes.
	rows, err := s.db.Query(s.q(`
		SELECT u.id, u.content_bearing, u.schema_version,
		       CASE WHEN u.content_bearing = TRUE THEN u.update_data END,
		       COALESCE(i.content_flushed_op_log_id, 0)
		FROM item_yjs_updates u
		JOIN items i ON i.id = u.item_id
		WHERE u.item_id = ?
		ORDER BY u.id ASC`), itemID)
	if err != nil {
		return nil, fmt.Errorf("materialize input (op-log): %w", err)
	}
	defer rows.Close()

	in := &MaterializeInput{ItemID: itemID, WorkspaceID: workspaceID}
	seen := map[string]bool{}
	for rows.Next() {
		var (
			id        int64
			bearing   bool
			version   string
			data      []byte
			watermark int64
		)
		if err := rows.Scan(&id, &bearing, &version, &data, &watermark); err != nil {
			return nil, fmt.Errorf("materialize input (scan): %w", err)
		}
		in.Cursor = id
		if !seen[version] {
			seen[version] = true
			in.SchemaVersions = append(in.SchemaVersions, version)
		}
		if bearing {
			in.Rows = append(in.Rows, data)
			if id > watermark {
				in.Pending++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("materialize input (rows): %w", err)
	}
	if in.Cursor == 0 {
		return nil, nil
	}
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM item_yjs_updates_set_aside WHERE item_id = ?`), itemID).Scan(&in.SetAside); err != nil {
		return nil, fmt.Errorf("materialize input (set-aside): %w", err)
	}
	return in, nil
}

// ItemHasPendingContent reports whether the item's op-log holds content-bearing
// rows above its flush watermark: content_state's applied_pending_flush
// predicate, for a live item.
func (s *Store) ItemHasPendingContent(itemID string) (bool, error) {
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM items i
		WHERE i.id = ? AND i.deleted_at IS NULL AND `+pendingFlushExistsSQLFor("i")), itemID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("item pending content: %w", err)
	}
	return n > 0, nil
}

// MaterializeCandidate is one row of the recovery sweep's backstop set, with
// the keyset position (LastAt, ItemID) it sorts at.
type MaterializeCandidate struct {
	ItemID string
	// LastAt is the item's newest op-log created_at, exactly as stored.
	LastAt string
}

// MaterializeCursor is a keyset position in the sweep's order. The zero value
// is the start.
type MaterializeCursor struct {
	LastAt string
	ItemID string
}

// ListMaterializeCandidates returns up to limit live items whose op-log holds
// content-bearing rows above the flush watermark, whose NEWEST op-log row is
// older than before, and which hold no set-aside rows: the recovery sweep's
// backstop set (the shape of ListDormantOpLogItemsBefore, with the flush
// predicate inverted). The caller still checks for an open room.
//
// KEYSET-PAGED, strictly after `after`, in (newest op-log created_at, item id)
// order. The caller filters what comes back (open rooms, and the worker's
// per-item failure budget), so a page read from the start every time would
// hand back the same items forever once `limit` of them sit filtered, and
// every candidate behind them would starve. Walking the set page by page and
// wrapping at the end reaches every candidate however many ineligible ones
// sort ahead of it.
func (s *Store) ListMaterializeCandidates(before time.Time, after MaterializeCursor, limit int) ([]MaterializeCandidate, error) {
	if limit <= 0 {
		limit = 100
	}
	cutoff := before.UTC().Format(time.RFC3339)
	rows, err := s.db.Query(s.q(`
		SELECT u.item_id, MAX(u.created_at)
		FROM item_yjs_updates u
		JOIN items i ON i.id = u.item_id
		WHERE i.deleted_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM item_yjs_updates_set_aside sa WHERE sa.item_id = u.item_id)
		GROUP BY u.item_id, i.content_flushed_op_log_id
		HAVING MAX(u.created_at) < ?
		   AND SUM(CASE WHEN u.content_bearing = TRUE AND u.id > COALESCE(i.content_flushed_op_log_id, 0) THEN 1 ELSE 0 END) > 0
		   AND (MAX(u.created_at) > ? OR (MAX(u.created_at) = ? AND u.item_id > ?))
		ORDER BY MAX(u.created_at) ASC, u.item_id ASC
		LIMIT ?`), cutoff, after.LastAt, after.LastAt, after.ItemID, limit)
	if err != nil {
		return nil, fmt.Errorf("list materialize candidates: %w", err)
	}
	defer rows.Close()
	var out []MaterializeCandidate
	for rows.Next() {
		var c MaterializeCandidate
		if err := rows.Scan(&c.ItemID, &c.LastAt); err != nil {
			return nil, fmt.Errorf("scan materialize candidate: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// MaterializeOutcome is what MaterializeFlush did.
type MaterializeOutcome string

const (
	// MaterializeApplied: items.content now holds the markdown; seq and
	// updated_at were bumped, one version row was written (when the body
	// changed) and the watermark is the cursor.
	MaterializeApplied MaterializeOutcome = "applied"
	// MaterializeStamped: the body already was the markdown, so only the
	// watermark moved (StampContentWatermarkIfCaughtUp): no seq, no version.
	MaterializeStamped MaterializeOutcome = "stamped"
	// MaterializeCursorMoved: the op-log changed after the job was built
	// (MAX(id) is not the cursor). Nothing was written.
	MaterializeCursorMoved MaterializeOutcome = "cursor_moved"
	// MaterializeNothingPending: no content-bearing row is above the watermark
	// any more (a tab flushed, or a direct write replaced the op-log).
	MaterializeNothingPending MaterializeOutcome = "nothing_pending"
	// MaterializeSetAside: the item holds set-aside rows (BUG-3244). They are
	// left untouched and the item is not written.
	MaterializeSetAside MaterializeOutcome = "set_aside"
	// MaterializeGone: the item is missing or soft-deleted.
	MaterializeGone MaterializeOutcome = "gone"
	// MaterializeEmptyRefused: the recovered body is blank and the stored one
	// is not, so nothing was written (BUG-3316). A tab never does this: its
	// lazy seed (TASK-1261) sees an empty document and seeds FROM
	// items.content. Recovery cannot see whether the document is empty or
	// merely renders blank, so it keeps the stored body in both cases, which
	// is the conservative side of the tab's rule. Decided under the write
	// lock, against the row being replaced.
	MaterializeEmptyRefused MaterializeOutcome = "empty_document"
)

type materializeAbort struct{ outcome MaterializeOutcome }

func (e *materializeAbort) Error() string { return "materialize flush: " + string(e.outcome) }

// materializeFlushPrecheckHook, when set by a test, runs inside the write's
// transaction immediately before the checks; materializeFlushAfterCheckHook
// runs after they have passed, before the UPDATE: the window a row committed
// by another connection lands in on Postgres (READ COMMITTED).
var (
	materializeFlushPrecheckHook   func(itemID string)
	materializeFlushAfterCheckHook func(itemID string)
)

// MaterializeFlush writes a recovered body to items.content (TASK-2198 U4).
//
// CALLER CONTRACT: hold the collab per-item setup lock with no room open for
// the item (collab.RoomManager.UnderItemLockIfNoRoom). Every op-log append
// goes through a room, and a room can only be created under that lock, so
// nothing can append while this runs. The checks below do not depend on that:
// they run inside the write's own transaction, under UpdateItem's locks, and
// the watermark moves through the cursor-gated SQL a collab-snapshot flush
// uses, so a row committing after the check is left above the watermark.
//
// It proceeds only when, in the write's transaction: MAX(op-log id) equals
// cursor, at least one content-bearing row is above the watermark, and the
// item holds no set-aside rows. Otherwise nothing is written and the outcome
// says which check failed; that is not an error. The version row is
// attributed to the system (models.ItemUpdate.Recovered). The op-log is never
// pruned.
func (s *Store) MaterializeFlush(itemID string, cursor int64, markdown string) (MaterializeOutcome, *models.Item, error) {
	if itemID == "" || cursor < 1 {
		return "", nil, errors.New("MaterializeFlush: itemID and a positive cursor are required")
	}

	var current string
	if err := s.db.QueryRow(s.q(`SELECT content FROM items WHERE id = ? AND deleted_at IS NULL`), itemID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MaterializeGone, nil, nil
		}
		return "", nil, fmt.Errorf("materialize flush (read): %w", err)
	}

	check := func(q Queryer, tx *sql.Tx) (MaterializeOutcome, error) {
		if materializeFlushPrecheckHook != nil {
			materializeFlushPrecheckHook(itemID)
		}
		var maxID int64
		if err := q.QueryRow(s.q(`SELECT COALESCE(MAX(id), 0) FROM item_yjs_updates WHERE item_id = ?`), itemID).Scan(&maxID); err != nil {
			return "", fmt.Errorf("materialize flush (max): %w", err)
		}
		if maxID != cursor {
			return MaterializeCursorMoved, nil
		}
		var setAside int
		if err := q.QueryRow(s.q(`SELECT COUNT(*) FROM item_yjs_updates_set_aside WHERE item_id = ?`), itemID).Scan(&setAside); err != nil {
			return "", fmt.Errorf("materialize flush (set-aside): %w", err)
		}
		if setAside > 0 {
			return MaterializeSetAside, nil
		}
		var pending int
		if tx != nil {
			n, err := s.CountPendingContentRowsTx(tx, itemID)
			if err != nil {
				return "", err
			}
			pending = n
		} else if err := q.QueryRow(s.q(`
			SELECT COUNT(*) FROM item_yjs_updates u JOIN items i ON i.id = u.item_id
			WHERE u.item_id = ? AND u.id > COALESCE(i.content_flushed_op_log_id, 0) AND u.content_bearing = TRUE`), itemID).Scan(&pending); err != nil {
			return "", fmt.Errorf("materialize flush (pending): %w", err)
		}
		if pending == 0 {
			return MaterializeNothingPending, nil
		}
		return "", nil
	}

	// The body already is the document: move the watermark only, as a tab's
	// watermark stamp would (BUG-3124 unit B). A version row and a seq bump
	// for a write that changes nothing would conflict every token-holder for
	// no reason.
	if current == markdown {
		if outcome, err := check(s.db, nil); err != nil || outcome != "" {
			return outcome, nil, err
		}
		sum := sha256.Sum256([]byte(markdown))
		moved, err := s.StampContentWatermarkIfCaughtUp(itemID, cursor, hex.EncodeToString(sum[:]))
		if err != nil {
			return "", nil, err
		}
		if !moved {
			return MaterializeCursorMoved, nil, nil
		}
		return MaterializeStamped, nil, nil
	}

	c := cursor
	input := models.ItemUpdate{
		Content:       &markdown,
		ChangeSummary: models.RecoveryChangeSummary,
		ForceVersion:  true,
		Recovered:     true,
		OpLogCursor:   &c,
	}
	updated, err := s.UpdateItemWithPreCheck(itemID, input, func(tx *sql.Tx, existing *models.Item) error {
		if strings.TrimSpace(markdown) == "" && existing != nil && strings.TrimSpace(existing.Content) != "" {
			return &materializeAbort{outcome: MaterializeEmptyRefused}
		}
		outcome, err := check(tx, tx)
		if err != nil {
			return err
		}
		if outcome != "" {
			return &materializeAbort{outcome: outcome}
		}
		if materializeFlushAfterCheckHook != nil {
			materializeFlushAfterCheckHook(itemID)
		}
		return nil
	})
	if err != nil {
		var abort *materializeAbort
		if errors.As(err, &abort) {
			return abort.outcome, nil, nil
		}
		return "", nil, err
	}
	if updated == nil {
		return MaterializeGone, nil, nil
	}
	return MaterializeApplied, updated, nil
}
