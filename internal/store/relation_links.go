package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The materialised reverse index for `relation` field values
// (PLAN-2857 U5 / TASK-2997). See migration 088 for why it is materialised
// and why it is its own table.
//
// THE INDEX IS A FUNCTION OF (BLOB, SCHEMA), and both halves matter. Which
// keys are relations is a property of the COLLECTION, so the same blob
// indexes differently after a move, and a schema change can invalidate every
// item in a collection with no item write at all. That second case is handled
// by ReindexCollectionRelationLinks, called inside the schema-update
// transaction (lead ruling on the U5 contract) — "it fixes itself on the next
// write" is false for items nobody edits, which is the same argument that
// made this index materialised rather than derived.
//
// THIS FUNCTION DOES NOT RESOLVE ANYTHING. Since U6 a relation value may be
// SUPPLIED as a title, but the resolver canonicalises it to an id in place
// before any store write, so the blob reaching here already holds ids. Doing
// its own lookup would be a second, divergent resolver — and would index a
// title on any path where the resolver had not run.

// replaceRelationLinks rebuilds the reverse-index rows for one source item.
//
// Must run inside `tx` — the caller owns transactionality — and is called at
// every store site that writes an item's `fields` blob, not at the doors. A
// door-level hook is a per-door wiring claim and the door population grows; a
// store-level one cannot be bypassed by a door that does not exist yet. That
// is also the shape replaceWikiLinks established for the content index.
//
// Idempotent: the same (item, blob, schema) twice yields the same row set.
// Calling it on a write that did not touch a relation value is a deliberate
// no-op rather than an omission, so no caller has to reason about which
// writes "can" matter.
func (s *Store) replaceRelationLinks(tx *sql.Tx, sourceItemID, workspaceID, collectionID, fieldsJSON string) error {
	// Delete first so callers never pre-clear, and so a blob that has lost
	// its relation values correctly ends with zero rows.
	if _, err := tx.Exec(s.q(`DELETE FROM item_relation_links WHERE source_item_id = ?`), sourceItemID); err != nil {
		return fmt.Errorf("delete prior relation links: %w", err)
	}

	schema, err := s.relationSchemaForCollectionTx(tx, collectionID)
	if err != nil {
		return err
	}
	if len(schema) == 0 {
		// The collection declares no relation fields, so there is nothing to
		// index. Not an error, and the delete above still ran — which is what
		// makes retyping a field away from `relation` self-healing on the
		// next write of each item.
		return nil
	}

	values := relationValuesFromBlob(fieldsJSON, schema)
	if len(values) == 0 {
		return nil
	}

	for _, v := range values {
		if _, err := tx.Exec(s.q(`
			INSERT INTO item_relation_links
				(source_item_id, source_field_key, target_item_id, workspace_id, ordinal)
			VALUES (?, ?, ?, ?, ?)
		`), sourceItemID, v.fieldKey, v.targetID, workspaceID, v.ordinal); err != nil {
			return fmt.Errorf("insert relation link %s/%s: %w", v.fieldKey, v.targetID, err)
		}
	}
	return nil
}

// relationLinkRow is one edge about to be written.
type relationLinkRow struct {
	fieldKey string
	targetID string
	ordinal  int
}

// relationValuesFromBlob extracts the relation edges a fields blob carries,
// given the set of keys the collection declares as relations.
//
// A blob that will not parse yields NO edges rather than an error: a corrupt
// blob is a different defect that other code reports, and refusing the write
// here would make this index able to block an item's save.
func relationValuesFromBlob(fieldsJSON string, relationKeys map[string]struct{}) []relationLinkRow {
	trimmed := strings.TrimSpace(fieldsJSON)
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return nil
	}
	var blob map[string]any
	if err := json.Unmarshal([]byte(trimmed), &blob); err != nil {
		return nil
	}
	var out []relationLinkRow
	for key := range relationKeys {
		raw, present := blob[key]
		if !present || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case string:
			if id := strings.TrimSpace(v); id != "" {
				out = append(out, relationLinkRow{fieldKey: key, targetID: id})
			}
		case []any:
			// `multi_relation` (U4, not yet landed). Indexed now so the table
			// does not need a second pass when that type arrives, and so a
			// hand-written array value is not silently unindexed today.
			for i, entry := range v {
				id, isStr := entry.(string)
				if !isStr {
					continue
				}
				if id = strings.TrimSpace(id); id != "" {
					out = append(out, relationLinkRow{fieldKey: key, targetID: id, ordinal: i})
				}
			}
		}
	}
	return out
}

