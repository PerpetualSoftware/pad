package store

import (
	"context"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func (f notifyFixture) kinds(t *testing.T, userID string) []string {
	t.Helper()
	rows, err := f.s.db.Query(f.s.q(`SELECT kind FROM user_notifications WHERE user_id = ? ORDER BY kind`), userID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out = append(out, k)
	}
	return out
}

func wantKinds(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: kinds = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: kinds = %v, want %v", what, got, want)
		}
	}
}

// Assigning to someone else enqueues for them; assigning yourself does not.
// Kills: dropping the UpdateItem hook (no row); dropping the `!= actor` check
// (a self row appears).
func TestNotify_AssignByOtherEnqueues_SelfAssignDoesNot(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	addDevice(t, f.s, a.ID)
	addDevice(t, f.s, b.ID)

	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &b.ID, ActorUserID: a.ID}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "A assigns B", f.kinds(t, b.ID), "assigned")

	item2 := createTestItem(t, f.s, f.ws.ID, f.coll.ID, "Second", "")
	if _, err := f.s.UpdateItem(item2.ID, models.ItemUpdate{AssignedUserID: &a.ID, ActorUserID: a.ID}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "A assigns A", f.kinds(t, a.ID))

	// An empty actor (a system writer) is not "self".
	item3 := createTestItem(t, f.s, f.ws.ID, f.coll.ID, "Third", "")
	if _, err := f.s.UpdateItem(item3.ID, models.ItemUpdate{AssignedUserID: &a.ID}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "system assigns A", f.kinds(t, a.ID), "assigned")
}

// Kills: dropping the createItemTxWithID hook.
func TestNotify_CreateWithAssigneeEnqueues(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	addDevice(t, f.s, a.ID)
	addDevice(t, f.s, b.ID)

	if _, err := f.s.CreateItem(f.ws.ID, f.coll.ID, models.ItemCreate{
		Title: "Born assigned", Fields: `{"status":"open"}`, AssignedUserID: &b.ID, ActorUserID: a.ID,
	}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "create assigned to B", f.kinds(t, b.ID), "assigned")
	if _, err := f.s.CreateItem(f.ws.ID, f.coll.ID, models.ItemCreate{
		Title: "Born mine", Fields: `{"status":"open"}`, AssignedUserID: &a.ID, ActorUserID: a.ID,
	}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "create assigned to self", f.kinds(t, a.ID))
}

// A user with no device accumulates nothing. Kills: removing the
// WHERE EXISTS (push_devices) gate.
func TestNotify_NoDeviceNoRow(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	if _, err := f.s.UpdateItem(f.item.ID, models.ItemUpdate{AssignedUserID: &b.ID, ActorUserID: a.ID}); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications`); n != 0 {
		t.Fatalf("%d rows for a user with no device", n)
	}
}

// The row commits with the write or not at all. Kills: writing the row on a
// connection outside the caller's transaction.
func TestNotify_RollbackDropsRow(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	addDevice(t, f.s, b.ID)
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.enqueueAssignedNotificationTx(tx, f.item, b.ID, a.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications`); n != 0 {
		t.Fatalf("%d rows survived a rollback", n)
	}
}

func TestNotify_CommentRecipients(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	// creator C, assignee D, parent author E, comment author A.
	c, d, e, a := notifyUser(t, f.s, "c@example.com"), notifyUser(t, f.s, "d@example.com"),
		notifyUser(t, f.s, "e@example.com"), notifyUser(t, f.s, "a@example.com")
	f.s.mustExec(t, `UPDATE items SET created_by_user_id = ?, assigned_user_id = ? WHERE id = ?`, c.ID, d.ID, f.item.ID)
	for _, u := range []*models.User{c, d, e, a} {
		addDevice(t, f.s, u.ID)
	}
	parent, err := f.s.CreateComment(f.ws.ID, f.item.ID, e.ID, models.CommentCreate{Body: "first"})
	if err != nil {
		t.Fatal(err)
	}
	// E's own top-level comment notified C and D, not E.
	wantKinds(t, "E top-level, C", f.kinds(t, c.ID), "comment")
	wantKinds(t, "E top-level, E", f.kinds(t, e.ID))
	f.s.mustExec(t, `DELETE FROM user_notifications`)

	if _, err := f.s.CreateComment(f.ws.ID, f.item.ID, a.ID, models.CommentCreate{Body: "re", ParentID: parent.ID}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "reply, creator", f.kinds(t, c.ID), "comment")
	wantKinds(t, "reply, assignee", f.kinds(t, d.ID), "comment")
	wantKinds(t, "reply, parent author", f.kinds(t, e.ID), "reply")
	wantKinds(t, "reply, author", f.kinds(t, a.ID))

	// The assignee commenting on their own item is told nothing.
	f.s.mustExec(t, `DELETE FROM user_notifications`)
	if _, err := f.s.CreateComment(f.ws.ID, f.item.ID, d.ID, models.CommentCreate{Body: "mine"}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "assignee's own comment", f.kinds(t, d.ID))

	// A person who is both the parent's author and the assignee gets one
	// "reply", not a reply and a comment.
	f.s.mustExec(t, `DELETE FROM user_notifications`)
	f.s.mustExec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, e.ID, f.item.ID)
	if _, err := f.s.CreateComment(f.ws.ID, f.item.ID, a.ID, models.CommentCreate{Body: "again", ParentID: parent.ID}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "reply, parent author who is assignee", f.kinds(t, e.ID), "reply")
}

