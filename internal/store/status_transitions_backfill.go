package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// extractFieldValue pulls a named string field out of an item's fields JSON
// blob. Returns "" when the blob is empty, unparseable, or carries no string
// value for the key — callers treat "" as "no value" (so an unset → set
// transition has an empty FromStatus, and a collection whose done field is
// unset never records a row).
func extractFieldValue(fieldsJSON, key string) string {
	if fieldsJSON == "" || key == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// parseFieldChange extracts the from/to value for a given field out of a single
// activities.metadata `changes` string. The string is built by
// handlers_documents.go::diffFields as a "; "-joined list of "key: old → new"
// segments (and "key: → new" when the key was newly set). We locate the
// "<fieldKey>:" segment and split it on the UTF-8 arrow.
//
// Returns ok=false when there is no segment for fieldKey. from may be ""
// (newly-set value); to is always non-empty when ok.
func parseFieldChange(changes, fieldKey string) (from, to string, ok bool) {
	prefix := fieldKey + ":"
	for _, seg := range strings.Split(changes, ";") {
		seg = strings.TrimSpace(seg)
		rest, found := strings.CutPrefix(seg, prefix)
		if !found {
			continue
		}
		parts := strings.SplitN(rest, "→", 2)
		if len(parts) != 2 {
			// Malformed (no arrow) — skip rather than guess.
			return "", "", false
		}
		from = strings.TrimSpace(parts[0])
		to = strings.TrimSpace(parts[1])
		if to == "" {
			return "", "", false
		}
		return from, to, true
	}
	return "", "", false
}

// doneFieldKey resolves the select field whose value represents a collection's
// workflow/done state (CollectionSettings.BoardGroupBy, else "status"). Used by
// the status-transition capture so collections grouped by a non-status field
// (hiring Candidates → "stage"/"result", etc.) record transitions on the right
// field. Best-effort: any lookup/parse failure falls back to "status" so
// capture degrades gracefully rather than dropping rows.
func (s *Store) doneFieldKey(collectionID string) string {
	col, err := s.GetCollection(collectionID)
	if err != nil || col == nil {
		return "status"
	}
	return doneFieldKeyFromCollection(col)
}

func doneFieldKeyFromCollection(col *models.Collection) string {
	return doneFieldKeyFromSchemaJSON(col.Schema, col.Settings)
}

// doneFieldKeyFromSchemaJSON resolves the done-field key from the raw schema +
// settings JSON strings (as stored on the collections row). Falls back to
// "status" on any parse failure. Lets callers that already hold the raw JSON
// (e.g. an in-tx read) resolve the key without a models.Collection.
func doneFieldKeyFromSchemaJSON(schemaJSON, settingsJSON string) string {
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(schemaJSON), &schema); err != nil {
		return "status"
	}
	var settings models.CollectionSettings
	if settingsJSON != "" {
		_ = json.Unmarshal([]byte(settingsJSON), &settings)
	}
	return models.DoneFieldKey(schema, settings)
}

// BackfillStatusTransitionsResult reports what the backfill did so the
// startup hook can log a one-line summary.
type BackfillStatusTransitionsResult struct {
	// Skipped is true when the table already had rows and the backfill
	// short-circuited without scanning the activity log.
	Skipped bool
	// ActivitiesScanned is the number of status-bearing activity rows
	// the backfill iterated.
	ActivitiesScanned int
	// Inserted is the number of status_transitions rows written.
	Inserted int
	// Errors counts activity rows skipped due to a parse or write failure.
	// Errors are logged at WARN but never abort the run.
	Errors int
}

