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
	return relationKeysFromSchemaJSON(schemaJSON.String), nil
}

// relationKeysFromSchemaJSON returns the field keys a collection schema
// declares as `relation` (or `multi_relation`).
//
// ONE function so the write hook and the backfill cannot disagree about what
// counts as a relation field — two copies of this rule would drift the moment
// a third relation-ish type appears, and the symptom would be an index that is
// correct at write time and wrong after a rebuild.
//
// A schema that will not parse yields no keys rather than an error: that is a
// different defect, reported elsewhere, and the index declines to be the
// second voice.
func relationKeysFromSchemaJSON(schemaJSON string) map[string]struct{} {
	if strings.TrimSpace(schemaJSON) == "" {
		return nil
	}
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return nil
	}
	keys := map[string]struct{}{}
	for _, def := range schema.Fields {
		if def.Type == "relation" || def.Type == "multi_relation" {
			keys[def.Key] = struct{}{}
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
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

// RelationBacklink is one item that points at the target through a relation
// field, plus the field it points through.
//
// FieldKey is the part that makes this not a wiki backlink: the reverse side
// can say "referenced by CAR-3 via `color`" rather than just "referenced by",
// which is the whole reason this index is its own table.
type RelationBacklink struct {
	SourceItemID   string `json:"source_item_id"`
	SourceRef      string `json:"source_ref"`
	SourceTitle    string `json:"source_title"`
	CollectionSlug string `json:"collection_slug"`
	FieldKey       string `json:"field_key"`
	FieldLabel     string `json:"field_label,omitempty"`
}

// relationBacklinkCap bounds any single page. Matches GetBacklinks' cap for
// the same reason: a caller that asks for everything should not be able to.
const relationBacklinkCap = 300

// GetRelationBacklinks returns the live items that reference targetItemID
// through a `relation` field, filtered to what this viewer may see.
//
// VISIBILITY IS THE VIEWER'S, NOT THE TRUTH (ratified, day 55). `vis` is built
// from ResolveBacklinksVisibility — the same resolver the wiki-link reverse
// side uses, reused verbatim rather than reinvented — and the consequence is
// on the record deliberately: "Referenced by 3" means "by 3 you can see". A
// true count would leak the existence of items the viewer has no access to,
// which is the exact leak that resolver exists to close.
//
// Filtering happens in SQL, not after the fetch, so LIMIT counts VISIBLE rows.
// Filtering above the limit would let invisible rows consume page slots and
// silently shrink pages — and worse, a page that came back short would itself
// be a signal about how many hidden rows there are.
//
// Soft-deleted SOURCES are excluded by the join, so a deleted item stops
// referencing and a restore brings it back without re-deriving anything. A
// soft-deleted TARGET keeps its rows: the question "who pointed at this?"
// still has an answer after deletion.
func (s *Store) GetRelationBacklinks(targetItemID, workspaceID string, limit, offset int, vis BacklinksVisibility) ([]RelationBacklink, error) {
	if limit <= 0 || limit > relationBacklinkCap {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	// Restricted with nothing visible → empty without a query. Postgres
	// rejects the `IN ()` form this would otherwise build.
	if !vis.Unrestricted && len(vis.FullCollectionIDs) == 0 && len(vis.GrantedItemIDs) == 0 {
		return nil, nil
	}

	args := []any{targetItemID, workspaceID}
	visClause := ""
	if !vis.Unrestricted {
		collClause := "FALSE"
		if len(vis.FullCollectionIDs) > 0 {
			ph := make([]string, len(vis.FullCollectionIDs))
			for i, cid := range vis.FullCollectionIDs {
				ph[i] = "?"
				args = append(args, cid)
			}
			collClause = "s.collection_id IN (" + strings.Join(ph, ",") + ")"
		}
		itemClause := "FALSE"
		if len(vis.GrantedItemIDs) > 0 {
			ph := make([]string, len(vis.GrantedItemIDs))
			for i, iid := range vis.GrantedItemIDs {
				ph[i] = "?"
				args = append(args, iid)
			}
			itemClause = "s.id IN (" + strings.Join(ph, ",") + ")"
		}
		visClause = " AND (" + collClause + " OR " + itemClause + ")"
	}
	args = append(args, limit, offset)

	rows, err := s.db.Query(s.q(`
		SELECT rl.source_item_id, rl.source_field_key,
		       s.title, s.item_number, c.prefix, c.slug
		FROM item_relation_links rl
		JOIN items s ON s.id = rl.source_item_id
		JOIN collections c ON c.id = s.collection_id
		WHERE rl.target_item_id = ? AND rl.workspace_id = ?
		  AND s.deleted_at IS NULL
		  -- A soft-deleted COLLECTION leaves its items live, so without this a
		  -- source in a collection the viewer can no longer open still shows
		  -- up here — including for an Unrestricted viewer, who has no
		  -- collection filter to catch it (codex round 2).
		  AND c.deleted_at IS NULL
		  AND s.id != rl.target_item_id`+visClause+`
		ORDER BY s.updated_at DESC, rl.source_item_id, rl.source_field_key
		LIMIT ? OFFSET ?
	`), args...)
	if err != nil {
		return nil, fmt.Errorf("query relation backlinks: %w", err)
	}
	defer rows.Close()

	var out []RelationBacklink
	for rows.Next() {
		var b RelationBacklink
		// item_number is NULLABLE on legacy rows — scanning it into an int
		// errors and would take the whole list down, the same defect U6 shipped
		// in hydration (codex round 7 there).
		var number sql.NullInt64
		var prefix string
		if err := rows.Scan(&b.SourceItemID, &b.FieldKey, &b.SourceTitle, &number, &prefix, &b.CollectionSlug); err != nil {
			return nil, fmt.Errorf("scan relation backlink: %w", err)
		}
		if number.Valid {
			b.SourceRef = fmt.Sprintf("%s-%d", prefix, number.Int64)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate relation backlinks: %w", err)
	}
	return out, nil
}

// CountRelationBacklinks is GetRelationBacklinks' count, under the SAME
// visibility and the SAME filters.
//
// It exists as its own query rather than len() of the page because the UI
// wants "Referenced by N" without fetching N rows. The filters must stay in
// lockstep with GetRelationBacklinks — a drift would let the header promise a
// number the list cannot produce, which is exactly the failure CountBacklinks
// documents for the wiki side.
func (s *Store) CountRelationBacklinks(targetItemID, workspaceID string, vis BacklinksVisibility) (int, error) {
	if !vis.Unrestricted && len(vis.FullCollectionIDs) == 0 && len(vis.GrantedItemIDs) == 0 {
		return 0, nil
	}
	args := []any{targetItemID, workspaceID}
	visClause := ""
	if !vis.Unrestricted {
		collClause := "FALSE"
		if len(vis.FullCollectionIDs) > 0 {
			ph := make([]string, len(vis.FullCollectionIDs))
			for i, cid := range vis.FullCollectionIDs {
				ph[i] = "?"
				args = append(args, cid)
			}
			collClause = "s.collection_id IN (" + strings.Join(ph, ",") + ")"
		}
		itemClause := "FALSE"
		if len(vis.GrantedItemIDs) > 0 {
			ph := make([]string, len(vis.GrantedItemIDs))
			for i, iid := range vis.GrantedItemIDs {
				ph[i] = "?"
				args = append(args, iid)
			}
			itemClause = "s.id IN (" + strings.Join(ph, ",") + ")"
		}
		visClause = " AND (" + collClause + " OR " + itemClause + ")"
	}
	var n int
	err := s.db.QueryRow(s.q(`
		SELECT COUNT(*)
		FROM item_relation_links rl
		JOIN items s ON s.id = rl.source_item_id
		JOIN collections c ON c.id = s.collection_id
		WHERE rl.target_item_id = ? AND rl.workspace_id = ?
		  AND s.deleted_at IS NULL
		  -- Same filter as the page query. The two must stay identical or the
		  -- header promises a number the list cannot produce.
		  AND c.deleted_at IS NULL
		  AND s.id != rl.target_item_id`+visClause), args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count relation backlinks: %w", err)
	}
	return n, nil
}
