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
//     renders to IS the stored body, byte for byte, or (since BUG-3197) is the
//     editor's own serialization of it. The tab stamps in the second case when
//     its document serializes exactly as the stored body does once parsed, so
//     a document re-seeded from the stored body serializes the same way. A real
//     flush gives the same equivalence and no more (it stores the
//     serialization, and the next seed parses it), so the stamp claims nothing
//     a flush would not. In BOTH cases the proof is about markdown: document
//     state the serializer does not write is not in items.content, and
//     neither a flush nor this stamp could put it there.
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
	res, err := s.StampContentWatermarkIfCaughtUpCounted(itemID, cursor, contentSHA256)
	return res.Advanced, err
}

// WatermarkStampResult is what StampContentWatermarkIfCaughtUpCounted reports.
type WatermarkStampResult struct {
	// Advanced is whether the watermark moved.
	Advanced bool
	// PrevWatermark is the watermark the stamp moved FROM (0 for none). Set
	// only when Advanced.
	PrevWatermark int64
	// CoveredContentRows counts the CONTENT-BEARING op-log rows the advance
	// newly covered: id in (PrevWatermark, cursor]. Set only when Advanced.
	//
	// TASK-3541 step 0: a stamp covering such rows is the one case where the
	// server takes a tab's word that rows it cannot read are already in
	// items.content. An honest tab gets there only when its document holds
	// content-bearing rows yet renders to the stored body (typed then undone,
	// editor normalisation ops). This count measures how often that happens
	// before any server-side check is built on it. Zero is the view-only stamp
	// (BUG-3124's SyncStep and resend rows), which claims nothing about content.
	CoveredContentRows int64
}

// StampContentWatermarkIfCaughtUpCounted is StampContentWatermarkIfCaughtUp
// that also reports what the advance covered (TASK-3541 step 0). The count is
// read in the same transaction as the conditional UPDATE. It is exact when the
// caller holds the collab item lock, as the HTTP door does, because every other
// watermark writer (the snapshot flush, recovery) holds it too. Without the
// lock, a flush landing between the read and the UPDATE could make the count
// include rows that flush covered: an over-count, which is the safe direction
// for a measurement. Behaviour is otherwise identical.
func (s *Store) StampContentWatermarkIfCaughtUpCounted(itemID string, cursor int64, contentSHA256 string) (WatermarkStampResult, error) {
	var none WatermarkStampResult
	if cursor < 1 {
		return none, fmt.Errorf("%w: op_log_cursor must be a positive op-log id", ErrWatermarkStampBadInput)
	}
	if b, err := hex.DecodeString(contentSHA256); err != nil || len(b) != sha256.Size {
		return none, fmt.Errorf("%w: content_sha256 must be 64 hex characters", ErrWatermarkStampBadInput)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return none, err
	}
	defer func() { _ = tx.Rollback() }()

	var content string
	var prev int64
	if err := tx.QueryRow(s.q(`SELECT content, COALESCE(content_flushed_op_log_id, 0) FROM items WHERE id = ? AND deleted_at IS NULL`), itemID).Scan(&content, &prev); err != nil {
		return none, fmt.Errorf("watermark stamp: read item: %w", err)
	}
	sum := sha256.Sum256([]byte(content))
	if hex.EncodeToString(sum[:]) != contentSHA256 {
		return none, nil
	}

	res, err := tx.Exec(s.q(`
		UPDATE items SET content_flushed_op_log_id = ?
		WHERE id = ?
		  AND content = ?
		  AND COALESCE(content_flushed_op_log_id, 0) < ?
		  AND ? = (SELECT COALESCE(MAX(id), 0) FROM item_yjs_updates WHERE item_id = ?)`),
		cursor, itemID, content, cursor, cursor, itemID)
	if err != nil {
		return none, fmt.Errorf("watermark stamp: update: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return none, err
	}
	if n != 1 {
		return none, tx.Commit()
	}
	// The UPDATE matched, so the watermark is now cursor; it was prev, read
	// above with the content compared (see the lock note on this function).
	out := WatermarkStampResult{Advanced: true, PrevWatermark: prev}
	if err := tx.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_yjs_updates
		WHERE item_id = ? AND id > ? AND id <= ? AND content_bearing = TRUE`),
		itemID, prev, cursor).Scan(&out.CoveredContentRows); err != nil {
		return none, fmt.Errorf("watermark stamp: count covered rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return none, err
	}
	return out, nil
}
