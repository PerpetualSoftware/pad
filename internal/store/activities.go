package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// ActivityDebounceCooldown is the window during which repeated "updated" actions
// on the same item by the same user are coalesced into a single activity entry.
const ActivityDebounceCooldown = 5 * time.Minute

func (s *Store) CreateActivity(a models.Activity) (string, error) {
	a.ID = newID()
	if a.Metadata == "" {
		a.Metadata = "{}"
	}
	ts := now()

	_, err := s.db.Exec(s.q(`
		INSERT INTO activities (id, workspace_id, document_id, action, actor, source, metadata, user_id, ip_address, user_agent, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`), a.ID, nilIfEmpty(a.WorkspaceID), nilIfEmpty(a.DocumentID), a.Action, a.Actor, a.Source, a.Metadata, nilIfEmpty(a.UserID), nilIfEmpty(a.IPAddress), nilIfEmpty(a.UserAgent), ts)
	return a.ID, err
}

// CreateActivityDebounced creates a new activity or updates an existing one if a
// matching activity (same document, same user, same action) was recorded within
// the cooldown window. Only "updated" actions are debounced; all other actions
// always create a new entry.
//
// When merging, the existing activity's timestamp is bumped to now and its
// metadata changes are accumulated (so a rapid sequence of field edits produces
// a single activity whose metadata lists all changed fields).
func (s *Store) CreateActivityDebounced(a models.Activity) (string, error) {
	if a.Metadata == "" {
		a.Metadata = "{}"
	}

	// Only debounce "updated" actions — everything else always creates a new row.
	if a.Action != "updated" {
		return s.CreateActivity(a)
	}

	ts := now()
	cutoff := time.Now().UTC().Add(-ActivityDebounceCooldown).Format(time.RFC3339)

	// Look for a recent activity to coalesce with.
	var existingID, existingMeta string
	err := s.db.QueryRow(s.q(`
		SELECT id, metadata FROM activities
		WHERE document_id = ? AND action = ? AND created_at >= ?
			AND ((user_id IS NOT NULL AND user_id = ?) OR (user_id IS NULL AND ? = ''))
		ORDER BY created_at DESC LIMIT 1
	`), a.DocumentID, a.Action, cutoff, a.UserID, a.UserID).Scan(&existingID, &existingMeta)

	if err == sql.ErrNoRows {
		// No recent match — create a new activity.
		return s.CreateActivity(a)
	}
	if err != nil {
		// Query error — fall back to creating a new activity.
		return s.CreateActivity(a)
	}

	// Merge metadata: accumulate "changes" strings from both old and new.
	merged := mergeActivityMeta(existingMeta, a.Metadata)

	_, err = s.db.Exec(s.q(`
		UPDATE activities SET metadata = ?, created_at = ? WHERE id = ?
	`), merged, ts, existingID)
	return existingID, err
}

// mergeActivityMeta combines two activity metadata JSON strings, accumulating
// the "changes" field. If both contain changes, they are joined with "; ".
// Other metadata fields (like "agent") are preserved from both sides.
func mergeActivityMeta(existing, incoming string) string {
	var existMap, newMap map[string]interface{}
	if err := json.Unmarshal([]byte(existing), &existMap); err != nil {
		existMap = make(map[string]interface{})
	}
	if err := json.Unmarshal([]byte(incoming), &newMap); err != nil {
		newMap = make(map[string]interface{})
	}

	// Accumulate changes
	existChanges, _ := existMap["changes"].(string)
	newChanges, _ := newMap["changes"].(string)

	// Start with existing map as base, overlay new fields
	for k, v := range newMap {
		if k == "changes" {
			continue // handled separately
		}
		existMap[k] = v
	}

	if newChanges != "" {
		if existChanges != "" {
			existMap["changes"] = collapseChanges(existChanges + "; " + newChanges)
		} else {
			existMap["changes"] = collapseChanges(newChanges)
		}
	}
	// If neither had changes, don't add an empty one.
	// If only existing had changes, it's already in existMap.

	result, err := json.Marshal(existMap)
	if err != nil {
		return existing
	}
	return string(result)
}

