package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TASK-3531: dormancy compaction. Instead of deleting a dormant item's whole
// op-log, the sweep can replace it with ONE frame, the Yjs state of the
// replayed document (materialize.Snapshot). Yjs identity (clientID, clock)
// survives that, so a tab that slept past the sweep can still merge its
// unsent edits; a deleted log forces it to refresh and hands its text back.
//
// items.yjs_compacted_through is the highest op-log id the snapshot covers,
// and items.yjs_snapshot_op_id is the snapshot row. Join admits a resume
// cursor <= yjs_compacted_through only while the snapshot row still exists
// (CompactedResumeCovers), so a later path that deletes the op-log cannot
// leave the columns admitting a tab onto a rebuilt document. The paths that
// delete the whole log also clear them (clearOpLogCompactionQ), as hygiene.

// ErrCompactionRefused reports that CompactItemOpLog changed nothing because
// the item no longer met the conditions the snapshot was built under.
var ErrCompactionRefused = errors.New("op-log compaction refused: the op-log changed or is no longer dormant and flushed")

// CompactItemOpLog replaces itemID's op-log rows up to expectMaxID with the
// single snapshot frame, in one transaction, iff the item is still exactly
// what the snapshot was built from:
//   - MAX(id) of its op-log is expectMaxID (no row arrived since the rows
//     were read);
//   - no row is newer than cutoff (still dormant: the sweep's own test);
//   - items.content_flushed_op_log_id >= expectMaxID (items.content holds
//     every row, which is what makes the body equal to the snapshot's text).
//
// Otherwise it returns ErrCompactionRefused and changes nothing. On success the
// snapshot is one content-bearing row, the flush watermark moves to it (the
// body already equals its text), and the two compaction columns are set. It
// returns the snapshot row's id.
//
// The caller holds the collab per-item lock with no room open, as for
// PruneItemOpLogIfDormantBefore; the checks here are the atomic re-check.
func (s *Store) CompactItemOpLog(itemID string, cutoff time.Time, expectMaxID int64, frame []byte, schemaVersion string) (int64, error) {
	if itemID == "" || expectMaxID <= 0 || len(frame) == 0 || schemaVersion == "" {
		return 0, errors.New("CompactItemOpLog: itemID, expectMaxID, frame and schemaVersion are required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("compact op-log (begin): %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Postgres serializes with other writers of the item row; SQLite's
	// _txlock=immediate already made this transaction the only writer.
	lock := ""
	if s.dialect.Driver() == DriverPostgres {
		lock = " FOR UPDATE"
	}
	var watermark sql.NullInt64
	if err := tx.QueryRow(s.dialect.Rebind(`SELECT content_flushed_op_log_id FROM items WHERE id = ?`+lock), itemID).Scan(&watermark); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrCompactionRefused
		}
		return 0, fmt.Errorf("compact op-log (read watermark): %w", err)
	}
	var maxID sql.NullInt64
	var newer bool
	if err := tx.QueryRow(s.dialect.Rebind(`
		SELECT MAX(id),
		       EXISTS (SELECT 1 FROM item_yjs_updates WHERE item_id = ? AND created_at >= ?)
		FROM item_yjs_updates WHERE item_id = ?`), itemID, cutoff.UTC().Format(time.RFC3339), itemID).Scan(&maxID, &newer); err != nil {
		return 0, fmt.Errorf("compact op-log (read op-log): %w", err)
	}
	if !maxID.Valid || maxID.Int64 != expectMaxID || newer || !watermark.Valid || watermark.Int64 < expectMaxID {
		return 0, ErrCompactionRefused
	}

	if _, err := tx.Exec(s.dialect.Rebind(`DELETE FROM item_yjs_updates WHERE item_id = ? AND id <= ?`), itemID, expectMaxID); err != nil {
		return 0, fmt.Errorf("compact op-log (delete): %w", err)
	}
	snapID, err := s.insertYjsFrameQ(tx, itemID, frame, schemaVersion, time.Now().UTC().Format(time.RFC3339), yjsFrameHash(frame), true)
	if err != nil {
		return 0, fmt.Errorf("compact op-log (insert snapshot): %w", err)
	}
	if _, err := tx.Exec(s.dialect.Rebind(`
		UPDATE items
		SET content_flushed_op_log_id = ?, yjs_compacted_through = ?, yjs_snapshot_op_id = ?
		WHERE id = ?`), snapID, expectMaxID, snapID, itemID); err != nil {
		return 0, fmt.Errorf("compact op-log (stamp item): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("compact op-log (commit): %w", err)
	}
	return snapID, nil
}

// CompactedResumeCovers reports whether a resume cursor `since` is covered by
// itemID's compaction snapshot: the item was compacted through an id >= since,
// AND its snapshot row still exists. The second condition is what makes a
// stale yjs_compacted_through harmless: every path that deletes the op-log
// deletes the snapshot row with it.
func (s *Store) CompactedResumeCovers(itemID string, since int64) (bool, error) {
	if since <= 0 {
		return false, nil
	}
	var covers bool
	err := s.db.QueryRow(s.dialect.Rebind(`
		SELECT EXISTS (
			SELECT 1 FROM items i
			JOIN item_yjs_updates u ON u.id = i.yjs_snapshot_op_id AND u.item_id = i.id
			WHERE i.id = ? AND i.yjs_compacted_through IS NOT NULL AND i.yjs_compacted_through >= ?
		)`), itemID, since).Scan(&covers)
	if err != nil {
		return false, fmt.Errorf("compacted resume check: %w", err)
	}
	return covers, nil
}

// IsCompactedLog reports whether itemID's op-log is exactly its compaction
// snapshot (one row, the snapshot row), so the sweep does not compact it again.
func (s *Store) IsCompactedLog(itemID string) (bool, error) {
	var only bool
	err := s.db.QueryRow(s.dialect.Rebind(`
		SELECT EXISTS (
			SELECT 1 FROM items i
			WHERE i.id = ? AND i.yjs_snapshot_op_id IS NOT NULL
			  AND (SELECT COUNT(*) FROM item_yjs_updates u WHERE u.item_id = i.id) = 1
			  AND (SELECT MAX(u.id) FROM item_yjs_updates u WHERE u.item_id = i.id) = i.yjs_snapshot_op_id
		)`), itemID).Scan(&only)
	if err != nil {
		return false, fmt.Errorf("compacted log check: %w", err)
	}
	return only, nil
}

// clearOpLogCompactionQ forgets an item's compaction when its op-log is
// deleted. Hygiene, not safety: CompactedResumeCovers already requires the
// snapshot row to exist.
func (s *Store) clearOpLogCompactionQ(q interface {
	Exec(query string, args ...any) (sql.Result, error)
}, itemID string) error {
	if _, err := q.Exec(s.dialect.Rebind(`UPDATE items SET yjs_compacted_through = NULL, yjs_snapshot_op_id = NULL WHERE id = ? AND yjs_snapshot_op_id IS NOT NULL`), itemID); err != nil {
		return fmt.Errorf("clear op-log compaction: %w", err)
	}
	return nil
}
