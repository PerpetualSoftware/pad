package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Needs-you snoozes (TASK-3918, IDEA-3906 half A unit A2): one person's
// dismissal or deferral of one item. See migration 153 for the hold rules.

// MaxNeedsDefer is how far ahead a deferral may be set.
const MaxNeedsDefer = 366 * 24 * time.Hour

// ErrInvalidNeedsSnooze reports a snooze request the store refuses: an
// unknown kind, or a deferral that is not after now and within
// MaxNeedsDefer. The handler answers it with 400.
var ErrInvalidNeedsSnooze = errors.New("invalid needs snooze")

// needsOthersComment is the comment predicate a dismissal watches: a live
// comment that is not the snoozing user's own human comment. The user's
// agent-authored comments count, as do comments with no user (legacy,
// apps). Imported comments do not: an import always creates new items, so
// they are history no snooze can predate, and their created_at is the
// bundle's text verbatim (export.go), which may not sort with now()'s
// format (#2231 r1). Bind the user id.
const needsOthersComment = `deleted_at IS NULL AND imported = 0 AND NOT (COALESCE(user_id, '') = ? AND created_by = 'user')`

// needsStamp is one other comment with its created_at normalised to the
// UTC whole second (needsCommentSecond).
type needsStamp struct {
	id, at string
}

// needsCommentSecond normalises a stored comments.created_at to the UTC whole
// second in time.RFC3339 (truncated, never rounded), so the marker never
// depends on how the row was stamped: a numeric offset and fractional seconds
// read as the instant they name. ok is false when the text does not parse.
func needsCommentSecond(raw string) (string, bool) {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "", false
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339), true
}

