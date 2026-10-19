package store

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Needs-you snoozes (TASK-3918). Runs on both backends via testStore
// (PAD_TEST_POSTGRES_URL selects Postgres).

type needsFixture struct {
	s     *Store
	ws    *models.Workspace
	item  *models.Item
	me    *models.User
	other *models.User
}

func newNeedsFixture(t *testing.T) needsFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Needs")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	return needsFixture{
		s: s, ws: ws,
		item:  createTestItem(t, s, ws.ID, col.ID, "Asked", ""),
		me:    needsUser(t, s, "me@test.com"),
		other: needsUser(t, s, "other@test.com"),
	}
}

func needsUser(t *testing.T, s *Store, email string) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Email: email, Name: email, Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u
}

// comment adds a comment by u (nil: no user) with createdBy, stamped at `at`
// so same-second cases are exact rather than timing-dependent.
func (f needsFixture) comment(t *testing.T, u *models.User, createdBy string, at time.Time) *models.Comment {
	t.Helper()
	uid := ""
	if u != nil {
		uid = u.ID
	}
	c, err := f.s.CreateComment(f.ws.ID, f.item.ID, uid, models.CommentCreate{Body: "b", Author: "a", CreatedBy: createdBy, Source: "web"})
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE comments SET created_at = ? WHERE id = ?`), at.UTC().Format(time.RFC3339), c.ID); err != nil {
		t.Fatal(err)
	}
	return c
}

func (f needsFixture) setStatus(t *testing.T, status string) {
	t.Helper()
	fields := `{"status":"` + status + `"}`
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{Fields: &fields}); err != nil {
		t.Fatal(err)
	}
}

func (f needsFixture) snooze(t *testing.T, u *models.User, kind string, until time.Time, m *models.NeedsSnoozeMarker) *models.NeedsSnooze {
	t.Helper()
	sn, err := f.s.SnoozeNeedsItem(u.ID, f.ws.ID, f.item.ID, kind, until, m)
	if err != nil {
		t.Fatalf("snooze: %v", err)
	}
	return sn
}

func (f needsFixture) holds(t *testing.T, u *models.User, at time.Time) bool {
	t.Helper()
	sn, err := f.s.GetNeedsSnooze(u.ID, f.item.ID, at)
	if err != nil {
		t.Fatal(err)
	}
	if sn == nil {
		t.Fatal("no snooze row")
	}
	return sn.Holds
}

var needsT0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func TestNeedsSnooze_NewCommentResurfaces(t *testing.T) {
	f := newNeedsFixture(t)
	f.comment(t, f.other, "user", needsT0)
	f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("a fresh dismissal does not hold")
	}

	// My own human comment never re-surfaces it.
	f.comment(t, f.me, "user", needsT0.Add(time.Minute))
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("my own human comment broke my dismissal")
	}
	// A deleted comment by someone else does not either. It has a reply
	// (mine), so the delete leaves a TOMBSTONE row with deleted_at set
	// rather than removing the row.
	gone := f.comment(t, f.other, "user", needsT0.Add(2*time.Minute))
	reply, err := f.s.CreateComment(f.ws.ID, f.item.ID, f.me.ID, models.CommentCreate{Body: "r", Author: "a", CreatedBy: "user", Source: "web", ParentID: gone.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeleteComment(gone.ID); err != nil {
		t.Fatal(err)
	}
	var tomb int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM comments WHERE id = ? AND deleted_at IS NOT NULL`), gone.ID).Scan(&tomb); err != nil || tomb != 1 {
		t.Fatalf("precondition: the deleted comment is a tombstone (%d, %v; reply %s)", tomb, err, reply.ID)
	}
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("a deleted comment broke the dismissal")
	}
	// An imported comment is history, whatever its created_at says: this one
	// carries a foreign offset form that sorts after every native stamp.
	imp := f.comment(t, f.other, "user", needsT0)
	if _, err := f.s.db.Exec(f.s.q(`UPDATE comments SET imported = 1, created_at = ? WHERE id = ?`), "2026-12-01T00:00:00+02:00", imp.ID); err != nil {
		t.Fatal(err)
	}
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("an imported comment broke the dismissal")
	}
	// An agent writing under my credentials is news.
	f.comment(t, f.me, "agent", needsT0.Add(3*time.Minute))
	if f.holds(t, f.me, time.Now()) {
		t.Fatal("an agent comment under my credentials did not re-surface the item")
	}

	// Someone else's newer comment re-surfaces it (fresh dismissal first).
	f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("re-dismissal does not hold")
	}
	f.comment(t, f.other, "user", needsT0.Add(4*time.Minute))
	if f.holds(t, f.me, time.Now()) {
		t.Fatal("another person's newer comment did not re-surface the item")
	}
}