// collapseChanges takes a "; "-delimited list of "field: old → new" entries
// and collapses runs of consecutive same-field entries into a single
// "field: first-old → last-new" entry. Net no-ops (first-old == last-new
// after collapse) are dropped entirely.
//
// Why: CreateActivityDebounced accumulates changes across PATCHes within
// the 5-minute cooldown. Before BUG-1466's web-side typing debounce,
// rapid typing in a structured text field produced 30+ chained
// per-keystroke entries on a single activity row (visible on BUG-1419's
// timeline). Even with the debounce in place, two edits to the same
// field in the same window still read more naturally as one transition.
//
// Parsing assumptions:
//   - Entries are joined by "; " — diffFields uses the same separator so
//     a single split-by-"; " produces a flat list regardless of whether
//     the changes string came from one multi-field PATCH or several.
//   - Each entry has the shape "<field>: <from> → <to>" with " → " (U+2192,
//     padded) as the arrow. Unparseable entries are preserved verbatim
//     (they never match a field name, so they never collapse).
//   - Collapse is run-based, not global: `a:1→2; b:x→y; a:2→3` stays as-is
//     (interleaved field edits keep their chronology). Easier to reason
//     about and more accurate as a history.
func collapseChanges(s string) string {
	if s == "" {
		return ""
	}
	const arrow = " → "
	type entry struct {
		field, from, to string
		raw             string // preserved verbatim for unparseable entries
		// mergedCount tracks how many input segments were collapsed into
		// this entry. mergedCount > 1 means at least one merge happened.
		mergedCount int
		// hadTransition is true iff any input entry in the run produced a
		// display-string transition — i.e. either the initial entry had
		// from != to, or a subsequent entry's `to` differed from the
		// run's anchored `from`. Used together with hasLossySummary
		// below to decide whether a from==to result is a true net no-op.
		hadTransition bool
		// hasLossySummary is true iff ANY entry in the run carried a
		// `(N items)` / `(object)` / `(1 note)` display value — the
		// lossy summary form that formatChangeValue emits for
		// structured fields (handlers_documents.go::formatChangeValue).
		//
		// When the display value is lossy, a from==to result CAN'T be
		// trusted as "no underlying change." Counter-example: editing
		// implementation_notes from 1 note → 2 notes → 1 different
		// note collapses to `(1 note) → (1 note)` with hadTransition=true,
		// but the final note differs from the original — diffFields
		// emitted both entries because reflect.DeepEqual saw real
		// changes (see TestDiffFieldsSameCardinalityArrayChangeStillReported).
		// We can't recover the raw values from the merged string, so
		// we never drop a run that touched a lossy summary. Per Codex
		// review round 3 [P2].
		hasLossySummary bool
	}

	// isLossySummary detects formatChangeValue's display labels:
	// `(N items)`, `(1 note)`, `(2 entries)`, `(object)`, etc. The
	// shape — parenthesised, never produced for primitive values — is
	// stable across formatChangeValue's known and fallback branches.
	isLossySummary := func(v string) bool {
		return len(v) >= 2 && strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")")
	}
	parts := strings.Split(s, "; ")
	entries := make([]entry, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		colonIdx := strings.Index(p, ":")
		if colonIdx == -1 {
			entries = append(entries, entry{raw: p})
			continue
		}
		field := strings.TrimSpace(p[:colonIdx])
		// NB: don't TrimSpace the value-part — diffFields formats it as
		// " <from> → <to>" with a single leading space (the ": " format
		// separator). Trimming that leading space breaks arrow detection
		// when `from` is empty ("→ x" → no match for " → ").
		valuePart := p[colonIdx+1:]
		arrowIdx := strings.Index(valuePart, arrow)
		if arrowIdx == -1 {
			// Deletion-to-empty case: diffFields renders this as
			// "<key>: <from> → " which loses its trailing space when
			// the segment is TrimSpace'd. Catch it via suffix match.
			if strings.HasSuffix(valuePart, " →") {
				from := strings.TrimSpace(valuePart[:len(valuePart)-len(" →")])
				entries = append(entries, entry{
					field: field, from: from, to: "",
					mergedCount: 1, hadTransition: from != "",
					hasLossySummary: isLossySummary(from),
				})
				continue
			}
			entries = append(entries, entry{raw: p})
			continue
		}
		from := strings.TrimSpace(valuePart[:arrowIdx])
		to := strings.TrimSpace(valuePart[arrowIdx+len(arrow):])
		entries = append(entries, entry{
			field: field, from: from, to: to,
			mergedCount: 1, hadTransition: from != to,
			hasLossySummary: isLossySummary(from) || isLossySummary(to),
		})
	}

	// Walk consecutively, extending runs of the same field.
	collapsed := make([]entry, 0, len(entries))
	for _, e := range entries {
		if e.raw != "" {
			collapsed = append(collapsed, e)
			continue
		}
		if len(collapsed) > 0 {
			prev := &collapsed[len(collapsed)-1]
			if prev.raw == "" && prev.field == e.field {
				if e.hadTransition || e.to != prev.from {
					prev.hadTransition = true
				}
				// hasLossySummary is sticky across the run: once any
				// entry brings a lossy display value into the chain,
				// the merged result can never be safely treated as a
				// raw-value no-op.
				if e.hasLossySummary {
					prev.hasLossySummary = true
				}
				prev.to = e.to
				prev.mergedCount += e.mergedCount
				continue
			}
		}
		collapsed = append(collapsed, e)
	}

	// Drop only true net no-ops, defined as: a collapsed run
	// (mergedCount > 1) whose from==to, whose intermediate `to` values
	// transitioned away from the anchor at some point (hadTransition),
	// AND whose display values are NOT lossy summaries (!hasLossySummary).
	//
	// Why each clause:
	//   - mergedCount > 1: a single entry is preserved because diffFields
	//     wouldn't emit it unless reflect.DeepEqual detected a real
	//     change. Only collapsed runs can synthesise a fake from==to.
	//   - hadTransition: distinguishes a real cancellation from a
	//     repeated same-display run (Codex round 2 [P2]).
	//   - !hasLossySummary: a `(1 note) → (2 notes) → (1 note)` run
	//     visibly transitions and visibly returns, but the underlying
	//     notes can be entirely different objects — the display labels
	//     are lossy. Never drop those (Codex round 3 [P2]).
	kept := collapsed[:0]
	for _, e := range collapsed {
		if e.raw == "" && e.mergedCount > 1 && e.from == e.to && e.hadTransition && !e.hasLossySummary {
			continue
		}
		kept = append(kept, e)
	}

	var sb strings.Builder
	for i, e := range kept {
		if i > 0 {
			sb.WriteString("; ")
		}
		if e.raw != "" {
			sb.WriteString(e.raw)
			continue
		}
		// Match diffFields's format: `"<field>: → <to>"` when from is
		// empty (one space between ":" and "→"), `"<field>: <from> → <to>"`
		// otherwise. arrow already carries its own leading space.
		sb.WriteString(e.field)
		if e.from == "" {
			sb.WriteString(":")
		} else {
			sb.WriteString(": ")
			sb.WriteString(e.from)
		}
		sb.WriteString(arrow)
		sb.WriteString(e.to)
	}
	return sb.String()
}

