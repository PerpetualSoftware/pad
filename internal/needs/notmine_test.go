package needs

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The "not mine" release rule (TASK-4643), one case per signal and the edges
// of each.
func TestNotMineHolds(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	stamp := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	marker := models.NeedsSnoozeMarker{
		SeenCommentAt: stamp(0), SeenCommentIDs: []string{"c0"}, SeenAt: stamp(time.Minute),
		SeenCollectionID: "asks", SeenAssigneeID: "me",
	}
	base := NotMineFacts{ViewerID: "me", CollectionID: "asks", AwaitsHuman: true, AssigneeUserID: "me"}
	due := func(d time.Duration) *time.Time { at := t0.Add(d); return &at }
	mention := func(user, id string, comment, at time.Duration) Mention {
		return Mention{UserID: user, CommentID: id, CommentAt: t0.Add(comment), At: t0.Add(at)}
	}
	for _, tc := range []struct {
		name  string
		m     models.NeedsSnoozeMarker
		edit  func(*NotMineFacts)
		holds bool
	}{
		{"nothing new", marker, nil, true},
		{"the marker's own mention", marker, func(f *NotMineFacts) { f.Mentions = []Mention{mention("me", "c0", 0, 0)} }, true},
		{"an older mention", marker, func(f *NotMineFacts) { f.Mentions = []Mention{mention("me", "old", -time.Hour, -time.Hour)} }, true},
		{"someone else's mention", marker, func(f *NotMineFacts) { f.Mentions = []Mention{mention("you", "c1", time.Hour, time.Hour)} }, true},
		{"a newer comment mentions me", marker, func(f *NotMineFacts) { f.Mentions = []Mention{mention("me", "c1", 30*time.Second, 30*time.Second)} }, false},
		{"a same-second comment outside the marker", marker, func(f *NotMineFacts) { f.Mentions = []Mention{mention("me", "c1", 0, 0)} }, false},
		{"an edit adds me after seen_at", marker, func(f *NotMineFacts) { f.Mentions = []Mention{mention("me", "c0", 0, 2*time.Minute)} }, false},
		{"an edit adds me at seen_at's second", marker, func(f *NotMineFacts) {
			f.Mentions = []Mention{mention("me", "c0", 0, time.Minute+500*time.Millisecond)}
		}, true},
		{"a waiting transition", marker, func(f *NotMineFacts) { f.WaitingTransition = true }, false},
		{"still in the same ask collection", marker, nil, true},
		{"moved into another ask collection", marker, func(f *NotMineFacts) { f.CollectionID = "other-asks" }, false},
		{"moved into a plain collection", marker, func(f *NotMineFacts) { f.CollectionID, f.AwaitsHuman = "plain", false }, true},
		{"assigned to someone else", marker, func(f *NotMineFacts) { f.AssigneeUserID = "you" }, true},
		{"assigned to me before seen_at", marker, func(f *NotMineFacts) { f.AssignedAt = t0.Add(30 * time.Second) }, true},
		{"assigned to me at seen_at's second", marker, func(f *NotMineFacts) { f.AssignedAt = t0.Add(time.Minute + 500*time.Millisecond) }, true},
		{"reassigned to me after seen_at", marker, func(f *NotMineFacts) { f.AssignedAt = t0.Add(2 * time.Minute) }, false},
		{"assigned to someone else after seen_at", marker, func(f *NotMineFacts) { f.AssigneeUserID, f.AssignedAt = "you", t0.Add(2*time.Minute) }, true},
		{"newly assigned to me", models.NeedsSnoozeMarker{SeenCommentAt: marker.SeenCommentAt, SeenCommentIDs: marker.SeenCommentIDs, SeenAt: marker.SeenAt, SeenCollectionID: "asks", SeenAssigneeID: "you"}, nil, false},
		{"due before seen_at", marker, func(f *NotMineFacts) { f.DueLatest = due(30 * time.Second) }, true},
		{"due at seen_at", marker, func(f *NotMineFacts) { f.DueLatest = due(time.Minute) }, true},
		{"due after seen_at", marker, func(f *NotMineFacts) { f.DueLatest = due(time.Minute + time.Second) }, false},
		{"no seen_at: a due reminder releases", models.NeedsSnoozeMarker{SeenCommentAt: marker.SeenCommentAt, SeenCollectionID: "asks", SeenAssigneeID: "me"}, func(f *NotMineFacts) { f.DueLatest = due(-time.Hour) }, false},
		{"no seen_comment_at: any mention releases", models.NeedsSnoozeMarker{SeenAt: marker.SeenAt, SeenCollectionID: "asks", SeenAssigneeID: "me"}, func(f *NotMineFacts) { f.Mentions = []Mention{mention("me", "old", -time.Hour, -time.Hour)} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			if tc.edit != nil {
				tc.edit(&f)
			}
			if got := NotMineHolds(tc.m, f); got != tc.holds {
				t.Fatalf("holds = %v, want %v", got, tc.holds)
			}
		})
	}
}
