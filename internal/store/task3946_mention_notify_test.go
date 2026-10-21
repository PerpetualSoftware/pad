package store

import (
	"context"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3946: @mentions enqueue a 'mention' push notification for each NEWLY
// indexed (comment, user) pair, on create and edit only, for users who can
// see the item (binding rule 645fab20), and a mention supersedes the same
// user's pending comment/reply row (lead ruling f0fc5933).

// mentionNotifyFixture is a workspace with author A and members bob and carol,
// each with a device, and dave, a non-member with a device.
type mentionNotifyFixture struct {
	notifyFixture
	a, bob, carol, dave *models.User
}

func newMentionNotifyFixture(t *testing.T) mentionNotifyFixture {
	t.Helper()
	f := newNotifyFixture(t)
	m := mentionNotifyFixture{
		notifyFixture: f,
		a:             mentionUser(t, f.s, "authora"),
		bob:           mentionUser(t, f.s, "bobm"),
		carol:         mentionUser(t, f.s, "carolm"),
		dave:          mentionUser(t, f.s, "davem"),
	}
	f.join(t, m.a, m.bob, m.carol)
	for _, u := range []*models.User{m.a, m.bob, m.carol, m.dave} {
		addDevice(t, f.s, u.ID)
	}
	return m
}

func (m mentionNotifyFixture) count(t *testing.T, where string, args ...any) int {
	t.Helper()
	return m.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE `+where, args...)
}

// Kills: dropping the create hook or either edit hook (no row); dropping the
// author skip (A's self-mention writes a row); notifying kept mentions on
// edit (a second bob row); re-notifying a removed-then-re-added mention.
func TestNotify_MentionOnCreateAndEditAdd(t *testing.T) {
	t.Parallel()
	m := newMentionNotifyFixture(t)
	c, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "cc @bobm and me @authora"})
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "create, bob", m.kinds(t, m.bob.ID), "mention")
	wantKinds(t, "create, self-mention", m.kinds(t, m.a.ID))
	if n := m.count(t, `user_id = ? AND dedupe_key = ? AND comment_id = ? AND actor_user_id = ?`,
		m.bob.ID, "mention:"+c.ID+":"+m.bob.ID, c.ID, m.a.ID); n != 1 {
		t.Fatalf("bob's mention row by key/comment/actor = %d, want 1", n)
	}

	// The edit keeps bob and adds carol: only carol is new. Dropping
	// @authora and adding it back is a new index row for the author, who is
	// still never told about their own mention.
	if _, err := m.s.UpdateComment(c.ID, "cc @bobm"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.s.UpdateComment(c.ID, "cc @bobm and @carolm, me @authora"); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "edit, bob kept", m.kinds(t, m.bob.ID), "mention")
	wantKinds(t, "edit, carol added", m.kinds(t, m.carol.ID), "mention")
	wantKinds(t, "edit, self-mention re-added", m.kinds(t, m.a.ID))
	if n := m.count(t, `user_id = ? AND actor_user_id = ?`, m.carol.ID, m.a.ID); n != 1 {
		t.Fatalf("carol's edit mention is not attributed to the author")
	}

	// Remove bob, then re-add him: his mention row still exists, so the
	// dedupe key holds and nothing new is written (ruling f0fc5933 item 4).
	if _, err := m.s.UpdateComment(c.ID, "cc @carolm"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.s.UpdateComment(c.ID, "cc @carolm @bobm"); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "remove then re-add bob", m.kinds(t, m.bob.ID), "mention")

	// The fenced (app) doors: create and edit.
	ff := newFenceFixture(t)
	fs := ff.s
	actor, erin, finn := notifyUser(t, fs, "actor@example.com"), mentionUser(t, fs, "erinm"), mentionUser(t, fs, "finnm")
	for _, u := range []*models.User{erin, finn} {
		if err := fs.AddWorkspaceMember(ff.ws.ID, u.ID, "editor"); err != nil {
			t.Fatal(err)
		}
		addDevice(t, fs, u.ID)
	}
	item := createTestItem(t, fs, ff.ws.ID, ff.companion.ID, "Ticket", "")
	fa := FencedActor{Kind: "agent", UserID: actor.ID}
	ftx, err := fs.BeginFenced(context.Background(), ff.spec())
	if err != nil {
		t.Fatal(err)
	}
	fc, err := ftx.CreateComment(FencedCommentCreate{ItemID: item.ID, Body: "ping @erinm", Actor: fa})
	if err != nil {
		ftx.Rollback()
		t.Fatal(err)
	}
	if err := ftx.Commit(); err != nil {
		t.Fatal(err)
	}
	fn := notifyFixture{s: fs}
	wantKinds(t, "fenced create, erin", fn.kinds(t, erin.ID), "mention")
	ftx, err = fs.BeginFenced(context.Background(), ff.spec())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ftx.UpdateComment(item.ID, fc.ID, "ping @erinm @finnm", fa); err != nil {
		ftx.Rollback()
		t.Fatal(err)
	}
	if err := ftx.Commit(); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "fenced edit, erin kept", fn.kinds(t, erin.ID), "mention")
	wantKinds(t, "fenced edit, finn added", fn.kinds(t, finn.ID), "mention")
}

// Kills: enqueueing inside reindexCommentMentionsTx (the backfill would
// notify); dropping the visibility gate (dave, a non-member, gets a row).
func TestNotify_MentionNotOnBackfillOrCopy(t *testing.T) {
	t.Parallel()
	m := newMentionNotifyFixture(t)

	// A non-member is never told, on create.
	if _, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "hey @davem"}); err != nil {
		t.Fatal(err)
	}
	if n := mentionCount(t, m.s, `user_id = ?`, m.dave.ID); n != 1 {
		t.Fatalf("control: dave's mention is not indexed (%d)", n)
	}
	wantKinds(t, "non-member on create", m.kinds(t, m.dave.ID))

	// Backfill: an unindexed native comment mentioning bob is indexed by the
	// backfill and notifies nobody.
	c, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "later @bobm"})
	if err != nil {
		t.Fatal(err)
	}
	m.s.mustExec(t, `DELETE FROM comment_mentions`)
	m.s.mustExec(t, `DELETE FROM user_notifications`)
	res, err := m.s.BackfillCommentMentions()
	if err != nil || res.Skipped {
		t.Fatalf("backfill = %+v, %v; want a real pass", res, err)
	}
	wantMentions(t, m.s, c.ID, "backfill indexed bob", m.bob)
	if n := m.count(t, `1 = 1`); n != 0 {
		t.Fatalf("backfill wrote %d notifications", n)
	}

	// Tombstone: deleting a comment with a reply tombstones it and notifies
	// nobody.
	parent, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "@carolm parent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "child", ParentID: parent.ID}); err != nil {
		t.Fatal(err)
	}
	m.s.mustExec(t, `DELETE FROM user_notifications`)
	if err := m.s.DeleteComment(parent.ID); err != nil {
		t.Fatal(err)
	}
	if n := m.count(t, `1 = 1`); n != 0 {
		t.Fatalf("tombstone wrote %d notifications", n)
	}

	// Import: an imported comment is never indexed, so never notifies.
	data, err := m.s.ExportWorkspace(m.ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.s.ImportWorkspace(data, "MentionNotifyDst", "", "api"); err != nil {
		t.Fatal(err)
	}
	if n := m.count(t, `1 = 1`); n != 0 {
		t.Fatalf("import wrote %d notifications", n)
	}

	// Cross-workspace copy carries no comments, so no mentions.
	f := newCopyStorageFixture(t, "free", 100, 50)
	gail := mentionUser(t, f.s, "gailm")
	if err := f.s.AddWorkspaceMember(f.wsID, gail.ID, "editor"); err != nil {
		t.Fatal(err)
	}
	addDevice(t, f.s, gail.ID)
	if _, err := f.s.CreateComment(f.wsID, f.itemID, f.owner.ID, models.CommentCreate{Body: "@gailm"}); err != nil {
		t.Fatal(err)
	}
	// CONTROL: the source mention did notify, so the zero below is a copy
	// property.
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE kind = 'mention' AND workspace_id = ?`, f.wsID); n != 1 {
		t.Fatalf("control: source mention rows = %d, want 1", n)
	}
	if err := f.copy(false); err != nil {
		t.Fatal(err)
	}
	if n := f.s.countRows(t, `SELECT COUNT(*) FROM user_notifications WHERE workspace_id = ?`, f.dst.wsID); n != 0 {
		t.Fatalf("copy wrote %d notifications in the destination", n)
	}
}