func (s *Store) ListWorkspaceActivity(workspaceID string, params models.ActivityListParams) ([]models.Activity, error) {
	query := `
		SELECT a.id, COALESCE(a.workspace_id, ''), COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, ''), COALESCE(a.ip_address, ''), COALESCE(a.user_agent, '')
		FROM activities a
		LEFT JOIN users u ON a.user_id = u.id
		WHERE a.workspace_id = ?
	`
	args := []interface{}{workspaceID}

	if params.Action != "" {
		query += " AND a.action = ?"
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += " AND a.actor = ?"
		args = append(args, params.Actor)
	}
	if params.Source != "" {
		query += " AND a.source = ?"
		args = append(args, params.Source)
	}

	query += " ORDER BY a.created_at DESC"

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	query += fmt.Sprintf(" LIMIT %d", limit)
	if params.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", params.Offset)
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}

func (s *Store) ListDocumentActivity(documentID string, params models.ActivityListParams) ([]models.Activity, error) {
	query := `
		SELECT a.id, COALESCE(a.workspace_id, ''), COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, ''), COALESCE(a.ip_address, ''), COALESCE(a.user_agent, '')
		FROM activities a
		LEFT JOIN users u ON a.user_id = u.id
		WHERE a.document_id = ?
	`
	args := []interface{}{documentID}

	if params.Action != "" {
		query += " AND a.action = ?"
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += " AND a.actor = ?"
		args = append(args, params.Actor)
	}

	query += " ORDER BY a.created_at DESC"

	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	query += fmt.Sprintf(" LIMIT %d", limit)
	if params.Offset > 0 {
		query += fmt.Sprintf(" OFFSET %d", params.Offset)
	}

	rows, err := s.db.Query(s.q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}

// ListDocumentActivityBeforeTime returns activities for a document created before the given time,
// ordered newest-first, limited to `limit` results. Used for cursor-based timeline pagination.
//
// When beforeID is empty (first page / no cursor), the secondary id tie-breaker
// is omitted. See ListCommentsBeforeTime for the rationale (BUG-1086).
func (s *Store) ListDocumentActivityBeforeTime(documentID string, before time.Time, beforeID string, limit int) ([]models.Activity, error) {
	ts := before.Format(time.RFC3339)
	const selectCols = `a.id, COALESCE(a.workspace_id, ''), COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, ''), COALESCE(a.ip_address, ''), COALESCE(a.user_agent, '')`
	const orderLimit = `ORDER BY a.created_at DESC, a.id DESC LIMIT ?`

	var rows *sql.Rows
	var err error
	if beforeID == "" {
		rows, err = s.db.Query(s.q(`
			SELECT `+selectCols+`
			FROM activities a
			LEFT JOIN users u ON a.user_id = u.id
			WHERE a.document_id = ? AND a.created_at < ?
			`+orderLimit), documentID, ts, limit)
	} else {
		rows, err = s.db.Query(s.q(`
			SELECT `+selectCols+`
			FROM activities a
			LEFT JOIN users u ON a.user_id = u.id
			WHERE a.document_id = ? AND (a.created_at < ? OR (a.created_at = ? AND a.id < ?))
			`+orderLimit), documentID, ts, ts, beforeID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}

func scanActivitiesWithUser(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
	Err() error
}) ([]models.Activity, error) {
	var activities []models.Activity
	for rows.Next() {
		var a models.Activity
		var createdAt string
		if err := rows.Scan(&a.ID, &a.WorkspaceID, &a.DocumentID, &a.Action, &a.Actor, &a.Source, &a.Metadata, &a.UserID, &createdAt, &a.ActorName, &a.IPAddress, &a.UserAgent); err != nil {
			return nil, err
		}
		a.CreatedAt = parseTime(createdAt)
		activities = append(activities, a)
	}
	return activities, rows.Err()
}

func nilIfEmpty(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// ListAuditLog returns activities matching the given audit log filters.
// Supports filtering by action, actor, workspace, and date range.
func (s *Store) ListAuditLog(params models.AuditLogParams) ([]models.Activity, error) {
	// Build the full query with ? placeholders first, then rebind once at
	// the end so PostgreSQL $1/$2/... numbering is correct across all filters.
	query := `
		SELECT a.id, COALESCE(a.workspace_id, ''), COALESCE(a.document_id, ''), a.action, a.actor, a.source, a.metadata, COALESCE(a.user_id, ''), a.created_at, COALESCE(u.name, ''), COALESCE(a.ip_address, ''), COALESCE(a.user_agent, '')
		FROM activities a
		LEFT JOIN users u ON a.user_id = u.id
		WHERE 1=1
	`
	args := []interface{}{}

	if params.WorkspaceID != "" {
		query += ` AND a.workspace_id = ?`
		args = append(args, params.WorkspaceID)
	}
	if params.Action != "" {
		query += ` AND a.action = ?`
		args = append(args, params.Action)
	}
	if params.Actor != "" {
		query += ` AND a.user_id = ?`
		args = append(args, params.Actor)
	}
	if params.Days > 0 {
		cutoff := time.Now().UTC().AddDate(0, 0, -params.Days).Format(time.RFC3339)
		query += ` AND a.created_at >= ?`
		args = append(args, cutoff)
	}

	query += ` ORDER BY a.created_at DESC`

	limit := params.Limit
	if limit <= 0 {
		limit = 50
	}
	query += fmt.Sprintf(` LIMIT %d`, limit)
	if params.Offset > 0 {
		query += fmt.Sprintf(` OFFSET %d`, params.Offset)
	}

	// Rebind all ? placeholders in one pass so $1, $2, ... are sequential.
	query = s.q(query)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanActivitiesWithUser(rows)
}
