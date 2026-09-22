package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// BUG-3133: `expected_seq` guards the ROW, and a browser tab's typing does not
// touch the row — it lands in the op-log and reaches items.content only when a
// tab flushes. So a content write carrying a current token used to replace
// unflushed tab edits the caller never saw: through the designated applier's
// setContent when a tab is open, and through PruneItemOpLogTx — which deletes
// them outright — when none is.
//
// The predicate is content_state's (contentStateSQLFor): content-bearing
// op-log rows above items.content_flushed_op_log_id. Counting it inside the
// write's own transaction is what makes the refusal agree with the write it
// guards rather than with a read taken earlier.

// ContentPendingFlushError refuses a TOKEN-CARRYING content write while the
// item's op-log holds content-bearing rows above its flush watermark. A caller
// that asserted "the body as of seq N" did not see those edits, so replacing
// them is not what it asked for. Re-reading does not clear it — the row, and
// its seq, are unchanged until a tab flushes — so it is its own error rather
// than an UpdateConflictError, which callers answer by re-reading and retrying.
type ContentPendingFlushError struct {
	ItemID      string
	PendingRows int
}

func (e *ContentPendingFlushError) Error() string {
	return fmt.Sprintf("item %s has %d unflushed collaborative edit row(s); refusing a content write guarded by a version token",
		e.ItemID, e.PendingRows)
}

// AsContentPendingFlushError unwraps err to a *ContentPendingFlushError.
func AsContentPendingFlushError(err error) (*ContentPendingFlushError, bool) {
	var e *ContentPendingFlushError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}

// CountPendingContentRowsTx counts the item's content-bearing op-log rows above
// its flush watermark, inside tx. Zero means content_state would read clean.
func (s *Store) CountPendingContentRowsTx(tx *sql.Tx, itemID string) (int, error) {
	if tx == nil {
		return 0, errors.New("CountPendingContentRowsTx: tx is required")
	}
	var n int
	err := tx.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_yjs_updates u
		JOIN items i ON i.id = u.item_id
		WHERE u.item_id = ?
		  AND u.id > COALESCE(i.content_flushed_op_log_id, 0)
		  AND u.content_bearing = TRUE`), itemID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count pending content rows: %w", err)
	}
	return n, nil
}