// BackfillStatusTransitions populates status_transitions from history in two
// passes, designed to be called once from server startup after migrations run
// (mirrors BackfillWikiLinks):
//
//  1. Activity-derived hops — parse each "updated" activity's metadata.changes
//     for a change to the item's collection done field (DoneFieldKey: "status"
//     for most collections, but e.g. "stage"/"result" for hiring/interviewing).
//  2. Create-time rows — one "" → initial-value row per item at its created_at,
//     so an item created directly in a terminal value still counts as a
//     completion. The initial value is the `from` of the item's earliest
//     recorded change, or its current done-field value if it never changed.
//
// Idempotency: gated on the table being empty. On first boot after migration
// 063/042 the table is empty and the historical activity log is replayed; on
// every subsequent boot the write-path hook has kept the table non-empty, so
// the backfill short-circuits immediately. This means the historical replay
// is best-effort and runs exactly once — a partial run that crashed midway is
// NOT resumed (the next boot sees rows and skips). The cost of a missed
// historical row is a slightly understated pre-upgrade report window; live
// data from the write-path hook is always complete.
//
// Two best-effort caveats apply to historical (pre-upgrade) rows only — live
// write-path rows have neither:
//
//   - Debounce: the activity feed is debounce-coalesced (mergeActivityMeta
//     collapses same-field runs), so rapid hops may be undercounted. This is
//     the very limitation the structured table fixes going forward.
//   - Collection attribution: backfilled rows are stamped with the item's
//     CURRENT collection_id. The activity log doesn't record which collection
//     an item was in at the time of a past status change, and reconstructing
//     it would require replaying each item's move history. For the common case
//     (an item never changes collection) this is exact; only a transition that
//     happened BEFORE a later collection move is mis-attributed to the newer
//     collection. The live move path (MoveItemWithPreCheck) stamps the
//     collection at transition time, so going-forward data is always correct.
//
// PLAN-1628 / TASK-1637.
func (s *Store) BackfillStatusTransitions() (*BackfillStatusTransitionsResult, error) {
	result := &BackfillStatusTransitionsResult{}

	var has bool
	if err := s.db.QueryRow(s.q(`
		SELECT EXISTS(SELECT 1 FROM status_transitions)
	`)).Scan(&has); err != nil {
		return nil, fmt.Errorf("status-transition backfill existence check: %w", err)
	}
	if has {
		result.Skipped = true
		return result, nil
	}

	// Per-collection done-field-key memo. DoneFieldKey loads + parses the
	// collection schema, so cache it across the two passes below.
	doneKeyCache := map[string]string{}
	doneKey := func(collectionID string) string {
		if k, ok := doneKeyCache[collectionID]; ok {
			return k
		}
		k := s.doneFieldKey(collectionID)
		doneKeyCache[collectionID] = k
		return k
	}

	// Dialect-aware conflict clause makes every backfilled insert idempotent
	// on its primary key. Combined with the deterministic ids below, this
	// closes the non-atomic gap the empty-table gate leaves: if two processes
	// ever replay history concurrently (a future multi-replica Postgres
	// deploy — single-instance today), the second insert is a no-op rather
	// than a duplicate that would overcount reports. The live write paths use
	// a random newID(), so live rows never collide with these.
	insertSQL := `INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if s.dialect.Driver() == DriverPostgres {
		insertSQL += ` ON CONFLICT (id) DO NOTHING`
	} else {
		insertSQL = strings.Replace(insertSQL, "INSERT INTO", "INSERT OR IGNORE INTO", 1)
	}
	insert := func(id, itemID, wsID, collID, fieldKey, from, to, createdAt string) {
		res, err := s.db.Exec(s.q(insertSQL), id, itemID, wsID, collID, fieldKey, from, to, createdAt)
		if err != nil {
			slog.Warn("status-transition backfill: insert failed",
				slog.String("item_id", itemID), slog.String("err", err.Error()))
			result.Errors++
			return
		}
		// Count only rows that actually landed — a conflict (already
		// backfilled by a concurrent process) reports 0 affected.
		if n, aerr := res.RowsAffected(); aerr == nil && n > 0 {
			result.Inserted++
		}
	}

	// --- Pass 1: activity-derived transitions ---
	// Join to items for the authoritative workspace_id + collection_id
	// (activities has workspace_id but not collection_id). Only "updated"
	// activities carry a changes blob; the LIKE prunes to rows that record a
	// field change (every "key: a → b" segment contains the arrow). Order by
	// created_at so multi-hop histories insert chronologically AND so the
	// firstChangeFrom map below captures each item's earliest hop.
	rows, err := s.db.Query(s.q(`
		SELECT a.id, a.document_id, i.workspace_id, i.collection_id, a.metadata, a.created_at
		FROM activities a
		JOIN items i ON i.id = a.document_id
		WHERE a.action = 'updated'
		  AND a.metadata LIKE '%→%'
		ORDER BY a.created_at
	`))
	if err != nil {
		return nil, fmt.Errorf("scan activities for status-transition backfill: %w", err)
	}
	defer rows.Close()

	// Buffer rows before writing — some drivers dislike interleaving
	// INSERTs with an open SELECT cursor (same caution as BackfillWikiLinks).
	type activityRow struct {
		activityID, itemID, workspaceID, collectionID, metadata, createdAt string
	}
	var scanned []activityRow
	for rows.Next() {
		var ar activityRow
		if err := rows.Scan(&ar.activityID, &ar.itemID, &ar.workspaceID, &ar.collectionID, &ar.metadata, &ar.createdAt); err != nil {
			return nil, fmt.Errorf("scan status-transition backfill row: %w", err)
		}
		scanned = append(scanned, ar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate status-transition backfill rows: %w", err)
	}
	rows.Close()

	// firstChangeFrom[itemID] = the `from` value of the item's EARLIEST
	// done-field change. Because rows are ordered by created_at, the first
	// time we see an item is its earliest hop, and that hop's `from` is the
	// item's initial value — used by Pass 2 to seed the create-time row.
	firstChangeFrom := map[string]string{}

	for _, ar := range scanned {
		result.ActivitiesScanned++

		var meta struct {
			Changes string `json:"changes"`
		}
		if err := json.Unmarshal([]byte(ar.metadata), &meta); err != nil {
			result.Errors++
			continue
		}
		key := doneKey(ar.collectionID)
		from, to, ok := parseFieldChange(meta.Changes, key)
		if !ok {
			// LIKE matched a change, but not on this collection's done field
			// (e.g. only priority changed) — nothing to record.
			continue
		}
		if _, seen := firstChangeFrom[ar.itemID]; !seen {
			firstChangeFrom[ar.itemID] = from
		}

		// Deterministic id: one activity row maps to at most one done-field
		// transition, so the activity id (prefixed to stay visibly
		// backfill-sourced) is a stable, collision-free primary key. Re-runs
		// hit the conflict clause and no-op.
		insert("bf_"+ar.activityID, ar.itemID, ar.workspaceID, ar.collectionID, key, from, to, ar.createdAt)
	}

	// --- Pass 2: create-time "entered initial status" rows ---
	// Every item entered an initial done-field value at creation; without a
	// row for it, an item created directly in a terminal value (e.g. a
	// retroactively-logged "done" task, or an import) would never count as a
	// completion. Seed one create-row per item at items.created_at.
	itemRows, err := s.db.Query(s.q(`
		SELECT id, workspace_id, collection_id, fields, created_at
		FROM items
		WHERE deleted_at IS NULL
	`))
	if err != nil {
		return nil, fmt.Errorf("scan items for create-seed backfill: %w", err)
	}
	defer itemRows.Close()

	type itemRow struct {
		id, workspaceID, collectionID, fields, createdAt string
	}
	var items []itemRow
	for itemRows.Next() {
		var ir itemRow
		if err := itemRows.Scan(&ir.id, &ir.workspaceID, &ir.collectionID, &ir.fields, &ir.createdAt); err != nil {
			return nil, fmt.Errorf("scan create-seed row: %w", err)
		}
		items = append(items, ir)
	}
	if err := itemRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate create-seed rows: %w", err)
	}
	itemRows.Close()

	for _, ir := range items {
		key := doneKey(ir.collectionID)
		// Initial value: the `from` of the item's earliest recorded change if
		// we saw one (that's the value it held before changing); otherwise the
		// item never changed its done field, so the current value IS the
		// initial value.
		initial, changed := firstChangeFrom[ir.id]
		if !changed {
			initial = extractFieldValue(ir.fields, key)
		}
		if initial == "" {
			// No initial done-field value to record (unset at creation).
			continue
		}
		insert("create_"+ir.id, ir.id, ir.workspaceID, ir.collectionID, key, "", initial, ir.createdAt)
	}

	return result, nil
}