func TestNeedsSnooze_SameSecondBoundary(t *testing.T) {
	f := newNeedsFixture(t)
	seen := f.comment(t, f.other, "user", needsT0)
	sn := f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	if sn.Marker.SeenCommentAt != needsT0.Format(time.RFC3339) || len(sn.Marker.SeenCommentIDs) != 1 || sn.Marker.SeenCommentIDs[0] != seen.ID {
		t.Fatalf("marker = %+v, want the one comment at %s", sn.Marker, needsT0.Format(time.RFC3339))
	}
	// A second comment in the SAME second: not newer, but not seen.
	f.comment(t, f.other, "user", needsT0)
	if f.holds(t, f.me, time.Now()) {
		t.Fatal("a same-second comment outside seen_comment_ids did not re-surface the item")
	}
	// Control: dismissing now covers both, and it holds.
	f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("a dismissal that saw both same-second comments does not hold")
	}
}

func TestNeedsSnooze_StatusRoundTripResurfaces(t *testing.T) {
	f := newNeedsFixture(t)
	f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	f.setStatus(t, "done")
	f.setStatus(t, "open")
	var status string
	if err := f.s.db.QueryRow(f.s.q(`SELECT fields FROM items WHERE id = ?`), f.item.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	// Parsed, not compared as text: Postgres JSONB re-serialises the spacing.
	var got map[string]any
	if err := json.Unmarshal([]byte(status), &got); err != nil || got["status"] != "open" {
		t.Fatalf("precondition: the round trip ends where it began, got %s (%v)", status, err)
	}
	if f.holds(t, f.me, time.Now()) {
		t.Fatal("a status round trip (open -> done -> open) did not re-surface the item")
	}
}

func TestNeedsSnooze_DeferUntilAndChangeBeforeDate(t *testing.T) {
	f := newNeedsFixture(t)
	until := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	sn := f.snooze(t, f.me, models.NeedsSnoozeDefer, until, nil)
	if sn.DeferUntil != until.UTC().Format(time.RFC3339) {
		t.Fatalf("defer_until = %q, want %q", sn.DeferUntil, until.UTC().Format(time.RFC3339))
	}
	// A change before the date does not bring it back (IDEA-3906 A2).
	f.comment(t, f.other, "user", time.Now())
	f.setStatus(t, "done")
	if !f.holds(t, f.me, time.Now()) {
		t.Fatal("a change before the date brought a deferred item back")
	}
	if !f.holds(t, f.me, until.Add(-time.Second)) {
		t.Fatal("the deferral lapsed before its date")
	}
	if f.holds(t, f.me, until) {
		t.Fatal("the deferral still holds at its date")
	}

	// A fractional time is stored rounded UP, never early.
	frac := time.Now().Add(48 * time.Hour).Truncate(time.Second).Add(900 * time.Millisecond)
	if sn := f.snooze(t, f.me, models.NeedsSnoozeDefer, frac, nil); sn.DeferUntil != frac.Truncate(time.Second).Add(time.Second).UTC().Format(time.RFC3339) {
		t.Fatalf("fractional defer_until stored as %q, want the next whole second", sn.DeferUntil)
	}

	for name, at := range map[string]time.Time{
		"now":           time.Now(),
		"past":          time.Now().Add(-time.Hour),
		"beyond a year": time.Now().Add(MaxNeedsDefer + time.Hour),
	} {
		if _, err := f.s.SnoozeNeedsItem(f.me.ID, f.ws.ID, f.item.ID, models.NeedsSnoozeDefer, at, nil); !errors.Is(err, ErrInvalidNeedsSnooze) {
			t.Errorf("defer until %s: err = %v, want ErrInvalidNeedsSnooze", name, err)
		}
	}
	if _, err := f.s.SnoozeNeedsItem(f.me.ID, f.ws.ID, f.item.ID, "mute", time.Time{}, nil); !errors.Is(err, ErrInvalidNeedsSnooze) {
		t.Errorf("unknown kind: err = %v, want ErrInvalidNeedsSnooze", err)
	}
}

func TestNeedsSnooze_MarkerClamped(t *testing.T) {
	f := newNeedsFixture(t)
	old := f.comment(t, f.other, "user", needsT0)
	f.setStatus(t, "done")
	cur, err := f.s.needsItemState(f.s.db, f.me.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A forged marker claiming the future, a bigger seq and an unknown id is
	// clamped to what the server has.
	sn := f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, &models.NeedsSnoozeMarker{
		SeenCommentAt:     needsT0.Add(24 * time.Hour).Format(time.RFC3339),
		SeenCommentIDs:    []string{old.ID, "forged-id"},
		SeenTransitionSeq: cur.SeenTransitionSeq + 1000,
	})
	if sn.Marker.SeenCommentAt != needsT0.Format(time.RFC3339) ||
		len(sn.Marker.SeenCommentIDs) != 1 || sn.Marker.SeenCommentIDs[0] != old.ID ||
		sn.Marker.SeenTransitionSeq != cur.SeenTransitionSeq {
		t.Fatalf("forged marker stored as %+v, want clamped to %+v", sn.Marker, cur)
	}
	// So the next comment still re-surfaces the item.
	f.comment(t, f.other, "user", needsT0.Add(time.Hour))
	if f.holds(t, f.me, time.Now()) {
		t.Fatal("a forged future marker suppressed a real later comment")
	}

	// An OLDER marker is kept: the client saw less, so what it missed counts.
	sn = f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, &models.NeedsSnoozeMarker{
		SeenCommentAt:     needsT0.Format(time.RFC3339),
		SeenCommentIDs:    []string{old.ID},
		SeenTransitionSeq: cur.SeenTransitionSeq,
	})
	if sn.Marker.SeenCommentAt != needsT0.Format(time.RFC3339) || sn.Holds {
		t.Fatalf("older marker: stored %+v holds=%v, want kept and not holding", sn.Marker, sn.Holds)
	}
	// The ids are filtered to the ones claimed: a marker that saw only one
	// of two same-second comments stores that one, and does not hold.
	b1 := f.comment(t, f.other, "user", needsT0.Add(2*time.Hour))
	f.comment(t, f.other, "user", needsT0.Add(2*time.Hour))
	cur, err = f.s.needsItemState(f.s.db, f.me.ID, f.item.ID)
	if err != nil {
		t.Fatal(err)
	}
	sn = f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, &models.NeedsSnoozeMarker{
		SeenCommentAt:     needsT0.Add(2 * time.Hour).Format(time.RFC3339),
		SeenCommentIDs:    []string{b1.ID},
		SeenTransitionSeq: cur.SeenTransitionSeq,
	})
	if len(sn.Marker.SeenCommentIDs) != 1 || sn.Marker.SeenCommentIDs[0] != b1.ID || sn.Holds {
		t.Fatalf("partial same-second marker: stored %+v holds=%v, want only %s and not holding", sn.Marker, sn.Holds, b1.ID)
	}
	// An unparseable second means nothing was seen.
	sn = f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, &models.NeedsSnoozeMarker{SeenCommentAt: "yesterday"})
	if sn.Marker.SeenCommentAt != "" || sn.Holds {
		t.Fatalf("unparseable marker: stored %+v holds=%v, want empty and not holding", sn.Marker, sn.Holds)
	}
}

