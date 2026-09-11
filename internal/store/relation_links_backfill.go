package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// BackfillRelationLinksResult reports what the backfill did so cmd/pad can log
// a one-line summary at startup.
type BackfillRelationLinksResult struct {
	// Skipped is true when the backfill decided it had nothing to do without
	// scanning items — see BackfillRelationLinks for the two ways that
	// happens.
	Skipped bool

	// ItemsScanned counts the live items the backfill walked.
	ItemsScanned int

	// LinksInserted is the number of index rows written.
	LinksInserted int
}

// BackfillRelationLinks derives `item_relation_links` for every live item.
// Called from server startup after migrations.
//
// ITS GUARD IS A COMPLETION MARKER, and the two shapes it is not are both
// instructive.
//
// It is not the wiki backfill's per-item EXISTS. That test does not work here:
// most items carry no relation value, so "zero rows" is their correct steady
// state and is indistinguishable from "never indexed" — a per-item EXISTS
// would re-derive nearly every item on every boot, forever.
//
// It is also no longer "does the table have any row", which is what I wrote
// first and which codex round 1 correctly called a P1. Rows commit per item,
// so a crash partway through leaves the table NON-EMPTY AND INCOMPLETE — and
// that guard then skips forever, with the missing edges never derived. The
// "rebuild by deleting the table" story only ever covered deliberate deletion,
// not an interrupted run.
//
// So completion is recorded EXPLICITLY, in platform_settings, and only AFTER a
// full pass finishes. A crash before that leaves no marker and the next boot
// re-derives — which is safe because replaceRelationLinks deletes before it
// inserts, so a repeat pass is idempotent rather than additive. The marker is
// written after the work for the same reason a dedupe token must be: writing
// it first turns a mid-run failure into a permanent silent loss.
//
// A REBUILD is still one line, just a different one: delete the marker (or the
// table and the marker) and restart.
//
// The schema probe stays as a cheap second skip — a database where no
// collection declares a relation field has nothing to derive — and it records
// completion too, because a pass over zero work is a completed pass. A
// relation field arriving later is handled by UpdateCollection's reindex and
// the write hooks, not by this.
//
// PLAN-2857 U5 / TASK-2997.
func (s *Store) BackfillRelationLinks() (*BackfillRelationLinksResult, error) {
	result := &BackfillRelationLinksResult{}

	var marker string
	err := s.db.QueryRow(s.q(`SELECT value FROM platform_settings WHERE key = ?`), relationLinksBackfilledFlag).Scan(&marker)
	if err == nil && marker == "1" {
		result.Skipped = true
		return result, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("probe relation backfill marker: %w", err)
	}

	// Which collections declare a relation field, and what their relation keys
	// are. One pass, so the per-item work below is a map lookup rather than a
	// schema parse.
	relationKeysByCollection, err := s.collectionsWithRelationFields()
	if err != nil {
		return nil, err
	}
	if len(relationKeysByCollection) == 0 {
		// A pass over zero work is a completed pass.
		if err := s.markRelationLinksBackfilled(); err != nil {
			return nil, err
		}
		result.Skipped = true
		return result, nil
	}

	rows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, collection_id, fields
		FROM items
		WHERE deleted_at IS NULL
	`))
	if err != nil {
		return nil, fmt.Errorf("scan items for relation backfill: %w", err)
	}
	type itemRow struct{ id, workspaceID, collectionID, fields string }
	var items []itemRow
	for rows.Next() {
		var r itemRow
		if scanErr := rows.Scan(&r.id, &r.workspaceID, &r.collectionID, &r.fields); scanErr != nil {
			rows.Close()
			return nil, fmt.Errorf("scan relation backfill row: %w", scanErr)
		}
		items = append(items, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate relation backfill rows: %w", rowsErr)
	}
	rows.Close()

	for _, it := range items {
		keys, ok := relationKeysByCollection[it.collectionID]
		if !ok {
			continue
		}
		result.ItemsScanned++
		edges := relationValuesFromBlob(it.fields, keys)
		if len(edges) == 0 {
			continue
		}
		// Per-item transaction, matching the wiki backfill: one bad row must
		// not poison the whole pass.
		tx, err := s.db.Begin()
		if err != nil {
			return nil, fmt.Errorf("begin relation backfill tx: %w", err)
		}
		if err := s.replaceRelationLinks(tx, it.id, it.workspaceID, it.collectionID, it.fields); err != nil {
			tx.Rollback() //nolint:errcheck // the error below is the one that matters
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit relation backfill tx: %w", err)
		}
		result.LinksInserted += len(edges)
	}

	// ONLY NOW. Every earlier return path is a failure, and none of them
	// records completion — so an interrupted run is re-derived on the next
	// boot rather than skipped forever.
	if err := s.markRelationLinksBackfilled(); err != nil {
		return nil, err
	}
	return result, nil
}

// relationLinksBackfilledFlag is the platform_settings key recording that a
// FULL relation-index derivation has completed. Same mechanism the
// webhook-secret encryption migration uses for the same question.
const relationLinksBackfilledFlag = "relation_links_backfilled"

// markRelationLinksBackfilled records a completed pass.
func (s *Store) markRelationLinksBackfilled() error {
	if _, err := s.db.Exec(s.q(`
		INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`), relationLinksBackfilledFlag, "1", now()); err != nil {
		return fmt.Errorf("persist relation backfill marker: %w", err)
	}
	return nil
}

// collectionsWithRelationFields maps collection id -> its relation field keys,
// for every collection that declares at least one.
func (s *Store) collectionsWithRelationFields() (map[string]map[string]struct{}, error) {
	rows, err := s.db.Query(s.q(`SELECT id, schema FROM collections`))
	if err != nil {
		return nil, fmt.Errorf("scan collections for relation keys: %w", err)
	}
	defer rows.Close()
	out := map[string]map[string]struct{}{}
	for rows.Next() {
		var id, schemaJSON string
		if err := rows.Scan(&id, &schemaJSON); err != nil {
			return nil, fmt.Errorf("scan collection schema: %w", err)
		}
		keys := relationKeysFromSchemaJSON(schemaJSON)
		if len(keys) > 0 {
			out[id] = keys
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate collection schemas: %w", err)
	}
	return out, nil
}
