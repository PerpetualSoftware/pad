package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// AppendYjsUpdate persists a single Yjs binary update for an item and
// returns the new monotonic id. Callers (the WebSocket relay and the
// designated-applier path) treat the returned id as the cursor every
// reconnecting peer compares against to resume from a known point.
//
// schemaVersion is stamped per row. A mismatch on reconnect triggers
// TASK-1268's rebuild flow.
//
// Empty inputs (zero-length update_data, blank itemID, blank
// schemaVersion) are validated up front rather than relying on the
// server-side NOT NULL — a Yjs update of zero bytes is a no-op (its
// presence in the op-log would be misleading) and a blank itemID would
// cascade-detach if the item were ever deleted.
//
// CONTRACT: callers MUST serialize AppendYjsUpdate per item_id.
// Concurrent appends to the same item across multiple goroutines /
// transactions can produce cursor gaps under Postgres because BIGSERIAL
// ids are allocation-ordered, not commit-order: a slower transaction
// can hold a smaller id while a faster one commits a larger id first.
// A reader that advances its cursor past the visible larger id would
// later miss the smaller id when it eventually commits. The dumb-relay
// room manager (TASK-1255) is the sole writer per item by design, so
// the contract is upheld at the application layer; this method does
// NOT take an advisory lock or otherwise serialize internally so it
// stays cheap when the caller already holds the per-room mutex. If a
// future multi-replica deployment lands (deferred IDEA), this contract
// must be re-enforced via Redis-side leadership or per-item locks.
func (s *Store) AppendYjsUpdate(itemID string, data []byte, schemaVersion string) (int64, error) {
	if itemID == "" {
		return 0, errors.New("AppendYjsUpdate: itemID is required")
	}
	if len(data) == 0 {
		return 0, errors.New("AppendYjsUpdate: data must be non-empty")
	}
	if schemaVersion == "" {
		return 0, errors.New("AppendYjsUpdate: schemaVersion is required")
	}

	now := time.Now().UTC().Format(time.RFC3339)

	// Postgres needs RETURNING; SQLite gives us the new rowid via
	// LastInsertId. Same insert payload either way.
	if s.dialect.Driver() == DriverPostgres {
		query := s.dialect.Rebind(`
			INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at)
			VALUES (?, ?, ?, ?)
			RETURNING id
		`)
		var id int64
		if err := s.db.QueryRow(query, itemID, data, schemaVersion, now).Scan(&id); err != nil {
			return 0, fmt.Errorf("append yjs update (postgres): %w", err)
		}
		return id, nil
	}

	res, err := s.db.Exec(
		`INSERT INTO item_yjs_updates (item_id, update_data, schema_version, created_at)
		 VALUES (?, ?, ?, ?)`,
		itemID, data, schemaVersion, now,
	)
	if err != nil {
		return 0, fmt.Errorf("append yjs update (sqlite): %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("append yjs update (last insert id): %w", err)
	}
	return id, nil
}

// LoadYjsUpdatesSince returns the Yjs op-log rows for an item with id
// strictly greater than sinceID, ordered by id ascending.
//
// A sinceID of 0 returns every row. The WebSocket relay uses this on a
// fresh room to rebuild the in-memory Y.Doc, and on reconnect to ship
// any updates that arrived while the client was offline.
func (s *Store) LoadYjsUpdatesSince(itemID string, sinceID int64) ([]models.YjsUpdate, error) {
	if itemID == "" {
		return nil, errors.New("LoadYjsUpdatesSince: itemID is required")
	}

	query := s.dialect.Rebind(`
		SELECT id, item_id, update_data, schema_version, created_at
		FROM item_yjs_updates
		WHERE item_id = ? AND id > ?
		ORDER BY id ASC
	`)

	rows, err := s.db.Query(query, itemID, sinceID)
	if err != nil {
		return nil, fmt.Errorf("load yjs updates: %w", err)
	}
	defer rows.Close()

	var updates []models.YjsUpdate
	for rows.Next() {
		var (
			u         models.YjsUpdate
			createdAt string
		)
		if err := rows.Scan(&u.ID, &u.ItemID, &u.UpdateData, &u.SchemaVersion, &createdAt); err != nil {
			return nil, fmt.Errorf("scan yjs update: %w", err)
		}
		// Tolerate either RFC3339 or a cleaner subset; we only ever
		// write RFC3339 in AppendYjsUpdate, but SQLite's CURRENT_TIMESTAMP
		// idiom uses "2006-01-02 15:04:05" — accepting both keeps any
		// future test fixture or operator-written row from blowing up
		// the load path.
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			u.CreatedAt = t
		} else if t, err := time.Parse("2006-01-02 15:04:05", createdAt); err == nil {
			u.CreatedAt = t.UTC()
		}
		updates = append(updates, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate yjs updates: %w", err)
	}
	return updates, nil
}

// LatestYjsUpdateSchemaVersion returns the `schema_version` AND `id`
// of the most recently appended op-log row for an item. The boolean
// is false when no rows exist yet (a fresh item, or one whose op-log
// was just pruned).
//
// Used by the schema-mismatch rebuild flow (TASK-1268, PLAN-1248):
// if the latest persisted version differs from the server's current
// SCHEMA_VERSION, the room manager prunes the op-log before
// replaying so the new-schema client doesn't replay old-schema ops
// that may be incompatible. The id is also returned so the rebuild
// can compare against items.content_flushed_op_log_id and log
// loudly when the prune is dropping unflushed edits — the user
// can't recover them (old-schema ops can't replay in new schema)
// but operators should see when this happens. Per Codex review of
// TASK-1309 round 4 [P2].
//
// We pick "most recent" rather than "any row" so a server that
// wrote some old-version rows then was rolled back, then forward
// again, still detects the mismatch from the row(s) that matter
// most.
func (s *Store) LatestYjsUpdateSchemaVersion(itemID string) (string, int64, bool, error) {
	if itemID == "" {
		return "", 0, false, errors.New("LatestYjsUpdateSchemaVersion: itemID is required")
	}
	query := s.dialect.Rebind(`
		SELECT schema_version, id
		FROM item_yjs_updates
		WHERE item_id = ?
		ORDER BY id DESC
		LIMIT 1
	`)
	var version string
	var rowID int64
	if err := s.db.QueryRow(query, itemID).Scan(&version, &rowID); err != nil {
		// sql.ErrNoRows is the well-known "no rows" path; surface it
		// as ok=false rather than an error so callers don't have to
		// import database/sql just for this check.
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("latest yjs schema version: %w", err)
	}
	return version, rowID, true, nil
}

// MinOpLogID returns MIN(id) of the persisted op-log rows for an
// item. ok is false when no rows exist (a freshly-pruned or never-
// touched item). Used by the resume-cursor protocol (TASK-1319): a
// reconnecting client announces its last known op-log id via
// `?since=`; if that id is below MIN, rows it expected to replay
// have been pruned and the server signals a force-refresh instead
// of a degraded delta replay.
func (s *Store) MinOpLogID(itemID string) (int64, bool, error) {
	if itemID == "" {
		return 0, false, errors.New("MinOpLogID: itemID is required")
	}
	query := s.dialect.Rebind(`
		SELECT MIN(id) FROM item_yjs_updates WHERE item_id = ?
	`)
	var v sql.NullInt64
	if err := s.db.QueryRow(query, itemID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("min op-log id: %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// MaxOpLogID returns MAX(id) of the persisted op-log rows for an
// item. ok is false when no rows exist. Used by the watermark-
// advancement check on collab-snapshot flush (TASK-1319): the
// server advances `items.content_flushed_op_log_id` only when the
// caller's claimed cursor matches MAX, proving the flushed markdown
// captures every persisted op.
func (s *Store) MaxOpLogID(itemID string) (int64, bool, error) {
	if itemID == "" {
		return 0, false, errors.New("MaxOpLogID: itemID is required")
	}
	query := s.dialect.Rebind(`
		SELECT MAX(id) FROM item_yjs_updates WHERE item_id = ?
	`)
	var v sql.NullInt64
	if err := s.db.QueryRow(query, itemID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("max op-log id: %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// ItemLastRestoreSeq returns items.last_restore_seq — the item.seq stamped by
// the most recent version RESTORE (BUG-2264), or (0, false) when the item has
// never been restored (NULL column) or is missing. This is the DURABLE restore
// boundary the collab Join fence reads to force_refresh a client whose
// ?content_seq seed predates the last restore; unlike the in-memory
// lastRestoreSeqs fast-path, it survives a server restart.
func (s *Store) ItemLastRestoreSeq(itemID string) (int64, bool, error) {
	if itemID == "" {
		return 0, false, errors.New("ItemLastRestoreSeq: itemID is required")
	}
	query := s.dialect.Rebind(`SELECT last_restore_seq FROM items WHERE id = ?`)
	var v sql.NullInt64
	if err := s.db.QueryRow(query, itemID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("item last restore seq: %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// StampRestoreBoundaryOpIDTx sets items.restore_boundary_op_id INSIDE the
// caller's transaction (BUG-2264) — the DURABLE counterpart of the in-memory
// RoomManager.RestoreBoundary. Version-restore stamps it with pre-prune
// MAX(op-log.id)+1 in the SAME tx as the op-log wipe + content write, so the
// collab-snapshot flush gate can reject a stale pre-restore flush even after a
// server restart (when the in-memory boundary is gone). op-log ids are monotonic
// across prunes, so the stored value never falsely rejects a genuine
// post-restore flush.
func (s *Store) StampRestoreBoundaryOpIDTx(tx *sql.Tx, itemID string, boundary int64) error {
	if tx == nil {
		return errors.New("StampRestoreBoundaryOpIDTx: tx is required")
	}
	if itemID == "" {
		return errors.New("StampRestoreBoundaryOpIDTx: itemID is required")
	}
	query := s.dialect.Rebind(`UPDATE items SET restore_boundary_op_id = ? WHERE id = ?`)
	if _, err := tx.Exec(query, boundary, itemID); err != nil {
		return fmt.Errorf("stamp restore boundary op id: %w", err)
	}
	return nil
}

// SetItemRestoreBoundaryOpID is the non-transactional setter for
// items.restore_boundary_op_id. Production stamps the boundary atomically inside
// the restore tx via StampRestoreBoundaryOpIDTx; this standalone form exists for
// callers/tests that set it outside a restore tx (e.g. simulating the
// post-restart state — durable column present, in-memory boundary absent).
func (s *Store) SetItemRestoreBoundaryOpID(itemID string, boundary int64) error {
	if itemID == "" {
		return errors.New("SetItemRestoreBoundaryOpID: itemID is required")
	}
	query := s.dialect.Rebind(`UPDATE items SET restore_boundary_op_id = ? WHERE id = ?`)
	if _, err := s.db.Exec(query, boundary, itemID); err != nil {
		return fmt.Errorf("set restore boundary op id: %w", err)
	}
	return nil
}

// ItemRestoreBoundaryOpID returns items.restore_boundary_op_id — the DURABLE
// op-log-id restore boundary (BUG-2264), or (0, false) when the item has never
// been restored (NULL) or is missing. The collab-snapshot flush gate reads it
// when the in-memory RoomManager.RestoreBoundary misses (after a restart), so a
// surviving pre-restore tab's stale flush is still fenced.
func (s *Store) ItemRestoreBoundaryOpID(itemID string) (int64, bool, error) {
	if itemID == "" {
		return 0, false, errors.New("ItemRestoreBoundaryOpID: itemID is required")
	}
	query := s.dialect.Rebind(`SELECT restore_boundary_op_id FROM items WHERE id = ?`)
	var v sql.NullInt64
	if err := s.db.QueryRow(query, itemID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("item restore boundary op id: %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// GetItemContentFlushedOpLogID returns the value of
// items.content_flushed_op_log_id for an item — the highest op-log
// id known to be reflected in items.content, or (0, false) if the
// item has never been flushed (NULL column value or item missing).
//
// Used by the schema-mismatch rebuild path (TASK-1268) to detect
// when an unsafe prune is about to drop unflushed edits. Per Codex
// review of TASK-1309 round 4 [P2].
func (s *Store) GetItemContentFlushedOpLogID(itemID string) (int64, bool, error) {
	if itemID == "" {
		return 0, false, errors.New("GetItemContentFlushedOpLogID: itemID is required")
	}
	query := s.dialect.Rebind(`
		SELECT content_flushed_op_log_id FROM items WHERE id = ?
	`)
	var (
		v   sql.NullInt64
		err = s.db.QueryRow(query, itemID).Scan(&v)
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("get content flushed op log id: %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// ListDormantOpLogItemsBefore returns the distinct item_ids whose
// ENTIRE op-log is older than the given cutoff AND whose
// items.content_flushed_op_log_id covers every persisted op-log
// row's id. Both conditions are required for the periodic prune
// sweeper (TASK-1309) to safely delete the op-log:
//
//  1. **Dormancy** (`MAX(op-log.created_at) < cutoff`): no recent
//     edits, so any new connect should pick up the canonical state
//     from items.content via lazy-seed (TASK-1261) rather than
//     replaying the live op stream.
//
//  2. **Flush coverage** (`MAX(op-log.id) <=
//     items.content_flushed_op_log_id`): items.content actually
//     contains every edit the op-log persisted. The id is a
//     monotonic AUTOINCREMENT/BIGSERIAL — strict id comparison
//     avoids the false-positive that wall-clock timestamp
//     comparison admits at second granularity (Codex review of
//     TASK-1309 round 4 [P1]).
//
// Without the flush-coverage check, a browser/process death between
// an op-log append and the next 5s collab-snapshot flush would
// leave unflushed edits stuck in the op-log; the GC would delete
// them on the dormancy threshold and the next cold connect would
// lazy-seed stale markdown.
//
// Items with NULL content_flushed_op_log_id are excluded (never-
// flushed, can't safely prune). Items with non-NULL but stale id
// (older than MAX op-log id) are also excluded.
//
// IMPORTANT: returns "all rows are dormant AND flushed" candidates
// only. Yjs op streams are causally linked, so prefix-pruning
// corrupts replay. Whole-log prune of dormant+flushed items is the
// safe operation — future cold connects then lazy-seed from
// items.content, producing a fresh, self-consistent Y.Doc.
//
// Returns an empty slice when no item qualifies (no error). Caller
// iterates the list, takes per-item locks, and calls
// PruneItemOpLogIfDormantBefore — which atomically re-checks
// dormancy. The id watermark is monotonically non-decreasing so
// the JOIN-time check trivially holds under the lock.
func (s *Store) ListDormantOpLogItemsBefore(before time.Time) ([]string, error) {
	cutoff := before.UTC().Format(time.RFC3339)
	query := s.dialect.Rebind(`
		SELECT u.item_id
		FROM item_yjs_updates u
		JOIN items i ON u.item_id = i.id
		WHERE i.content_flushed_op_log_id IS NOT NULL
		GROUP BY u.item_id, i.content_flushed_op_log_id
		HAVING MAX(u.created_at) < ?
		   AND MAX(u.id) <= i.content_flushed_op_log_id
	`)
	rows, err := s.db.Query(query, cutoff)
	if err != nil {
		return nil, fmt.Errorf("list dormant op-log items: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan item id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate item ids: %w", err)
	}
	return ids, nil
}

// PruneItemOpLogIfDormantBefore atomically deletes every op-log row
// for itemID IFF no row exists with created_at >= cutoff. Returns
// the count deleted (0 if the item turned out to be non-dormant).
//
// This is the safe replacement for "list-then-prune" in the GC
// sweeper. Without the conditional NOT EXISTS check, a row appended
// by a live readLoop between the candidate-list query and the
// per-item DELETE would be deleted along with the older rows,
// corrupting the active session's op stream. The conditional DELETE
// is atomic at the SQL layer: either every row of the item gets
// deleted (no recent row at evaluation time) or none do.
//
// Per Codex review of TASK-1309 [P1].
func (s *Store) PruneItemOpLogIfDormantBefore(itemID string, before time.Time) (int64, error) {
	if itemID == "" {
		return 0, errors.New("PruneItemOpLogIfDormantBefore: itemID is required")
	}
	cutoff := before.UTC().Format(time.RFC3339)
	query := s.dialect.Rebind(`
		DELETE FROM item_yjs_updates
		WHERE item_id = ?
		  AND NOT EXISTS (
		    SELECT 1 FROM item_yjs_updates
		    WHERE item_id = ? AND created_at >= ?
		  )
	`)
	res, err := s.db.Exec(query, itemID, itemID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune dormant op-log: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune dormant op-log (rows affected): %w", err)
	}
	return n, nil
}

// PruneYjsUpdatesBefore deletes op-log rows for an item with created_at
// strictly before the given cutoff and returns the row count removed.
//
// **Not safe as a generic GC primitive** — Yjs op streams are
// causally linked, so deleting older rows while keeping newer ones
// corrupts replay (the suffix's references to pruned-prefix structs
// can't be resolved). This method is retained ONLY for callers that
// guarantee the suffix doesn't depend on the prefix:
//
//   - The schema-mismatch rebuild path (TASK-1268) calls it with a
//     far-future cutoff to wipe the entire op-log on a version
//     change — same effective behaviour as the GC sweeper, just
//     reached via a different signal.
//   - PruneAndApply's direct-write fallback (TASK-1257) — the
//     caller guarantees no live readLoop is appending while the
//     prune+items.content-write runs.
//
// New callers SHOULD use ListDormantOpLogItemsBefore +
// PruneItemOpLogIfDormantBefore, which enforce the dormant-only
// invariant + flush-watermark check at the SQL layer.
//
// Per Codex review of TASK-1309 — comment was previously stale,
// describing future GC use that turned out to be unsafe.
func (s *Store) PruneYjsUpdatesBefore(itemID string, before time.Time) (int64, error) {
	if itemID == "" {
		return 0, errors.New("PruneYjsUpdatesBefore: itemID is required")
	}
	cutoff := before.UTC().Format(time.RFC3339)
	query := s.dialect.Rebind(`DELETE FROM item_yjs_updates WHERE item_id = ? AND created_at < ?`)
	res, err := s.db.Exec(query, itemID, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune yjs updates: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("prune yjs updates (rows affected): %w", err)
	}
	return n, nil
}

// MaxOpLogIDTx is the in-transaction variant of MaxOpLogID. Version-restore
// (BUG-2264) reads the pre-prune MAX inside the SAME tx as the op-log wipe +
// content write, so the restore boundary (MAX+1) is captured atomically: a read
// error rolls the whole restore back (clean failure) instead of the previous
// out-of-tx fail-open, where a MAX read error silently published boundary 1 and
// let a stale snapshot carrying any real pre-prune cursor slip through.
func (s *Store) MaxOpLogIDTx(tx *sql.Tx, itemID string) (int64, bool, error) {
	if tx == nil {
		return 0, false, errors.New("MaxOpLogIDTx: tx is required")
	}
	if itemID == "" {
		return 0, false, errors.New("MaxOpLogIDTx: itemID is required")
	}
	query := s.dialect.Rebind(`SELECT MAX(id) FROM item_yjs_updates WHERE item_id = ?`)
	var v sql.NullInt64
	if err := tx.QueryRow(query, itemID).Scan(&v); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("max op-log id (tx): %w", err)
	}
	if !v.Valid {
		return 0, false, nil
	}
	return v.Int64, true, nil
}

// PruneItemOpLogTx deletes an item's ENTIRE op-log inside the caller's
// transaction. Used by version-restore (BUG-2264): the restore's canonical
// items.content write, its "Restored from…" version row, AND the op-log wipe
// must be one atomic unit — a commit-then-prune (or prune-then-commit) split
// leaves a divergent state on any failure (restored content + a stale op-log,
// or a wiped op-log + unchanged content that a cold connect then lazy-seeds
// wrong). Running the DELETE in UpdateItem's own tx (via its precheck hook)
// means a failed update rolls the prune back too, so the three move together
// or not at all (Codex xhigh [P1]).
//
// Wipes every row (no cutoff): a restore makes the old version canonical and
// discards all in-flight collaborative state, so nothing in the op-log should
// survive to be replayed. Safe as a full wipe for the same reason the
// schema-rebuild path is (yjs_updates.go's prefix-prune caveat is about
// deleting a PREFIX while keeping a dependent suffix — deleting everything has
// no dangling causal references).
func (s *Store) PruneItemOpLogTx(tx *sql.Tx, itemID string) error {
	if tx == nil {
		return errors.New("PruneItemOpLogTx: tx is required")
	}
	if itemID == "" {
		return errors.New("PruneItemOpLogTx: itemID is required")
	}
	query := s.dialect.Rebind(`DELETE FROM item_yjs_updates WHERE item_id = ?`)
	if _, err := tx.Exec(query, itemID); err != nil {
		return fmt.Errorf("prune item op-log in tx: %w", err)
	}
	return nil
}
