package store

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Batch reads behind the importance and simplicity scorers on GET /me/needs
// (TASK-3959, IDEA-3906 B4b). All are read-only, take the ids the router
// already admitted, and are chunked like the router's other batch reads.

// NeedsScoringItem is what the scorers read of one item: where it lives, its
// fields (priority, dates, status), and the length of its body in characters.
type NeedsScoringItem struct {
	ID           string
	CollectionID string
	Fields       string
	ContentRunes int
}

// NeedsScoringItems returns the scoring facts of the live items among ids in
// the workspace. length() counts characters on SQLite and Postgres alike.
func (s *Store) NeedsScoringItems(workspaceID string, ids []string) (map[string]NeedsScoringItem, error) {
	out := map[string]NeedsScoringItem{}
	for _, chunk := range chunkStrings(ids, needsChunk) {
		args := []any{workspaceID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT i.id, i.collection_id, i.fields, length(i.content)
			FROM items i
			WHERE i.workspace_id = ? AND i.deleted_at IS NULL AND i.id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("needs scoring items: %w", err)
		}
		for rows.Next() {
			var it NeedsScoringItem
			if err := rows.Scan(&it.ID, &it.CollectionID, &it.Fields, &it.ContentRunes); err != nil {
				rows.Close()
				return nil, err
			}
			out[it.ID] = it
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// LatestDecisionsForItems returns, per item, the newest answer for each
// question key of one set: LatestWorkspaceDecisions restricted to the given
// items, so a caller that holds a list of ids does not read the whole
// workspace. Soft-deleted items are left out.
func (s *Store) LatestDecisionsForItems(ids []string, questionSet string) (map[string][]models.ItemDecision, error) {
	out := map[string][]models.ItemDecision{}
	for _, chunk := range chunkStrings(ids, needsChunk) {
		args := []any{questionSet}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT d.id, d.item_id, d.question_set, d.question_key, d.kind, d.answer, d.confidence,
			       d.provider, d.model, d.state_hash, d.question_hash, d.item_seq, d.state_truncated, d.evaluated_at
			FROM item_decisions d
			JOIN items i ON i.id = d.item_id AND i.deleted_at IS NULL
			WHERE d.question_set = ? AND d.item_id IN (`+placeholders(len(chunk))+`)
			  AND NOT EXISTS (
				SELECT 1 FROM item_decisions n
				WHERE n.item_id = d.item_id AND n.question_set = d.question_set AND n.question_key = d.question_key
				  AND (n.evaluated_at > d.evaluated_at OR (n.evaluated_at = d.evaluated_at AND n.id > d.id))
			  )
			ORDER BY d.item_id, d.question_key`), args...)
		if err != nil {
			return nil, fmt.Errorf("latest decisions for items: %w", err)
		}
		ds, err := scanItemDecisions(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		for _, d := range ds {
			out[d.ItemID] = append(out[d.ItemID], d)
		}
	}
	return out, nil
}

// OwedDecisionJobs returns the set of items among ids that have a job owed
// for the question set and not failed: the same "owed" the dashboard flags
// use (a job carrying a recorded failure is not coming).
func (s *Store) OwedDecisionJobs(ids []string, questionSet string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, chunk := range chunkStrings(ids, needsChunk) {
		args := []any{questionSet}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT item_id FROM decision_jobs
			WHERE question_set = ? AND (last_error IS NULL OR last_error = '')
			  AND item_id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("owed decision jobs: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
