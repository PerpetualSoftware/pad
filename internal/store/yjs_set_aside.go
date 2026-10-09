package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3244: edits a collab schema-version rebuild takes out of the op-log.
//
// A join at a new editor schema version cannot replay rows written under the
// old one, so the rebuild empties the item's op-log. The content-bearing rows
// above the flush watermark are edits items.content never received; they are
// MOVED to item_yjs_updates_set_aside first (migration 097 / pg 072), where no
// op-log reader sees them, and they hold content_state at
// models.ContentStateSetAside until they are recovered or explicitly
// discarded. Rows at or below the watermark are already in items.content, and
// non-content-bearing rows cannot change the document (BUG-3124), so neither
// is kept.

// setAsideBeforeClearHook, when set by a test, runs inside the transaction
// before the DELETE: the gap a concurrent append would land in.
var setAsideBeforeClearHook func()

// SetAsideAndClearOpLog moves the item's unflushed content-bearing op-log rows
// to the set-aside table and deletes its whole op-log, in one transaction, so
// a failure leaves the op-log untouched rather than half moved. It returns how
// many rows were set aside and how many op-log rows were deleted.
//
// The rows set aside are chosen from the rows the DELETE itself returned, not
// by a separate read before it. The setup lock the rebuild holds does not stop
// a connected peer's readLoop appending, and on Postgres (READ COMMITTED) each
// statement takes its own snapshot, so a row committed between a copy and a
// delete would be deleted without being considered.
func (s *Store) SetAsideAndClearOpLog(itemID string) (setAside, cleared int64, err error) {
	if itemID == "" {
		return 0, 0, errors.New("SetAsideAndClearOpLog: itemID is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, fmt.Errorf("set aside op-log (begin): %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	if setAsideBeforeClearHook != nil {
		setAsideBeforeClearHook()
	}

	// The DELETE is the transaction's first statement, as the INSERT it
	// replaces was: on SQLite a read first would take a snapshot that a
	// concurrent append invalidates, and the write that follows would then
	// fail with SQLITE_BUSY_SNAPSHOT instead of waiting.
	type deletedRow struct {
		id             int64
		data           []byte
		schemaVersion  string
		createdAt      any
		contentBearing bool
	}
	rows, err := tx.Query(s.dialect.Rebind(`
		DELETE FROM item_yjs_updates WHERE item_id = ?
		RETURNING id, update_data, schema_version, created_at, content_bearing`), itemID)
	if err != nil {
		return 0, 0, fmt.Errorf("set aside op-log (clear): %w", err)
	}
	var deleted []deletedRow
	for rows.Next() {
		var r deletedRow
		if err = rows.Scan(&r.id, &r.data, &r.schemaVersion, &r.createdAt, &r.contentBearing); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("set aside op-log (clear scan): %w", err)
		}
		deleted = append(deleted, r)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return 0, 0, fmt.Errorf("set aside op-log (clear rows): %w", err)
	}
	rows.Close()
	cleared = int64(len(deleted))

	// Read after the delete. A flush that has advanced it covers rows that are
	// in items.content, so a later value is only more accurate.
	var watermark int64
	if err = tx.QueryRow(s.dialect.Rebind(
		`SELECT COALESCE(content_flushed_op_log_id, 0) FROM items WHERE id = ?`), itemID).Scan(&watermark); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("set aside op-log (watermark): %w", err)
	}
	err = nil
	var keep []deletedRow
	for _, r := range deleted {
		if r.contentBearing && r.id > watermark {
			keep = append(keep, r)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].id < keep[j].id })

	now := time.Now().UTC().Format(time.RFC3339)
	for _, r := range keep {
		if _, err = tx.Exec(s.dialect.Rebind(`
			INSERT INTO item_yjs_updates_set_aside
				(item_id, op_log_id, update_data, schema_version, created_at, set_aside_at)
			VALUES (?, ?, ?, ?, ?, ?)`), itemID, r.id, r.data, r.schemaVersion, r.createdAt, now); err != nil {
			return 0, 0, fmt.Errorf("set aside op-log (copy): %w", err)
		}
		setAside++
	}
	// A compaction snapshot, if there was one, was deleted with the log (TASK-3531).
	if err = s.clearOpLogCompactionQ(tx, itemID); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("set aside op-log (commit): %w", err)
	}
	return setAside, cleared, nil
}