// relationSchemaForCollectionTx returns the set of field keys the collection
// declares as `relation` (or `multi_relation`).
//
// Reads through the transaction on purpose: a schema change and the reindex it
// triggers happen in ONE transaction, so a pool read here would see the
// pre-change schema and rebuild the index to match the shape being replaced.
func (s *Store) relationSchemaForCollectionTx(tx *sql.Tx, collectionID string) (map[string]struct{}, error) {
	var schemaJSON sql.NullString
	err := tx.QueryRow(s.q(`SELECT schema FROM collections WHERE id = ?`), collectionID).Scan(&schemaJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read collection schema for relation index: %w", err)
	}
	if !schemaJSON.Valid || strings.TrimSpace(schemaJSON.String) == "" {
		return nil, nil
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(schemaJSON.String), &schema); err != nil {
		// A schema that will not parse is a different defect, reported
		// elsewhere; the index declines to be the second voice.
		return nil, nil
	}
	keys := map[string]struct{}{}
	for _, def := range schema.Fields {
		if def.Type == "relation" || def.Type == "multi_relation" {
			keys[def.Key] = struct{}{}
		}
	}
	return keys, nil
}

// ReindexCollectionRelationLinks rebuilds the reverse index for every live
// item in one collection.
//
// WHY THIS EXISTS (lead ruling on the U5 contract). The index is a function of
// (blob, schema), and a SCHEMA change moves the second half without touching a
// single item. Declare a `relation` field on a collection that already holds
// values under that key, or retype one away, and every existing item is
// mis-indexed. "It fixes itself on the next write of each item" is false for
// items nobody edits — which is the same argument that made this index
// materialised rather than derived at read time, so accepting it here would
// undo the decision.
//
// Runs INSIDE the schema-update transaction, so the property is: after a
// committed schema change, no item in the collection is mis-indexed. A failure
// rolls the schema change back with it rather than committing a schema the
// index does not match.
//
// Bounded by the collection's live item count. The cost at 10k items is
// recorded on TASK-2997's trail, because "a rare owner-only operation" is a
// claim about frequency, not about cost, and the two are worth separating.
func (s *Store) ReindexCollectionRelationLinks(tx *sql.Tx, collectionID, workspaceID string) error {
	// Ids first, then per-item work: holding a SELECT cursor open while
	// writing upsets some drivers, the same reason the wiki backfill collects
	// before it writes.
	rows, err := tx.Query(s.q(`
		SELECT id, fields FROM items
		WHERE collection_id = ? AND workspace_id = ? AND deleted_at IS NULL
	`), collectionID, workspaceID)
	if err != nil {
		return fmt.Errorf("scan collection for relation reindex: %w", err)
	}
	type row struct{ id, fields string }
	var items []row
	for rows.Next() {
		var r row
		if scanErr := rows.Scan(&r.id, &r.fields); scanErr != nil {
			rows.Close()
			return fmt.Errorf("scan relation reindex row: %w", scanErr)
		}
		items = append(items, r)
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		rows.Close()
		return fmt.Errorf("iterate relation reindex rows: %w", rowsErr)
	}
	rows.Close()

	for _, r := range items {
		if err := s.replaceRelationLinks(tx, r.id, workspaceID, collectionID, r.fields); err != nil {
			return err
		}
	}
	return nil
}