func TestNeedsSnooze_PerUser(t *testing.T) {
	f := newNeedsFixture(t)
	f.snooze(t, f.me, models.NeedsSnoozeDismiss, time.Time{}, nil)
	if sn, err := f.s.GetNeedsSnooze(f.other.ID, f.item.ID, time.Now()); err != nil || sn != nil {
		t.Fatalf("another user sees my snooze: %+v, %v", sn, err)
	}
	// The other person's own comment breaks MY dismissal, and theirs would
	// not break theirs: "own" is per user.
	f.snooze(t, f.other, models.NeedsSnoozeDismiss, time.Time{}, nil)
	f.comment(t, f.other, "user", needsT0)
	if f.holds(t, f.me, time.Now()) {
		t.Fatal("the other person's comment did not re-surface my dismissal")
	}
	if !f.holds(t, f.other, time.Now()) {
		t.Fatal("the other person's own comment broke their dismissal")
	}

	ok, err := f.s.UnsnoozeNeedsItem(f.me.ID, f.item.ID)
	if err != nil || !ok {
		t.Fatalf("unsnooze: %v, %v", ok, err)
	}
	if sn, _ := f.s.GetNeedsSnooze(f.me.ID, f.item.ID, time.Now()); sn != nil {
		t.Fatal("my snooze survived unsnooze")
	}
	if sn, _ := f.s.GetNeedsSnooze(f.other.ID, f.item.ID, time.Now()); sn == nil {
		t.Fatal("unsnoozing mine removed theirs")
	}
	if ok, err := f.s.UnsnoozeNeedsItem(f.me.ID, f.item.ID); err != nil || ok {
		t.Fatalf("second unsnooze: %v, %v; want false, nil", ok, err)
	}
}
