package needs

import (
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// NotMineFacts are what the "not mine" release rule reads about one item
// (TASK-4643), all of it the viewer's own or the item's: no other person's
// state is consulted.
type NotMineFacts struct {
	ViewerID string
	// Mentions are the item's open mentions; only the viewer's are read.
	Mentions []Mention
	// CollectionID is the item's collection now; AwaitsHuman reports whether
	// it is an awaits_human (ask) collection.
	CollectionID string
	AwaitsHuman  bool
	// AssigneeUserID is the item's assigned person now, or "".
	AssigneeUserID string
	// WaitingTransition: a status transition with a seq above the marker's
	// moved the done field into one of its waiting statuses.
	WaitingTransition bool
	// DueLatest is when the newest outstanding reminder due to the viewer
	// came due, or nil when none is.
	DueLatest *time.Time
}

// NotMineHolds reports whether a "not mine" marker still hides the item. It
// is released by any of:
//
//  1. an open mention of the viewer whose comment falls outside the marker
//     (newer than seen_comment_at, or at that second and not in
//     seen_comment_ids), or whose mention was made after seen_at (an edit
//     can add a mention to an older comment);
//  2. a transition after seen_transition_seq into a waiting status;
//  3. the item now in an ask collection other than seen_collection_id;
//  4. the item now assigned to the viewer when seen_assignee_id was not
//     the viewer;
//  5. a reminder due to the viewer that came due after seen_at.
//
// Times are compared in whole seconds and must be strictly later. An empty or
// unparseable seen time claims nothing was seen, so anything releases.
func NotMineHolds(m models.NeedsSnoozeMarker, f NotMineFacts) bool {
	seenAt, atOK := parseSeen(m.SeenAt)
	seenComment, commentOK := parseSeen(m.SeenCommentAt)
	later := func(t time.Time, seen time.Time, ok bool) bool {
		return !ok || t.UTC().Truncate(time.Second).After(seen)
	}
	inMarker := make(map[string]bool, len(m.SeenCommentIDs))
	for _, id := range m.SeenCommentIDs {
		inMarker[id] = true
	}
	for _, mn := range f.Mentions {
		if mn.UserID != f.ViewerID {
			continue
		}
		if later(mn.At, seenAt, atOK) || later(mn.CommentAt, seenComment, commentOK) {
			return false
		}
		if mn.CommentAt.UTC().Truncate(time.Second).Equal(seenComment) && !inMarker[mn.CommentID] {
			return false
		}
	}
	if f.WaitingTransition {
		return false
	}
	if f.AwaitsHuman && f.CollectionID != m.SeenCollectionID {
		return false
	}
	if f.ViewerID != "" && f.AssigneeUserID == f.ViewerID && m.SeenAssigneeID != f.ViewerID {
		return false
	}
	if f.DueLatest != nil && later(*f.DueLatest, seenAt, atOK) {
		return false
	}
	return true
}

func parseSeen(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC().Truncate(time.Second), true
}
