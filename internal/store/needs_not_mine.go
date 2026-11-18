package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// "Not mine" (TASK-4643): one person's sticky dismissal of one item from
// their needs-you inbox. See migration 186 for the release rule; the inbox
// evaluates it at read time from the rows and transitions read here.

// ErrInvalidNeedsSource reports a not-mine write with a source the store does
// not know. The handler derives source from the request's authentication, so
// this is a programming error, never a client one.
var ErrInvalidNeedsSource = errors.New("invalid needs source")

// lockNeedsHide serializes the two hide-state writes (snooze and not-mine)
// for one (user, item) on Postgres, so each write's delete of the other sees
// a concurrent one's insert. SQLite has one writer already.
func (s *Store) lockNeedsHide(tx *sql.Tx, userID, itemID string) error {
	if s.dialect.Driver() != DriverPostgres {
		return nil
	}
	if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('pad:needs-hide:' || $1 || ':' || $2))`, userID, itemID); err != nil {
		return fmt.Errorf("lock needs hide state: %w", err)
	}
	return nil
}

// needsNotMineState is the item's state as a not-mine made now would record
// it: the dismissal marker plus now, the item's collection, and userID when
// the item is assigned to them. Another assignee is stored as "": the release
// rule asks only whether it was the caller, and a row never names anyone else.
func (s *Store) needsNotMineState(tx *sql.Tx, userID, itemID string, now time.Time) (models.NeedsSnoozeMarker, error) {
	cur, err := s.needsItemState(tx, userID, itemID)
	if err != nil {
		return cur, err
	}
	var assignee sql.NullString
	if err := tx.QueryRow(s.q(`SELECT collection_id, assigned_user_id FROM items WHERE id = ?`), itemID).
		Scan(&cur.SeenCollectionID, &assignee); err != nil {
		return cur, fmt.Errorf("read item for not-mine: %w", err)
	}
	if assignee.Valid && assignee.String == userID {
		cur.SeenAssigneeID = userID
	}
	cur.SeenAt = now.UTC().Truncate(time.Second).Format(time.RFC3339)
	return cur, nil
}

// clampNotMineMarker is clampNeedsMarker plus the not-mine fields: SeenAt is
// a parsed time held at or before now (unparseable is "", seen nothing), and
// a collection or assignee that is not the item's current one is "". Each
// can only make the item come back sooner.
func (s *Store) clampNotMineMarker(tx *sql.Tx, userID, itemID string, m, cur models.NeedsSnoozeMarker) (models.NeedsSnoozeMarker, error) {
	out, err := s.clampNeedsMarker(tx, userID, itemID, m, cur)
	if err != nil {
		return out, err
	}
	if t, err := time.Parse(time.RFC3339, m.SeenAt); err == nil {
		t = t.UTC().Truncate(time.Second)
		out.SeenAt = t.Format(time.RFC3339)
		if now, _ := time.Parse(time.RFC3339, cur.SeenAt); t.After(now) {
			out.SeenAt = cur.SeenAt
		}
	}
	if m.SeenCollectionID != "" && m.SeenCollectionID == cur.SeenCollectionID {
		out.SeenCollectionID = cur.SeenCollectionID
	}
	if m.SeenAssigneeID != "" && m.SeenAssigneeID == cur.SeenAssigneeID {
		out.SeenAssigneeID = cur.SeenAssigneeID
	}
	return out, nil
}

// MarkNeedsNotMine records userID's "not mine" on an item, replacing any
// earlier one and deleting any snooze of theirs on it in the same
// transaction. marker is what the client had seen (nil: the item's current
// state) and is clamped to the server's values. source is how the request
// authenticated (models.NeedsSourceWeb or models.NeedsSourceCLI). The caller
// has already checked the user may see the item.
func (s *Store) MarkNeedsNotMine(userID, workspaceID, itemID, source string, marker *models.NeedsSnoozeMarker) (*models.NeedsNotMine, error) {
	if source != models.NeedsSourceWeb && source != models.NeedsSourceCLI {
		return nil, fmt.Errorf("%w: %q", ErrInvalidNeedsSource, source)
	}
	nowT := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.lockNeedsHide(tx, userID, itemID); err != nil {
		return nil, err
	}
	seen, err := s.needsNotMineState(tx, userID, itemID, nowT)
	if err != nil {
		return nil, err
	}
	if marker != nil {
		if seen, err = s.clampNotMineMarker(tx, userID, itemID, *marker, seen); err != nil {
			return nil, err
		}
	}
	ids, err := json.Marshal(seen.SeenCommentIDs)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(s.q(`
		INSERT INTO item_needs_not_mine
			(user_id, item_id, workspace_id, seen_comment_at, seen_comment_ids, seen_transition_seq,
			 seen_collection_id, seen_assignee_id, seen_at, source, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, item_id) DO UPDATE SET
			workspace_id        = excluded.workspace_id,
			seen_comment_at     = excluded.seen_comment_at,
			seen_comment_ids    = excluded.seen_comment_ids,
			seen_transition_seq = excluded.seen_transition_seq,
			seen_collection_id  = excluded.seen_collection_id,
			seen_assignee_id    = excluded.seen_assignee_id,
			seen_at             = excluded.seen_at,
			source              = excluded.source,
			created_at          = excluded.created_at`),
		userID, itemID, workspaceID, seen.SeenCommentAt, string(ids), seen.SeenTransitionSeq,
		seen.SeenCollectionID, seen.SeenAssigneeID, seen.SeenAt, source, nowT.Format(time.RFC3339)); err != nil {
		return nil, fmt.Errorf("mark needs item not mine: %w", err)
	}
	if _, err := tx.Exec(s.q(`DELETE FROM item_needs_snoozes WHERE user_id = ? AND item_id = ?`), userID, itemID); err != nil {
		return nil, fmt.Errorf("clear needs snooze: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetNeedsNotMine(userID, itemID)
}

// UnmarkNeedsNotMine removes userID's "not mine" on an item. It reports
// whether there was one.
func (s *Store) UnmarkNeedsNotMine(userID, itemID string) (bool, error) {
	res, err := s.db.Exec(s.q(`DELETE FROM item_needs_not_mine WHERE user_id = ? AND item_id = ?`), userID, itemID)
	if err != nil {
		return false, fmt.Errorf("unmark needs item not mine: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const needsNotMineCols = `n.item_id, n.workspace_id, n.seen_comment_at, n.seen_comment_ids, n.seen_transition_seq,
	n.seen_collection_id, n.seen_assignee_id, n.seen_at, n.source, n.created_at`

func scanNeedsNotMine(sc interface{ Scan(...any) error }) (*models.NeedsNotMine, error) {
	nm := &models.NeedsNotMine{}
	var ids string
	if err := sc.Scan(&nm.ItemID, &nm.WorkspaceID, &nm.Marker.SeenCommentAt, &ids, &nm.Marker.SeenTransitionSeq,
		&nm.Marker.SeenCollectionID, &nm.Marker.SeenAssigneeID, &nm.Marker.SeenAt, &nm.Source, &nm.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(ids), &nm.Marker.SeenCommentIDs); err != nil {
		return nil, fmt.Errorf("needs not-mine seen_comment_ids: %w", err)
	}
	return nm, nil
}

// GetNeedsNotMine returns userID's "not mine" on an item, or nil when there
// is none. Whether it still holds is the inbox's call (it needs the routing
// facts), so this reports the row alone.
func (s *Store) GetNeedsNotMine(userID, itemID string) (*models.NeedsNotMine, error) {
	nm, err := scanNeedsNotMine(s.db.QueryRow(s.q(`SELECT `+needsNotMineCols+`
		FROM item_needs_not_mine n WHERE n.user_id = ? AND n.item_id = ?`), userID, itemID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get needs not-mine: %w", err)
	}
	return nm, nil
}

// NeedsTransition is one status transition recorded after a not-mine's
// marker: the field it changed and the value it moved to.
type NeedsTransition struct {
	FieldKey string
	ToStatus string
}

// NeedsNotMineRead is one not-mine row with the transitions recorded on its
// item since its marker's seq.
type NeedsNotMineRead struct {
	NotMine     *models.NeedsNotMine
	Transitions []NeedsTransition
}

// NeedsNotMineForItems returns userID's not-mine rows on the given items of
// one workspace, each with the transitions after its marker, keyed by item
// id. It is scoped like NeedsSnoozesForItems: joined to the live item and
// scoped by items.workspace_id (a cross-workspace move leaves the row on the
// archived source), over ids the caller already filtered by visibility. One
// batch read by primary key, and one transition join limited to the
// not-mine items found.
func (s *Store) NeedsNotMineForItems(userID, workspaceID string, itemIDs []string) (map[string]*NeedsNotMineRead, error) {
	out := map[string]*NeedsNotMineRead{}
	for _, chunk := range chunkStrings(itemIDs, needsChunk) {
		args := []any{userID, workspaceID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT `+needsNotMineCols+`
			FROM item_needs_not_mine n JOIN items i ON i.id = n.item_id
			WHERE n.user_id = ? AND i.workspace_id = ? AND i.deleted_at IS NULL
			  AND n.item_id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("needs not-mine: %w", err)
		}
		for rows.Next() {
			nm, err := scanNeedsNotMine(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[nm.ItemID] = &NeedsNotMineRead{NotMine: nm}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(out) == 0 {
		return out, nil
	}
	found := make([]string, 0, len(out))
	for _, id := range itemIDs {
		if out[id] != nil {
			found = append(found, id)
		}
	}
	for _, chunk := range chunkStrings(found, needsChunk) {
		args := []any{userID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT DISTINCT t.item_id, t.field_key, t.to_status
			FROM status_transitions t
			JOIN item_needs_not_mine n ON n.item_id = t.item_id AND n.user_id = ?
			WHERE t.seq > n.seen_transition_seq
			  AND t.item_id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("needs not-mine transitions: %w", err)
		}
		for rows.Next() {
			var id string
			var tr NeedsTransition
			if err := rows.Scan(&id, &tr.FieldKey, &tr.ToStatus); err != nil {
				rows.Close()
				return nil, err
			}
			if r := out[id]; r != nil {
				r.Transitions = append(r.Transitions, tr)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
