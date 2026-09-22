package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// ErrWatermarkStampBadInput is returned for a stamp request the server could
// never honour (a cursor below 1, a hash that is not 64 hex characters).
var ErrWatermarkStampBadInput = errors.New("watermark stamp: bad input")

// StampContentWatermarkIfCaughtUp advances items.content_flushed_op_log_id to
// cursor WITHOUT writing content, when a browser tab proves that the stored body
// already is the live document (BUG-3124 unit B).
//
// Why it exists: a tab never re-flushes an unchanged document (the view dedupe,
// BUG-1899/1941), so op-log rows that arrived after the last flush, or that no
// flush ever covered, kept content_state "pending" forever, including after the
// tab that could clear it had opened and closed again. A PATCH is the wrong tool
// here: it writes a version row and bumps seq and updated_at on every idle view,
// which would make `pad item edit`'s expected_seq conflict with a tab that
// merely looked at the item (BUG-3035).
//
// THE PROOF is the TASK-1319 conditional flush's, applied to a flush whose
// content is identical to the row:
//   - cursor == MAX(op-log id): the tab's Y.Doc has applied every persisted row;
//   - sha256(items.content) == contentSHA256: the markdown that document
//     renders to IS the stored body.
//
// Together they say the row covers every op, which is exactly what the
// watermark records. Both are checked in ONE conditional UPDATE, against the
// content value read in the same transaction, so a concurrent write (a CLI
// PATCH, a real flush, a prune) that changes either makes this a no-op rather
// than a stamp over content that no longer matches.
//
// cursor == MAX also covers both of the collab-snapshot PATCH's staleness gates:
// it is >= MIN(op-log id), and >= any restore boundary, because a restore
// records its boundary at or below the MAX its own applier op creates. The
// watermark never regresses (the `<` guard).
//
// Returns whether the watermark moved. Nothing else on the row is touched: no
// content, no seq, no updated_at, no version.
func (s *Store) StampContentWatermarkIfCaughtUp(itemID string, cursor int64, contentSHA256 string) (bool, error) {
	if cursor < 1 {
		return false, fmt.Errorf("%w: op_log_cursor must be a positive op-log id", ErrWatermarkStampBadInput)
	}
	if b, err := hex.DecodeString(contentSHA256); err != nil || len(b) != sha256.Size {
		return false, fmt.Errorf("%w: content_sha256 must be 64 hex characters", ErrWatermarkStampBadInput)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var content string
	if err := tx.QueryRow(s.q(`SELECT content FROM items WHERE id = ? AND deleted_at IS NULL`), itemID).Scan(&content); err != nil {
		return false, fmt.Errorf("watermark stamp: read item: %w", err)
	}
	sum := sha256.Sum256([]byte(content))
	if hex.EncodeToString(sum[:]) != contentSHA256 {
		return false, nil
	}

	res, err := tx.Exec(s.q(`
		UPDATE items SET content_flushed_op_log_id = ?
		WHERE id = ?
		  AND content = ?
		  AND COALESCE(content_flushed_op_log_id, 0) < ?
		  AND ? = (SELECT COALESCE(MAX(id), 0) FROM item_yjs_updates WHERE item_id = ?)`),
		cursor, itemID, content, cursor, cursor, itemID)
	if err != nil {
		return false, fmt.Errorf("watermark stamp: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return n == 1, nil
}
