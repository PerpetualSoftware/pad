package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Store reads behind the cross-workspace needs-you router (TASK-3920,
// IDEA-3906 half A unit A4). Every read here takes ids the caller already
// scoped to what the viewer may see; none of them decides visibility.

// needsChunk bounds an IN list.
const needsChunk = 500

// GuestWorkspaceIDs lists the live workspaces where userID holds a collection
// or item grant but is NOT a member: the guest workspaces. A member's grants
// do not make a workspace a guest one, and a soft-deleted workspace never
// appears. The result is sorted by id. BUG-3914 reuses it for search.
func (s *Store) GuestWorkspaceIDs(userID string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
		SELECT w.id FROM workspaces w
		WHERE w.deleted_at IS NULL
		  AND (EXISTS (SELECT 1 FROM collection_grants cg WHERE cg.workspace_id = w.id AND cg.user_id = ?)
		    OR EXISTS (SELECT 1 FROM item_grants ig WHERE ig.workspace_id = w.id AND ig.user_id = ?))
		  AND NOT EXISTS (SELECT 1 FROM workspace_members wm WHERE wm.workspace_id = w.id AND wm.user_id = ?)
		ORDER BY w.id`), userID, userID, userID)
	if err != nil {
		return nil, fmt.Errorf("guest workspace ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetWorkspacesByIDs loads live workspaces by id with their owner's username,
// in one query per chunk. Unknown and soft-deleted ids are skipped.
func (s *Store) GetWorkspacesByIDs(ids []string) ([]models.Workspace, error) {
	var out []models.Workspace
	for _, chunk := range chunkStrings(ids, needsChunk) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.Query(s.q(`
			SELECT w.id, w.name, w.slug, COALESCE(w.owner_id, ''), COALESCE(ou.username, ''), w.description, w.settings, w.source, w.created_at, w.updated_at
			FROM workspaces w LEFT JOIN users ou ON ou.id = w.owner_id
			WHERE w.deleted_at IS NULL AND w.id IN (`+placeholders(len(chunk))+`)
			ORDER BY w.id`), args...)
		if err != nil {
			return nil, fmt.Errorf("workspaces by ids: %w", err)
		}
		for rows.Next() {
			var ws models.Workspace
			var createdAt, updatedAt string
			if err := rows.Scan(&ws.ID, &ws.Name, &ws.Slug, &ws.OwnerID, &ws.OwnerUsername, &ws.Description, &ws.Settings, &ws.Source, &createdAt, &updatedAt); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan workspace: %w", err)
			}
			ws.CreatedAt = parseTime(createdAt)
			ws.UpdatedAt = parseTime(updatedAt)
			ws.HydrateDerivedFields()
			out = append(out, ws)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// WorkspaceOwnerIDs lists the workspace's human owners, ordered by id. Disabled
// accounts are included: eligibility belongs to the router.
func (s *Store) WorkspaceOwnerIDs(workspaceID string) ([]string, error) {
	rows, err := s.db.Query(s.q(`
		SELECT wm.user_id FROM workspace_members wm JOIN users u ON u.id = wm.user_id
		WHERE wm.workspace_id = ? AND wm.role = 'owner' AND u.kind = 'human'
		ORDER BY wm.user_id`), workspaceID)
	if err != nil {
		return nil, fmt.Errorf("workspace owner ids: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// openMentionSQL is the predicate for an OPEN mention row m (comment_mentions)
// on item alias i: the comment is live, its author is not the mentioned user,
// and the mentioned user has written no live human comment on the item since
// (a reply at the same second counts as an answer: >=). It binds nothing.
const openMentionSQL = `
		JOIN comments cm ON cm.id = m.comment_id AND cm.deleted_at IS NULL
		WHERE NOT (COALESCE(cm.user_id, '') = m.user_id AND cm.created_by = 'user')
		  AND NOT EXISTS (
			SELECT 1 FROM comments r
			WHERE r.item_id = m.item_id AND r.deleted_at IS NULL AND r.created_by = 'user'
			  AND r.user_id = m.user_id AND r.created_at >= m.created_at)`

// NeedsCandidateParams selects the items the router considers in one workspace.
type NeedsCandidateParams struct {
	// AwaitsCollectionIDs are the awaits_human collections: their items are
	// candidates. FlaggedItemIDs are the needs_human items. An item with an
	// open mention is a candidate in any non-system collection.
	AwaitsCollectionIDs []string
	FlaggedItemIDs      []string
	// ForUserID, when set, keeps only the items that user is the assignee of or
	// holds an open mention on: the rows a non-owner caller can be routed.
	ForUserID string
}

// NeedsCandidateItems returns the live items in scope that are router
// candidates: in a live, non-system collection, and an ask, a flagged item or
// holding an open mention. The scope clause is the caller's visibility and is
// applied in SQL, so nothing else is read for an item outside it. Status is not
// filtered here (it is per-collection JSON); the router drops terminal items.
// Only the columns a needs row shows are filled; Content is empty.
func (s *Store) NeedsCandidateItems(workspaceID string, scope CatchUpScope, p NeedsCandidateParams) ([]models.Item, error) {
	if scope.seesNothing() {
		return nil, nil
	}
	scopeSQL, scopeArgs := scope.clause("i")
	args := []any{workspaceID}
	args = append(args, scopeArgs...)

	var src []string
	for _, chunk := range chunkStrings(p.AwaitsCollectionIDs, needsChunk) {
		src = append(src, "i.collection_id IN ("+placeholders(len(chunk))+")")
		for _, id := range chunk {
			args = append(args, id)
		}
	}
	for _, chunk := range chunkStrings(p.FlaggedItemIDs, needsChunk) {
		src = append(src, "i.id IN ("+placeholders(len(chunk))+")")
		for _, id := range chunk {
			args = append(args, id)
		}
	}
	src = append(src, `EXISTS (SELECT 1 FROM comment_mentions m `+openMentionSQL+` AND m.item_id = i.id)`)

	q := `
		SELECT i.id, i.collection_id, i.title, i.slug, i.fields, i.assigned_user_id, COALESCE(au.name, ''),
		       i.item_number, i.created_at, i.updated_at, c.slug, c.name, c.prefix
		FROM items i
		JOIN collections c ON c.id = i.collection_id AND c.deleted_at IS NULL AND NOT c.is_system
		LEFT JOIN users au ON au.id = i.assigned_user_id
		WHERE i.workspace_id = ? AND i.deleted_at IS NULL` + scopeSQL + `
		  AND (` + strings.Join(src, " OR ") + `)`
	if p.ForUserID != "" {
		q += `
		  AND (i.assigned_user_id = ? OR EXISTS (SELECT 1 FROM comment_mentions m ` + openMentionSQL + ` AND m.item_id = i.id AND m.user_id = ?))`
		args = append(args, p.ForUserID, p.ForUserID)
	}
	q += ` ORDER BY i.created_at, i.id`

	rows, err := s.db.Query(s.q(q), args...)
	if err != nil {
		return nil, fmt.Errorf("needs candidate items: %w", err)
	}
	defer rows.Close()
	var out []models.Item
	for rows.Next() {
		var it models.Item
		var createdAt, updatedAt string
		if err := rows.Scan(&it.ID, &it.CollectionID, &it.Title, &it.Slug, &it.Fields, &it.AssignedUserID, &it.AssignedUserName,
			&it.ItemNumber, &createdAt, &updatedAt, &it.CollectionSlug, &it.CollectionName, &it.CollectionPrefix); err != nil {
			return nil, fmt.Errorf("scan needs candidate: %w", err)
		}
		it.WorkspaceID = workspaceID
		it.CreatedAt = parseTime(createdAt)
		it.UpdatedAt = parseTime(updatedAt)
		it.ComputeRef()
		out = append(out, it)
	}
	return out, rows.Err()
}

// NeedsMention is one open @mention of a user on an item.
type NeedsMention struct {
	UserID    string
	CommentID string
	By        string // the commenter's display name
	At        time.Time
}

// OpenMentionsForItems returns the open mentions on the given items (see
// openMentionSQL), per item ordered by mention time then user id.
func (s *Store) OpenMentionsForItems(itemIDs []string) (map[string][]NeedsMention, error) {
	out := map[string][]NeedsMention{}
	for _, chunk := range chunkStrings(itemIDs, needsChunk) {
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.Query(s.q(`
			SELECT m.item_id, m.user_id, m.comment_id, cm.author, m.created_at
			FROM comment_mentions m `+openMentionSQL+`
			  AND m.item_id IN (`+placeholders(len(chunk))+`)
			ORDER BY m.item_id, m.created_at, m.user_id`), args...)
		if err != nil {
			return nil, fmt.Errorf("open mentions: %w", err)
		}
		for rows.Next() {
			var itemID, at string
			var m NeedsMention
			if err := rows.Scan(&itemID, &m.UserID, &m.CommentID, &m.By, &at); err != nil {
				rows.Close()
				return nil, err
			}
			m.At = parseTime(at)
			out[itemID] = append(out[itemID], m)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// NeedsSnoozesForItems returns userID's snoozes on the given items of one
// workspace with Holds evaluated at now, keyed by item id. It is the batch of
// GetNeedsSnooze with two scoping rules (TASK-3920, the day-19 rulings):
//
//   - the snooze is joined to its ITEM and scoped by items.workspace_id, never
//     by item_needs_snoozes.workspace_id: a cross-workspace move clones the item
//     and leaves the snooze on the archived source, which is not live here;
//   - the caller passes ids it has already filtered by visibility, so a snooze
//     on an item the user can no longer see is never read.
func (s *Store) NeedsSnoozesForItems(userID, workspaceID string, itemIDs []string, now time.Time) (map[string]*models.NeedsSnooze, error) {
	out := map[string]*models.NeedsSnooze{}
	var dismissed []string
	for _, chunk := range chunkStrings(itemIDs, needsChunk) {
		args := []any{userID, workspaceID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT s.item_id, s.kind, s.defer_until, s.seen_comment_at, s.seen_comment_ids, s.seen_transition_seq, s.created_at
			FROM item_needs_snoozes s JOIN items i ON i.id = s.item_id
			WHERE s.user_id = ? AND i.workspace_id = ? AND i.deleted_at IS NULL
			  AND s.item_id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("needs snoozes: %w", err)
		}
		for rows.Next() {
			sn := &models.NeedsSnooze{WorkspaceID: workspaceID}
			var until sql.NullString
			var ids string
			if err := rows.Scan(&sn.ItemID, &sn.Kind, &until, &sn.Marker.SeenCommentAt, &ids, &sn.Marker.SeenTransitionSeq, &sn.CreatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			sn.DeferUntil = until.String
			if err := json.Unmarshal([]byte(ids), &sn.Marker.SeenCommentIDs); err != nil {
				rows.Close()
				return nil, fmt.Errorf("needs snooze seen_comment_ids: %w", err)
			}
			if sn.Kind == models.NeedsSnoozeDefer {
				t, err := time.Parse(time.RFC3339, sn.DeferUntil)
				sn.Holds = err == nil && now.Before(t)
			} else {
				dismissed = append(dismissed, sn.ItemID)
			}
			out[sn.ItemID] = sn
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(dismissed) == 0 {
		return out, nil
	}
	cur, err := s.needsItemStates(userID, dismissed, out)
	if err != nil {
		return nil, err
	}
	for _, id := range dismissed {
		out[id].Holds = needsDismissHolds(out[id].Marker, cur[id])
	}
	return out, nil
}

// needsItemStates is needsItemState for many items: the newest other comment's
// second, the other comments at the second the snooze's marker names (the only
// second whose ids matter; every item's newest second when snoozes is nil), and
// the latest transition seq.
func (s *Store) needsItemStates(userID string, itemIDs []string, snoozes map[string]*models.NeedsSnooze) (map[string]models.NeedsSnoozeMarker, error) {
	cur := make(map[string]models.NeedsSnoozeMarker, len(itemIDs))
	for _, id := range itemIDs {
		cur[id] = models.NeedsSnoozeMarker{}
	}
	for _, chunk := range chunkStrings(itemIDs, needsChunk) {
		args := []any{userID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`SELECT item_id, MAX(created_at) FROM comments WHERE `+needsOthersComment+
			` AND item_id IN (`+placeholders(len(chunk))+`) GROUP BY item_id`), args...)
		if err != nil {
			return nil, fmt.Errorf("read latest comments: %w", err)
		}
		for rows.Next() {
			var id, at string
			if err := rows.Scan(&id, &at); err != nil {
				rows.Close()
				return nil, err
			}
			m := cur[id]
			m.SeenCommentAt = at
			cur[id] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		args = args[1:]
		rows, err = s.db.Query(s.q(`SELECT item_id, COALESCE(MAX(seq), 0) FROM status_transitions WHERE item_id IN (`+
			placeholders(len(chunk))+`) GROUP BY item_id`), args...)
		if err != nil {
			return nil, fmt.Errorf("read latest transitions: %w", err)
		}
		for rows.Next() {
			var id string
			var seq int64
			if err := rows.Scan(&id, &seq); err != nil {
				rows.Close()
				return nil, err
			}
			m := cur[id]
			m.SeenTransitionSeq = seq
			cur[id] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	// The ids at a second matter only where the item's newest second is the
	// marker's second; fetch them for those (item, second) pairs together.
	var pairs []string
	for _, id := range itemIDs {
		if at := cur[id].SeenCommentAt; at != "" && (snoozes == nil || at == snoozes[id].Marker.SeenCommentAt) {
			pairs = append(pairs, id)
		}
	}
	for _, chunk := range chunkStrings(pairs, needsChunk/2) {
		args := []any{userID}
		conds := make([]string, len(chunk))
		for i, id := range chunk {
			conds[i] = "(item_id = ? AND created_at = ?)"
			args = append(args, id, cur[id].SeenCommentAt)
		}
		rows, err := s.db.Query(s.q(`SELECT item_id, id FROM comments WHERE `+needsOthersComment+
			` AND (`+strings.Join(conds, " OR ")+`) ORDER BY item_id, id`), args...)
		if err != nil {
			return nil, fmt.Errorf("read comments at the marker second: %w", err)
		}
		for rows.Next() {
			var itemID, id string
			if err := rows.Scan(&itemID, &id); err != nil {
				rows.Close()
				return nil, err
			}
			m := cur[itemID]
			m.SeenCommentIDs = append(m.SeenCommentIDs, id)
			cur[itemID] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return cur, nil
}

// NeedsMarkersForItems returns each item's state as userID would see it now
// (the marker a snooze made at this moment would carry), for the rows a page
// shows. Every id gets an entry, with a non-nil SeenCommentIDs.
func (s *Store) NeedsMarkersForItems(userID string, itemIDs []string) (map[string]models.NeedsSnoozeMarker, error) {
	cur, err := s.needsItemStates(userID, itemIDs, nil)
	if err != nil {
		return nil, err
	}
	for id, m := range cur {
		if m.SeenCommentIDs == nil {
			m.SeenCommentIDs = []string{}
			cur[id] = m
		}
	}
	return cur, nil
}

// LatestAssignmentTimes returns, per item, when it was last assigned to the
// named person: the newest "updated" activity whose change list holds
// "assigned: <before> → <name>". assigned maps item id to the assignee's
// display name. An item with no such row is absent (the caller falls back to
// the item's created_at: an item created already assigned has no row).
// The activity trail records the name, not the id, so two people sharing a
// display name can be confused; the worst effect is an age.
func (s *Store) LatestAssignmentTimes(workspaceID string, assigned map[string]string) (map[string]time.Time, error) {
	ids := make([]string, 0, len(assigned))
	for id := range assigned {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := map[string]time.Time{}
	for _, chunk := range chunkStrings(ids, needsChunk) {
		args := []any{workspaceID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.db.Query(s.q(`
			SELECT a.document_id, `+s.activityMetaText("a")+`, a.created_at
			FROM activities a
			WHERE a.workspace_id = ? AND a.action = 'updated'
			  AND `+s.activityMetaText("a")+` LIKE '%assigned: %'
			  AND a.document_id IN (`+placeholders(len(chunk))+`)
			ORDER BY a.created_at, a.id`), args...)
		if err != nil {
			return nil, fmt.Errorf("assignment activity: %w", err)
		}
		for rows.Next() {
			var id, meta, at string
			if err := rows.Scan(&id, &meta, &at); err != nil {
				rows.Close()
				return nil, err
			}
			var m map[string]any
			if json.Unmarshal([]byte(meta), &m) != nil {
				continue
			}
			changes, _ := m["changes"].(string)
			for _, part := range strings.Split(changes, "; ") {
				if strings.HasPrefix(part, "assigned: ") && strings.HasSuffix(part, " → "+assigned[id]) {
					out[id] = parseTime(at) // rows ascend, so the last match is the newest
				}
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
