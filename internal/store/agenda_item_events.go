package store

import (
	"database/sql"
	"fmt"
	"time"
)

// AgendaItemEvent is a live item with its creation or latest completion instant.
type AgendaItemEvent struct {
	ItemID     string
	ItemRef    string
	ItemTitle  string
	Collection string
	At         time.Time
}

// agendaEventBound rounds a query boundary up to the next stored second.
// Item creations and status transitions store whole seconds; rounding both
// bounds up preserves the half-open interval for fractional RFC 3339 inputs.
func agendaEventBound(at time.Time) string {
	if at.Nanosecond() != 0 {
		at = at.Truncate(time.Second).Add(time.Second)
	}
	return at.UTC().Format(time.RFC3339)
}

// ListCreatedForAgenda reads live, non-system items created in [from, to).
// Scope is applied before the time/id ordering and SQL row limit.
func (s *Store) ListCreatedForAgenda(workspaceID string, scope PendingReminderScope, from, to time.Time, limit int) ([]AgendaItemEvent, error) {
	if scope.CollectionIDs != nil && len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
		return nil, nil
	}
	clause, args := scopeClause("i", scope)
	params := []any{workspaceID, agendaEventBound(from), agendaEventBound(to)}
	params = append(append(params, args...), limit)
	rows, err := s.db.Query(s.q(`SELECT i.id, i.slug, i.item_number, i.title, c.slug, c.prefix, i.created_at AS event_at
  FROM items i JOIN collections c ON c.id = i.collection_id
  WHERE i.workspace_id = ? AND i.created_at >= ? AND i.created_at < ?
   AND i.deleted_at IS NULL AND c.deleted_at IS NULL AND NOT c.is_system`+clause+`
  ORDER BY event_at, i.id LIMIT ?`), params...)
	if err != nil {
		return nil, fmt.Errorf("agenda created items: %w", err)
	}
	return scanAgendaItemEvents(rows)
}

// ListCompletedForAgenda returns one entry per item, at its latest positive
// terminal transition in [from, to). Current item visibility is applied before
// the limit, so a completion cannot disclose an item moved out of scope.
func (s *Store) ListCompletedForAgenda(workspaceID string, scope PendingReminderScope, from, to time.Time, limit int) ([]AgendaItemEvent, error) {
	if scope.CollectionIDs != nil && len(scope.CollectionIDs) == 0 && len(scope.ItemIDs) == 0 {
		return nil, nil
	}
	// The minimal read: ListCollections also counts items per collection, which
	// would make this call's cost depend on items the caller cannot see.
	all, err := s.ListCollectionsMinimal(workspaceID)
	if err != nil {
		return nil, err
	}
	var colls []reportCollection
	for _, c := range all {
		if !c.IsSystem {
			colls = append(colls, reportCollectionFor(c))
		}
	}
	positive, posArgs := s.positiveTerminalExpr(colls)
	if positive == "" {
		return nil, nil
	}
	clause, scopeArgs := scopeClause("i", scope)
	args := []any{workspaceID, workspaceID, agendaEventBound(from), agendaEventBound(to)}
	args = append(append(append(args, posArgs...), scopeArgs...), limit)
	rows, err := s.db.Query(s.q(`SELECT i.id, i.slug, i.item_number, i.title, c.slug, c.prefix, MAX(st.created_at) AS event_at
  FROM status_transitions st
  JOIN items i ON i.id = st.item_id
  JOIN collections c ON c.id = i.collection_id
  WHERE st.workspace_id = ? AND i.workspace_id = ? AND st.created_at >= ? AND st.created_at < ?
   AND i.deleted_at IS NULL AND c.deleted_at IS NULL AND NOT c.is_system
   AND `+positive+clause+`
  GROUP BY i.id, i.slug, i.item_number, i.title, c.slug, c.prefix
  ORDER BY event_at, i.id LIMIT ?`), args...)
	if err != nil {
		return nil, fmt.Errorf("agenda completed items: %w", err)
	}
	return scanAgendaItemEvents(rows)
}

func scanAgendaItemEvents(rows *sql.Rows) ([]AgendaItemEvent, error) {
	defer rows.Close()
	var out []AgendaItemEvent
	for rows.Next() {
		var e AgendaItemEvent
		var slug, prefix, at string
		var number *int64
		if err := rows.Scan(&e.ItemID, &slug, &number, &e.ItemTitle, &e.Collection, &prefix, &at); err != nil {
			return nil, err
		}
		e.ItemRef = slug
		if number != nil {
			e.ItemRef = fmt.Sprintf("%s-%d", prefix, *number)
		}
		e.At = parseTime(at)
		out = append(out, e)
	}
	return out, rows.Err()
}
