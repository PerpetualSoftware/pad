package store

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// extractStatusValue pulls the `status` field out of an item's fields JSON
// blob. Returns "" when the blob is empty, unparseable, or carries no string
// status — callers treat "" as "no status" (so an unset → set transition has
// an empty FromStatus, and a status-less collection never records a row).
func extractStatusValue(fieldsJSON string) string {
	if fieldsJSON == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &m); err != nil {
		return ""
	}
	if s, ok := m["status"].(string); ok {
		return s
	}
	return ""
}

// parseStatusChange extracts the from/to status out of a single
// activities.metadata `changes` string. The string is built by
// handlers_documents.go::diffFields as a "; "-joined list of
// "key: old → new" segments (and "key: → new" when the key was newly set).
// We locate the "status:" segment and split it on the UTF-8 arrow.
//
// Returns ok=false when there is no status segment. from may be "" (newly-set
// status); to is always non-empty when ok.
func parseStatusChange(changes string) (from, to string, ok bool) {
	for _, seg := range strings.Split(changes, ";") {
		seg = strings.TrimSpace(seg)
		rest, found := strings.CutPrefix(seg, "status:")
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

// BackfillStatusTransitions populates status_transitions from the historical
// activity log, parsing each "updated" activity's metadata.changes for a
// status hop. Designed to be called once from server startup after
// migrations run, mirroring BackfillWikiLinks.
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
// Because the activity feed is debounce-coalesced (mergeActivityMeta collapses
// same-field runs), historical transitions may undercount rapid hops — this is
// the limitation the structured table exists to fix going forward, and is
// documented on TASK-1637.
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

	// Join to items for the authoritative workspace_id + collection_id
	// (activities.workspace_id exists but collection_id does not). Only
	// "updated" activities carry a changes blob; the LIKE prunes the scan
	// to status-bearing rows. Order by created_at so multi-hop histories
	// insert in chronological order.
	rows, err := s.db.Query(s.q(`
		SELECT a.document_id, i.workspace_id, i.collection_id, a.metadata, a.created_at
		FROM activities a
		JOIN items i ON i.id = a.document_id
		WHERE a.action = 'updated'
		  AND a.metadata LIKE '%status:%'
		ORDER BY a.created_at
	`))
	if err != nil {
		return nil, fmt.Errorf("scan activities for status-transition backfill: %w", err)
	}
	defer rows.Close()

	// Buffer rows before writing — some drivers dislike interleaving
	// INSERTs with an open SELECT cursor (same caution as BackfillWikiLinks).
	type activityRow struct {
		itemID, workspaceID, collectionID, metadata, createdAt string
	}
	var scanned []activityRow
	for rows.Next() {
		var ar activityRow
		if err := rows.Scan(&ar.itemID, &ar.workspaceID, &ar.collectionID, &ar.metadata, &ar.createdAt); err != nil {
			return nil, fmt.Errorf("scan status-transition backfill row: %w", err)
		}
		scanned = append(scanned, ar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate status-transition backfill rows: %w", err)
	}
	rows.Close()

	for _, ar := range scanned {
		result.ActivitiesScanned++

		var meta struct {
			Changes string `json:"changes"`
		}
		if err := json.Unmarshal([]byte(ar.metadata), &meta); err != nil {
			result.Errors++
			continue
		}
		from, to, ok := parseStatusChange(meta.Changes)
		if !ok {
			// LIKE matched but no parseable status segment (e.g. the word
			// "status" appeared elsewhere in the blob) — skip silently.
			continue
		}

		if _, err := s.db.Exec(s.q(`
			INSERT INTO status_transitions (id, item_id, workspace_id, collection_id, from_status, to_status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`), newID(), ar.itemID, ar.workspaceID, ar.collectionID, from, to, ar.createdAt); err != nil {
			slog.Warn("status-transition backfill: insert failed",
				slog.String("item_id", ar.itemID), slog.String("err", err.Error()))
			result.Errors++
			continue
		}
		result.Inserted++
	}

	return result, nil
}