// Kills: adding mentions after the item's people on create (bob gets a
// comment row); dropping the supersede DELETE (pending: two rows); dropping
// its push_state condition (sent: one row); running the DELETE for a
// mention that wrote nothing (non-member: the comment row is gone).
func TestNotify_MentionSupersedesComment(t *testing.T) {
	t.Parallel()
	m := newMentionNotifyFixture(t)

	// Create: bob is the assignee AND mentioned, so he gets the one mention.
	m.s.mustExec(t, `UPDATE items SET assigned_user_id = ? WHERE id = ?`, m.bob.ID, m.item.ID)
	if _, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "@bobm see this"}); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "create, assignee mentioned", m.kinds(t, m.bob.ID), "mention")

	// Edit, pending: bob's comment row is still pending, the edit mentions
	// him, and the mention replaces it.
	m.s.mustExec(t, `DELETE FROM user_notifications`)
	c, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "control: bob's comment row", m.kinds(t, m.bob.ID), "comment")
	if _, err := m.s.UpdateComment(c.ID, "plain @bobm"); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "edit supersedes pending", m.kinds(t, m.bob.ID), "mention")

	// Edit, sent: the comment row already went out, so it stays and the
	// mention is written too.
	m.s.mustExec(t, `DELETE FROM user_notifications`)
	c2, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "second"})
	if err != nil {
		t.Fatal(err)
	}
	m.s.mustExec(t, `UPDATE user_notifications SET push_state = 'sent' WHERE user_id = ?`, m.bob.ID)
	if _, err := m.s.UpdateComment(c2.ID, "second @bobm"); err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "edit after sent", m.kinds(t, m.bob.ID), "comment", "mention")

	// Edit, non-member: dave is the item's creator (so he holds a comment
	// row) but no longer a member. Mentioning him writes nothing and must
	// not delete his comment row.
	m.s.mustExec(t, `DELETE FROM user_notifications`)
	m.s.mustExec(t, `UPDATE items SET created_by_user_id = ? WHERE id = ?`, m.dave.ID, m.item.ID)
	c3, err := m.s.CreateComment(m.ws.ID, m.item.ID, m.a.ID, models.CommentCreate{Body: "third"})
	if err != nil {
		t.Fatal(err)
	}
	wantKinds(t, "control: dave's comment row", m.kinds(t, m.dave.ID), "comment")
	if _, err := m.s.UpdateComment(c3.ID, "third @davem"); err != nil {
		t.Fatal(err)
	}
	if n := mentionCount(t, m.s, `comment_id = ? AND user_id = ?`, c3.ID, m.dave.ID); n != 1 {
		t.Fatalf("control: dave's edit mention is not indexed (%d)", n)
	}
	wantKinds(t, "edit, non-member", m.kinds(t, m.dave.ID), "comment")
}
