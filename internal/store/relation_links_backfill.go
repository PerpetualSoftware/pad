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
// ITS SHORT-CIRCUIT IS NOT THE WIKI BACKFILL'S, and the difference is the
// interesting part. BackfillWikiLinks skips an item that already HAS rows. That
// test does not work here: most items carry no relation value at all, so "zero
// rows" is the correct steady state for them and is indistinguishable from
// "never indexed". A per-item EXISTS would therefore re-derive almost every
// item on every boot, forever.
//
// So the guard is asked at the TABLE and the SCHEMA instead:
//
//  1. any row in item_relation_links at all → the index is populated and the
//     write hooks have kept it current since; nothing to do.
//  2. no collection in the database declares a relation field → there is
//     nothing this index could contain; nothing to do.
//
// Only when both are false does it walk items. That makes the steady-state
// cost two cheap queries, and it makes a REBUILD a one-line migration exactly
// as migration 062 did for wiki-links: DELETE the table and the next boot
// repopulates it from scratch.
//
// The residual case is a database that uses relation fields but happens to
// have zero live values — it re-scans each boot. That is bounded by the item
// count and does no writes, and paying it is better than the alternative,
// which is a marker row whose staleness nothing would ever check.
//
// PLAN-2857 U5 / TASK-2997.
func (s *Store) BackfillRelationLinks() (*BackfillRelationLinksResult, error) {
	result := &BackfillRelationLinksResult{}

	var existing int
	if err := s.db.QueryRow(s.q(`SELECT 1 FROM item_relation_links LIMIT 1`)).Scan(&existing); err == nil {
		result.Skipped = true
		return result, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("probe relation links: %w", err)
	}

	// Which collections declare a relation field, and what their relation keys
	// are. One pass, so the per-item work below is a map lookup rather than a
	// schema parse.
	relationKeysByCollection, err := s.collectionsWithRelationFields()
	if err != nil {
		return nil, err
	}
	if len(relationKeysByCollection) == 0 {
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
	return result, nil
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
