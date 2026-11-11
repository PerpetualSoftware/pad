package store

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// AgendaWorkDetails loads schedule facts and gate presence in two queries,
// regardless of candidate count. Gate evaluation remains per gated item;
// AgendaOutstanding still resolves roles/overrides per scheduled candidate.
func (s *Store) AgendaWorkDetails(workspaceID string, itemIDs []string) (map[string]*models.ItemSchedule, map[string]bool, error) {
	return s.agendaWorkDetailsQ(s.db, workspaceID, itemIDs)
}

func (s *Store) agendaWorkDetailsQ(q Queryer, workspaceID string, itemIDs []string) (map[string]*models.ItemSchedule, map[string]bool, error) {
	schedules := make(map[string]*models.ItemSchedule)
	if len(itemIDs) == 0 {
		return schedules, map[string]bool{}, nil
	}
	args := []any{workspaceID}
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := q.Query(s.q(`SELECT `+scheduleColumns+` FROM item_schedules WHERE workspace_id=? AND item_id IN (`+placeholders(len(itemIDs))+`)`), args...)
	if err != nil {
		return nil, nil, fmt.Errorf("batch agenda schedules: %w", err)
	}
	for rows.Next() {
		sc, err := scanSchedule(rows)
		if err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("scan batch agenda schedule: %w", err)
		}
		schedules[sc.ItemID] = sc
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	gated, err := s.agendaGatedItemIDsQ(q, workspaceID, itemIDs)
	return schedules, gated, err
}

// AgendaGatedItemIDs identifies the candidates requiring gate evaluation.
func (s *Store) AgendaGatedItemIDs(workspaceID string, itemIDs []string) (map[string]bool, error) {
	return s.agendaGatedItemIDsQ(s.db, workspaceID, itemIDs)
}

func (s *Store) agendaGatedItemIDsQ(q Queryer, workspaceID string, itemIDs []string) (map[string]bool, error) {
	gated := make(map[string]bool)
	if len(itemIDs) == 0 {
		return gated, nil
	}
	args := []any{workspaceID}
	for _, id := range itemIDs {
		args = append(args, id)
	}
	rows, err := q.Query(s.q(`SELECT DISTINCT blocked_item_id FROM schedule_gates WHERE workspace_id=? AND blocked_item_id IN (`+placeholders(len(itemIDs))+`)`), args...)
	if err != nil {
		return nil, fmt.Errorf("batch agenda gate presence: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		gated[id] = true
	}
	return gated, rows.Err()
}