// ListYjsSetAside returns the item's set-aside rows in their original op-log
// order, raw bytes included. Empty when there are none.
func (s *Store) ListYjsSetAside(itemID string) ([]models.YjsSetAside, error) {
	if itemID == "" {
		return nil, errors.New("ListYjsSetAside: itemID is required")
	}
	rows, err := s.db.Query(s.dialect.Rebind(`
		SELECT id, item_id, op_log_id, update_data, schema_version, created_at, set_aside_at
		FROM item_yjs_updates_set_aside
		WHERE item_id = ?
		ORDER BY op_log_id ASC, id ASC`), itemID)
	if err != nil {
		return nil, fmt.Errorf("list set-aside updates: %w", err)
	}
	defer rows.Close()

	out := []models.YjsSetAside{}
	for rows.Next() {
		var (
			r                  models.YjsSetAside
			createdAt, movedAt string
		)
		if err := rows.Scan(&r.ID, &r.ItemID, &r.OpLogID, &r.UpdateData, &r.SchemaVersion, &createdAt, &movedAt); err != nil {
			return nil, fmt.Errorf("scan set-aside update: %w", err)
		}
		r.CreatedAt = parseOpLogTime(createdAt)
		r.SetAsideAt = parseOpLogTime(movedAt)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate set-aside updates: %w", err)
	}
	return out, nil
}

// CountYjsSetAsideTx counts the item's set-aside rows inside tx.
func (s *Store) CountYjsSetAsideTx(tx *sql.Tx, itemID string) (int, error) {
	if tx == nil {
		return 0, errors.New("CountYjsSetAsideTx: tx is required")
	}
	var n int
	if err := tx.QueryRow(s.dialect.Rebind(
		`SELECT COUNT(*) FROM item_yjs_updates_set_aside WHERE item_id = ?`), itemID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count set-aside updates: %w", err)
	}
	return n, nil
}

// DeleteYjsSetAsideTx discards the item's set-aside rows inside tx and returns
// how many it deleted. Only an explicit discard may call it: the rows are the
// only copy of those edits.
func (s *Store) DeleteYjsSetAsideTx(tx *sql.Tx, itemID string) (int64, error) {
	if tx == nil {
		return 0, errors.New("DeleteYjsSetAsideTx: tx is required")
	}
	res, err := tx.Exec(s.dialect.Rebind(`DELETE FROM item_yjs_updates_set_aside WHERE item_id = ?`), itemID)
	if err != nil {
		return 0, fmt.Errorf("discard set-aside updates: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("discard set-aside updates (count): %w", err)
	}
	return n, nil
}

// DiscardYjsSetAside is DeleteYjsSetAsideTx in its own transaction, for the
// explicit discard door, which writes nothing else.
func (s *Store) DiscardYjsSetAside(itemID string) (int64, error) {
	if itemID == "" {
		return 0, errors.New("DiscardYjsSetAside: itemID is required")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("discard set-aside updates (begin): %w", err)
	}
	n, err := s.DeleteYjsSetAsideTx(tx, itemID)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("discard set-aside updates (commit): %w", err)
	}
	return n, nil
}

// importSchemaVersionRe is what a bundle's set-aside schema_version must look
// like to be imported (BUG-3244). The server stamps short opaque tags ("1",
// "2-rc1"); anything else did not come from a server and is not stored.
var importSchemaVersionRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// normalizeImportedInstant parses a bundle timestamp in either form the
// op-log is written in and returns it as the RFC3339 UTC the store writes, so
// an imported row is indistinguishable from a native one.
func normalizeImportedInstant(v string) (string, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	t, err := time.Parse("2006-01-02 15:04:05", v)
	if err != nil {
		return "", fmt.Errorf("not an instant: %q", v)
	}
	return t.UTC().Format(time.RFC3339), nil
}

// parseOpLogTime reads an op-log timestamp: RFC3339 as written, or SQLite's
// CURRENT_TIMESTAMP form a fixture may use (the same tolerance
// LoadYjsUpdatesSince applies).
func parseOpLogTime(v string) time.Time {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", v); err == nil {
		return t.UTC()
	}
	return time.Time{}
}