// needsOtherCommentStamps reads the other comments (needsOthersComment) on
// the items, keyed by item id, each with its normalised second.
func (s *Store) needsOtherCommentStamps(q Queryer, userID string, itemIDs []string) (map[string][]needsStamp, error) {
	out := map[string][]needsStamp{}
	for _, chunk := range chunkStrings(itemIDs, needsChunk) {
		args := []any{userID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := q.Query(s.q(`SELECT item_id, id, created_at FROM comments WHERE `+needsOthersComment+
			` AND item_id IN (`+placeholders(len(chunk))+`)`), args...)
		if err != nil {
			return nil, fmt.Errorf("read other comments: %w", err)
		}
		for rows.Next() {
			var itemID, id, raw string
			if err := rows.Scan(&itemID, &id, &raw); err != nil {
				rows.Close()
				return nil, err
			}
			at, ok := needsCommentSecond(raw)
			if !ok {
				// Skipped rather than failing the read: native writers stamp
				// now(), so no native row is unparseable, and imported rows
				// (bundle text verbatim) are excluded by needsOthersComment.
				continue
			}
			out[itemID] = append(out[itemID], needsStamp{id: id, at: at})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// newestNeedsSecond is the newest normalised second among stamps ("" for
// none). The normalised form is fixed-width UTC, so it orders as text.
func newestNeedsSecond(stamps []needsStamp) string {
	newest := ""
	for _, st := range stamps {
		if st.at > newest {
			newest = st.at
		}
	}
	return newest
}

// needsIDsAt lists, sorted, the ids of the stamps at the normalised second at
// (none for "").
func needsIDsAt(stamps []needsStamp, at string) []string {
	ids := []string{}
	if at == "" {
		return ids
	}
	for _, st := range stamps {
		if st.at == at {
			ids = append(ids, st.id)
		}
	}
	sort.Strings(ids)
	return ids
}

// needsItemState is the item's state as the snoozing user would see it now:
// the newest other comment's normalised second (needsCommentSecond), the ids
// at that second, and the latest status-transition seq (legacy rows are 0).
func (s *Store) needsItemState(q Queryer, userID, itemID string) (models.NeedsSnoozeMarker, error) {
	var st models.NeedsSnoozeMarker
	stamps, err := s.needsOtherCommentStamps(q, userID, []string{itemID})
	if err != nil {
		return st, err
	}
	st.SeenCommentAt = newestNeedsSecond(stamps[itemID])
	st.SeenCommentIDs = needsIDsAt(stamps[itemID], st.SeenCommentAt)
	if err := q.QueryRow(s.q(`SELECT COALESCE(MAX(seq), 0) FROM status_transitions WHERE item_id = ?`),
		itemID).Scan(&st.SeenTransitionSeq); err != nil {
		return st, fmt.Errorf("read latest transition: %w", err)
	}
	return st, nil
}

// needsCommentIDsAt lists, sorted, the other comments whose normalised second
// (needsCommentSecond) is `at`, itself a normalised second.
func (s *Store) needsCommentIDsAt(q Queryer, userID, itemID, at string) ([]string, error) {
	if at == "" {
		return []string{}, nil
	}
	stamps, err := s.needsOtherCommentStamps(q, userID, []string{itemID})
	if err != nil {
		return nil, err
	}
	return needsIDsAt(stamps[itemID], at), nil
}

// clampNeedsMarker brings a client's marker within the server's state, so it
// can only claim less was seen: the second, normalised by needsCommentSecond,
// is no later than the newest other comment's (an
// unparseable one is "", seen nothing), the ids are the other comments whose
// normalised second it is, and the seq is no higher than the item's. Both
// seconds are fixed-width UTC, so they order as text.
func (s *Store) clampNeedsMarker(q Queryer, userID, itemID string, m models.NeedsSnoozeMarker, cur models.NeedsSnoozeMarker) (models.NeedsSnoozeMarker, error) {
	var out models.NeedsSnoozeMarker
	out.SeenCommentAt, _ = needsCommentSecond(m.SeenCommentAt)
	if out.SeenCommentAt > cur.SeenCommentAt {
		out.SeenCommentAt = cur.SeenCommentAt
	}
	real, err := s.needsCommentIDsAt(q, userID, itemID, out.SeenCommentAt)
	if err != nil {
		return out, err
	}
	claimed := make(map[string]bool, len(m.SeenCommentIDs))
	for _, id := range m.SeenCommentIDs {
		claimed[id] = true
	}
	out.SeenCommentIDs = []string{}
	for _, id := range real {
		if claimed[id] {
			out.SeenCommentIDs = append(out.SeenCommentIDs, id)
		}
	}
	out.SeenTransitionSeq = min(max(m.SeenTransitionSeq, 0), cur.SeenTransitionSeq)
	return out, nil
}

// SnoozeNeedsItem records userID's dismissal or deferral of an item,
// replacing any earlier one. kind is models.NeedsSnoozeDismiss or
// models.NeedsSnoozeDefer; deferUntil is read only for a deferral. marker is
// what the client had seen (nil: the item's current state) and is clamped
// to the server's values. Any "not mine" of the user's on the item is
// deleted in the same transaction. The caller has already checked the user
// may see the item.
func (s *Store) SnoozeNeedsItem(userID, workspaceID, itemID, kind string, deferUntil time.Time, marker *models.NeedsSnoozeMarker) (*models.NeedsSnooze, error) {
	nowT := time.Now().UTC()
	var until sql.NullString
	switch kind {
	case models.NeedsSnoozeDismiss:
	case models.NeedsSnoozeDefer:
		if !deferUntil.After(nowT) || deferUntil.After(nowT.Add(MaxNeedsDefer)) {
			return nil, fmt.Errorf("%w: defer_until must be after now and within 366 days", ErrInvalidNeedsSnooze)
		}
		// Stored in whole seconds, rounded UP so a fractional time never
		// brings the item back early (#2231 r1).
		if t := deferUntil.Truncate(time.Second); t.Before(deferUntil) {
			deferUntil = t.Add(time.Second)
		}
		until = sql.NullString{String: deferUntil.UTC().Format(time.RFC3339), Valid: true}
	default:
		return nil, fmt.Errorf("%w: kind must be %q or %q", ErrInvalidNeedsSnooze, models.NeedsSnoozeDismiss, models.NeedsSnoozeDefer)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.lockNeedsHide(tx, userID, itemID); err != nil {
		return nil, err
	}
	seen, err := s.needsItemState(tx, userID, itemID)
	if err != nil {
		return nil, err
	}
	if marker != nil {
		if seen, err = s.clampNeedsMarker(tx, userID, itemID, *marker, seen); err != nil {
			return nil, err
		}
	}
	ids, err := json.Marshal(seen.SeenCommentIDs)
	if err != nil {
		return nil, err
	}
	created := nowT.Format(time.RFC3339)
	if _, err := tx.Exec(s.q(`
		INSERT INTO item_needs_snoozes
			(user_id, item_id, workspace_id, kind, defer_until, seen_comment_at, seen_comment_ids, seen_transition_seq, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, item_id) DO UPDATE SET
			kind                = excluded.kind,
			defer_until         = excluded.defer_until,
			seen_comment_at     = excluded.seen_comment_at,
			seen_comment_ids    = excluded.seen_comment_ids,
			seen_transition_seq = excluded.seen_transition_seq,
			created_at          = excluded.created_at`),
		userID, itemID, workspaceID, kind, until, seen.SeenCommentAt, string(ids), seen.SeenTransitionSeq, created); err != nil {
		return nil, fmt.Errorf("snooze needs item: %w", err)
	}
	// One hide state at a time: a snooze replaces a "not mine" (TASK-4643).
	if _, err := tx.Exec(s.q(`DELETE FROM item_needs_not_mine WHERE user_id = ? AND item_id = ?`), userID, itemID); err != nil {
		return nil, fmt.Errorf("clear needs not-mine: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetNeedsSnooze(userID, itemID, nowT)
}

// UnsnoozeNeedsItem removes userID's snooze on an item. It reports whether
// there was one.
func (s *Store) UnsnoozeNeedsItem(userID, itemID string) (bool, error) {
	res, err := s.db.Exec(s.q(`DELETE FROM item_needs_snoozes WHERE user_id = ? AND item_id = ?`), userID, itemID)
	if err != nil {
		return false, fmt.Errorf("unsnooze needs item: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetNeedsSnooze returns userID's snooze on an item with Holds evaluated at
// now, or nil when there is none. A deferral holds until defer_until, whatever
// happens before then (IDEA-3906 A2). A dismissal holds until the item's
// state passes the marker: a newer other comment, another comment at the
// marker's second, or a later status transition.
func (s *Store) GetNeedsSnooze(userID, itemID string, now time.Time) (*models.NeedsSnooze, error) {
	var sn models.NeedsSnooze
	var until sql.NullString
	var ids string
	err := s.db.QueryRow(s.q(`
		SELECT item_id, workspace_id, kind, defer_until, seen_comment_at, seen_comment_ids, seen_transition_seq, created_at
		FROM item_needs_snoozes WHERE user_id = ? AND item_id = ?`), userID, itemID).
		Scan(&sn.ItemID, &sn.WorkspaceID, &sn.Kind, &until, &sn.Marker.SeenCommentAt, &ids, &sn.Marker.SeenTransitionSeq, &sn.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get needs snooze: %w", err)
	}
	sn.DeferUntil = until.String
	if err := json.Unmarshal([]byte(ids), &sn.Marker.SeenCommentIDs); err != nil {
		return nil, fmt.Errorf("needs snooze seen_comment_ids: %w", err)
	}
	if sn.Kind == models.NeedsSnoozeDefer {
		t, err := time.Parse(time.RFC3339, sn.DeferUntil)
		sn.Holds = err == nil && now.Before(t)
		return &sn, nil
	}
	cur, err := s.needsItemState(s.db, userID, itemID)
	if err != nil {
		return nil, err
	}
	sn.Holds = needsDismissHolds(sn.Marker, cur)
	return &sn, nil
}

// needsDismissHolds compares a dismissal's marker with the item's current
// state. Both seconds are normalised fixed-width UTC (needsCommentSecond,
// clampNeedsMarker), never raw comments.created_at text, so they order as
// text. A comment deleted since the marker leaves the newest second earlier,
// which never breaks the hold.
func needsDismissHolds(seen, cur models.NeedsSnoozeMarker) bool {
	if cur.SeenTransitionSeq > seen.SeenTransitionSeq || cur.SeenCommentAt > seen.SeenCommentAt {
		return false
	}
	if cur.SeenCommentAt < seen.SeenCommentAt {
		return true
	}
	had := make(map[string]bool, len(seen.SeenCommentIDs))
	for _, id := range seen.SeenCommentIDs {
		had[id] = true
	}
	for _, id := range cur.SeenCommentIDs {
		if !had[id] {
			return false
		}
	}
	return true
}