// The fenced (app) comment path enqueues too. Kills: dropping the fenced hook.
func TestNotify_FencedCommentEnqueues(t *testing.T) {
	t.Parallel()
	ff := newFenceFixture(t)
	s := ff.s
	owner, actor := notifyUser(t, s, "owner@example.com"), notifyUser(t, s, "actor@example.com")
	item := createTestItem(t, s, ff.ws.ID, ff.companion.ID, "Ticket", "")
	s.mustExec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, owner.ID, item.ID)
	addDevice(t, s, owner.ID)
	addDevice(t, s, actor.ID)

	ftx, err := s.BeginFenced(context.Background(), ff.spec())
	if err != nil {
		t.Fatal(err)
	}
	defer ftx.Rollback()
	if _, err := ftx.CreateComment(FencedCommentCreate{ItemID: item.ID, Body: "from the app", Actor: FencedActor{Kind: "agent", UserID: actor.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := ftx.Commit(); err != nil {
		t.Fatal(err)
	}
	f := notifyFixture{s: s}
	wantKinds(t, "fenced comment, assignee", f.kinds(t, owner.ID), "comment")
	wantKinds(t, "fenced comment, author", f.kinds(t, actor.ID))
	if n := s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE actor_is_agent`+agentTrue(s)); n != 1 {
		t.Fatalf("agent-authored rows = %d, want 1", n)
	}
}

func agentTrue(s *Store) string {
	if s.dialect.Driver() == DriverPostgres {
		return " = TRUE"
	}
	return " = 1"
}

// Kills: firing without the hook; inverting assignee-else-creator; dropping
// fired_at from the key.
func TestNotify_ReminderRecipient(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	creator, assignee := notifyUser(t, f.s, "creator@example.com"), notifyUser(t, f.s, "assignee@example.com")
	addDevice(t, f.s, creator.ID)
	addDevice(t, f.s, assignee.ID)
	f.s.mustExec(t, `UPDATE items SET created_by_user_id = ? WHERE id = ?`, creator.ID, f.item.ID)

	id := armReminder(t, f.s, f.ws.ID, f.item.ID, past)
	if _, err := f.s.FireDueReminders(nowTS(), 0); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "unassigned, creator", f.kinds(t, creator.ID), "reminder")
	wantKinds(t, "unassigned, assignee", f.kinds(t, assignee.ID))

	refire := func() {
		t.Helper()
		if _, err := f.s.RearmReminder(f.ws.ID, id, past); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // fired_at is whole seconds
		if _, err := f.s.FireDueReminders(nowTS(), 0); err != nil {
			t.Fatal(err)
		}
	}
	// A re-armed reminder that fires again is a new notification.
	refire()
	wantKinds(t, "re-fired, creator", f.kinds(t, creator.ID), "reminder", "reminder")

	f.s.mustExec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, assignee.ID, f.item.ID)
	refire()
	wantKinds(t, "assigned, assignee", f.kinds(t, assignee.ID), "reminder")
	wantKinds(t, "assigned, creator unchanged", f.kinds(t, creator.ID), "reminder", "reminder")
}

// Kills: removing ON CONFLICT (an error) or the unique index.
func TestNotify_DedupeKeyIdempotent(t *testing.T) {
	t.Parallel()
	f := newNotifyFixture(t)
	a, b := notifyUser(t, f.s, "a@example.com"), notifyUser(t, f.s, "b@example.com")
	f.join(t, a, b)
	addDevice(t, f.s, b.ID)
	for i := 0; i < 2; i++ {
		tx, err := f.s.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.enqueueAssignedNotificationTx(tx, f.item, b.ID, a.ID, false); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	wantKinds(t, "same fact twice", f.kinds(t, b.ID), "assigned")
}

// A claim keeps the user row for a new person, so the old holder's devices,
// queued pushes and settings must not follow. The devices also die with the
// sessions the claim deletes (FK cascade), so the test asserts the outcome
// (no rows after the claim) whichever line does the work; the explicit
// push_devices delete is a known mutation survivor for that reason.
// Kills: dropping the queue delete, dropping the settings delete.
func TestNotify_ClaimEmptiesDevicesQueueAndSettings(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Claim")
	coll := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "")
	squatter := createUnverifiedUser(t, s, "squat@example.com")
	other := notifyUser(t, s, "other@example.com")
	for _, u := range []*models.User{squatter, other} {
		if err := s.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
			t.Fatal(err)
		}
	}
	addDevice(t, s, squatter.ID)
	if err := s.PutNotificationSettings(squatter.ID, models.NotificationSettings{NotifyAssigned: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{AssignedUserID: &squatter.ID, ActorUserID: other.ID}); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"push_devices", "user_notifications", "user_notification_settings"} {
		if n := s.countRows(t, `SELECT COUNT(*) FROM `+tbl+` WHERE user_id = ?`, squatter.ID); n != 1 {
			t.Fatalf("fixture: %s has %d rows for the user, want 1", tbl, n)
		}
	}
	if _, err := s.ClaimAccountByProvider(squatter.ID, "google", "", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, tbl := range []string{"push_devices", "user_notifications", "user_notification_settings"} {
		if n := s.countRows(t, `SELECT COUNT(*) FROM `+tbl+` WHERE user_id = ?`, squatter.ID); n != 0 {
			t.Errorf("%s has %d rows for the user after a claim, want 0", tbl, n)
		}
	}
}
