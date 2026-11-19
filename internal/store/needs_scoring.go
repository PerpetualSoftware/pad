package store

import (
	"fmt"
)

// Batch reads behind the importance and simplicity scorers on GET /me/needs
// (TASK-3959, IDEA-3906 B4b). All are read-only, take the ids the router
// already admitted, and are chunked like the router's other batch reads.

// dedupeIDs drops repeated ids, keeping first-seen order, so a repeat landing
// in a later chunk cannot append the same row twice.
func dedupeIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// NeedsScoringItem is what the scorers read of one item: where it lives, its
// fields (priority, dates, status), and the length of its body in characters.
type NeedsScoringItem struct {
	ID           string
	CollectionID string
	Fields       string
	ContentRunes int
}

// NeedsScoringItems returns the scoring facts of the live items among ids in
// the workspace. length() counts characters on SQLite and Postgres alike; a
// NULL content (the column is nullable) counts as empty.
func (s *Store) NeedsScoringItems(workspaceID string, ids []string) (map[string]NeedsScoringItem, error) {
	out := map[string]NeedsScoringItem{}
	for _, chunk := range chunkStrings(dedupeIDs(ids), needsChunk) {
		args := []any{workspaceID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT i.id, i.collection_id, i.fields, COALESCE(length(i.content), 0)
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
