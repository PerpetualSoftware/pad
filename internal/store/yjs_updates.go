package store

import (
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

// PruneYjsUpdatesBefore deletes op-log rows for an item with created_at
// strictly before the given cutoff and returns the row count removed.
//
// Used by the eventual GC sweeper (out of scope for this task) to
// reclaim space after a successful markdown snapshot makes the older
// op-log rows redundant. items.content remains canonical, so pruning
// is safe — at most we lose the ability to do live op-replay for very
// old peer reconnects, who will fall back to the markdown snapshot.
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
