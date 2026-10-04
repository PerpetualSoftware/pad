package appstore

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// U2b (TASK-3391): app comment writes. Every mutation runs under the write
// census, and every refusal must capture NOTHING.

// allowedAppCommentWrite is DOC-3371 §4's allowed-writes table for comment
// writes: the comment (and, on a delete, its reactions), the "commented"
// activity, the event and the decision job. No attachment row is written: an
// app comment's references are checked, not stamped (store.checkItemAttachmentRefs).
// On SQLite the FTS5 shadow tables of comments_fts are observed by name.
func allowedAppCommentWrite(w storetest.Write) bool {
	switch w.Table {
	case "comments", "activities", "event_outbox", "decision_jobs":
		return true
	case "comment_reactions":
		return w.Op == "DELETE"
	}
	return strings.HasPrefix(w.Table, "comments_fts_")
}

func assertOnlyAllowedCommentWrites(t *testing.T, writes []storetest.Write) {
	t.Helper()
	for _, w := range writes {
		if !allowedAppCommentWrite(w) {
			t.Errorf("app comment write touched %s (%s), which the allowed-writes table does not permit", w.Table, w.Op)
		}
	}
}

type commentFixture struct {
	appFixture
	mine, other *models.Item // two app items in the companion collection
	hidden      *models.Item // a human item in a non-companion collection
}

func newCommentFixture(t *testing.T) commentFixture {
	t.Helper()
	f := newAppFixture(t, Options{})
	ctx := context.Background()
	mine, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Mine"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	other, err := f.a.CreateItem(ctx, f.spec, f.companion.ID, AppItemCreate{Title: "Other"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := f.s.CreateItem(f.ws.ID, f.private.ID, models.ItemCreate{Title: "Hidden"})
	if err != nil {
		t.Fatal(err)
	}
	return commentFixture{appFixture: f, mine: mine, other: other, hidden: hidden}
}

// attachment inserts an attachment row bound to itemID ("" = unbound),
// optionally a variant of parentID.
func (f commentFixture) attachment(t *testing.T, itemID, parentID string) string {
	t.Helper()
	a := &models.Attachment{
		ID: uuid.NewString(), WorkspaceID: f.ws.ID, UploadedBy: f.owner.ID,
		StorageKey: "fs:" + uuid.NewString(), ContentHash: uuid.NewString(), MimeType: "image/png",
		SizeBytes: 1, Filename: "a.png", FilenameSource: "caller",
	}
	if itemID != "" {
		a.ItemID = &itemID
	}
	if parentID != "" {
		v := "thumb-sm"
		a.ParentID, a.Variant = &parentID, &v
	}
	if err := f.s.CreateAttachment(a); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func (f commentFixture) stamped(t *testing.T, id string) bool {
	t.Helper()
	var at sql.NullString
	if err := f.s.DB().QueryRow(f.s.D().Rebind(`SELECT last_referenced_at FROM attachments WHERE id = ?`), id).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at.Valid && at.String != ""
}

func ref(id string) string { return "![x](pad-attachment:" + id + ")" }

func TestAppCreateComment_WritesOnlyWhatTheSpecAllows(t *testing.T) {
	f := newCommentFixture(t)
	onItem := f.attachment(t, f.mine.ID, "")
	variant := f.attachment(t, f.mine.ID, onItem)
	onOther := f.attachment(t, f.other.ID, "")

	var c *models.Comment
	writes := storetest.CaptureWrites(t, f.s, func() {
		var err error
		c, err = f.a.CreateComment(context.Background(), f.spec, f.mine.ID, AppCommentCreate{Body: "Looking into it " + ref(onItem), Author: "Helpdesk"}, f.actor)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	assertOnlyAllowedCommentWrites(t, writes)
	tables := storetest.Tables(writes)
	for _, want := range []string{"comments", "activities", "event_outbox"} {
		if !contains(tables, want) {
			t.Errorf("create did not write %s: %v", want, tables)
		}
	}

	var source, createdBy, viaApp, userID, activityID, author string
	if err := f.s.DB().QueryRow(f.s.D().Rebind(`SELECT source, created_by, via_app, user_id, activity_id, author FROM comments WHERE id = ?`), c.ID).
		Scan(&source, &createdBy, &viaApp, &userID, &activityID, &author); err != nil {
		t.Fatal(err)
	}
	if source != "app" || createdBy != "user" || viaApp != f.spec.InstallID || userID != f.owner.ID || author != "Helpdesk" {
		t.Errorf("comment attribution: source=%s created_by=%s via_app=%s user=%s author=%s", source, createdBy, viaApp, userID, author)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM activities WHERE id = ? AND action = 'commented' AND via_app = ? AND document_id = ?`, activityID, f.spec.InstallID, f.mine.ID); n != 1 {
		t.Errorf("the comment's activity is not the app's commented row: %d", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'comment.created' AND subject_id = ?`, c.ID); n != 1 {
		t.Errorf("comment.created events: %d", n)
	}
	for _, id := range []string{onItem, variant, onOther} {
		if f.stamped(t, id) {
			t.Errorf("attachment %s was stamped; an app comment checks references and writes nothing to attachments", id)
		}
	}
}

func TestAppUpdateAndDeleteComment_WriteOnlyWhatTheSpecAllows(t *testing.T) {
	f := newCommentFixture(t)
	ctx := context.Background()
	parent, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "parent"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	reply, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "reply", ParentID: parent.ID}, f.actor)
	if err != nil {
		t.Fatal(err)
	}

	writes := storetest.CaptureWrites(t, f.s, func() {
		if _, err := f.a.UpdateComment(ctx, f.spec, f.mine.ID, reply.ID, "reply, edited", f.actor); err != nil {
			t.Fatalf("update: %v", err)
		}
	})
	assertOnlyAllowedCommentWrites(t, writes)
	if n := f.count(t, `SELECT COUNT(*) FROM event_outbox WHERE event_type = 'comment.updated' AND subject_id = ?`, reply.ID); n != 1 {
		t.Errorf("comment.updated events: %d", n)
	}

	// The parent has a reply: a delete tombstones it.
	writes = storetest.CaptureWrites(t, f.s, func() {
		if err := f.a.DeleteComment(ctx, f.spec, f.mine.ID, parent.ID, f.actor); err != nil {
			t.Fatalf("delete parent: %v", err)
		}
	})
	assertOnlyAllowedCommentWrites(t, writes)
	if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE id = ? AND deleted_at IS NOT NULL AND body = ''`, parent.ID); n != 1 {
		t.Fatal("the parent with a reply was not tombstoned")
	}

	// Deleting the last reply removes it and reaps the emptied tombstone.
	writes = storetest.CaptureWrites(t, f.s, func() {
		if err := f.a.DeleteComment(ctx, f.spec, f.mine.ID, reply.ID, f.actor); err != nil {
			t.Fatalf("delete reply: %v", err)
		}
	})
	assertOnlyAllowedCommentWrites(t, writes)
	if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE id IN (?, ?)`, parent.ID, reply.ID); n != 0 {
		t.Fatalf("reply and its tombstone should both be gone, %d left", n)
	}
	// The reply's deletion event carries the projection frozen before the
	// reap: its parent, on the same item, is still named.
	var payload string
	if err := f.s.DB().QueryRow(f.s.D().Rebind(`SELECT payload FROM event_outbox WHERE event_type = 'comment.deleted' AND subject_id = ?`), reply.ID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, parent.ID) || !strings.Contains(payload, f.mine.ID) {
		t.Errorf("comment.deleted payload lost its projection: %s", payload)
	}
}

func TestAppCommentRefusalsWriteNothing(t *testing.T) {
	f := newCommentFixture(t)
	ctx := context.Background()

	onItem := f.attachment(t, f.mine.ID, "")
	onOther := f.attachment(t, f.other.ID, "")
	unbound := f.attachment(t, "", "")
	gone := f.attachment(t, f.mine.ID, "")
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`UPDATE attachments SET deleted_at = ? WHERE id = ?`), time.Now().UTC().Format(time.RFC3339), gone); err != nil {
		t.Fatal(err)
	}

	mine, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "mine"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	onOtherItem, err := f.a.CreateComment(ctx, f.spec, f.other.ID, AppCommentCreate{Body: "on the other item"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	human, err := f.s.CreateComment(f.ws.ID, f.mine.ID, f.owner.ID, models.CommentCreate{Body: "a human wrote this", CreatedBy: "user", Source: "web"})
	if err != nil {
		t.Fatal(err)
	}
	// Another install's comment on this item, by the same user: seeded as a
	// row, since no install can write through a collection stamped with
	// another's via_app (the fence checks the stamp live, TASK-3401 U6c). It
	// is reachable as history: an item moved here, or a re-stamped companion.
	otherInstall := insertInstall(t, f.s, f.ws.ID)
	theirsID := uuid.NewString()
	tsTheirs := time.Now().UTC().Format(time.RFC3339)
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`INSERT INTO comments (id, item_id, workspace_id, author, user_id, body, created_by, source, created_at, updated_at, via_app)
		VALUES (?, ?, ?, 'a', ?, 'another install', 'user', 'app', ?, ?, ?)`),
		theirsID, f.mine.ID, f.ws.ID, f.actor.UserID, tsTheirs, tsTheirs, otherInstall); err != nil {
		t.Fatal(err)
	}
	theirs := &models.Comment{ID: theirsID}
	user2, err := f.s.CreateUser(models.UserCreate{Email: "u2-" + uuid.NewString()[:8] + "@example.com", Name: "Two", Password: "password123"})
	if err != nil {
		t.Fatal(err)
	}
	actor2 := store.FencedActor{Kind: "user", UserID: user2.ID}
	byUser2, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "same install, other user"}, actor2)
	if err != nil {
		t.Fatal(err)
	}
	tomb, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "to be tombstoned"}, f.actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "keeps it", ParentID: tomb.ID}, f.actor); err != nil {
		t.Fatal(err)
	}
	if err := f.a.DeleteComment(ctx, f.spec, f.mine.ID, tomb.ID, f.actor); err != nil {
		t.Fatal(err)
	}
	// A legacy thread that crosses items (written before BUG-3346): a
	// comment on mine whose parent is on the other item.
	crossed := uuid.NewString()
	ts := time.Now().UTC().Format(time.RFC3339)
	if _, err := f.s.DB().Exec(f.s.D().Rebind(`INSERT INTO comments (id, item_id, workspace_id, author, user_id, body, created_by, source, parent_id, created_at, updated_at, via_app)
		VALUES (?, ?, ?, 'a', ?, 'crossed', 'user', 'app', ?, ?, ?, ?)`),
		crossed, f.mine.ID, f.ws.ID, f.owner.ID, onOtherItem.ID, ts, ts, f.spec.InstallID); err != nil {
		t.Fatal(err)
	}
	stale := f.spec
	stale.Epoch = 2
	// 400 distinct bound attachments fill the check's first chunk, so the
	// foreign ref after them lands in the second.
	var many strings.Builder
	for i := 0; i < 400; i++ {
		many.WriteString(ref(f.attachment(t, f.mine.ID, "")))
	}
	manyRefs := many.String()

	is := func(target error) func(error) bool { return func(err error) bool { return errors.Is(err, target) } }
	create := func(spec store.FenceSpec, itemID string, in AppCommentCreate) func() error {
		return func() error { _, err := f.a.CreateComment(ctx, spec, itemID, in, f.actor); return err }
	}
	update := func(itemID, commentID, body string, actor store.FencedActor) func() error {
		return func() error { _, err := f.a.UpdateComment(ctx, f.spec, itemID, commentID, body, actor); return err }
	}
	del := func(itemID, commentID string, actor store.FencedActor) func() error {
		return func() error { return f.a.DeleteComment(ctx, f.spec, itemID, commentID, actor) }
	}

	for name, tc := range map[string]struct {
		run  func() error
		want func(error) bool
	}{
		"stale epoch":                {create(stale, f.mine.ID, AppCommentCreate{Body: "x"}), is(store.ErrFenceStale)},
		"non-companion item":         {create(f.spec, f.hidden.ID, AppCommentCreate{Body: "x"}), is(store.ErrNotCompanion)},
		"unknown item":               {create(f.spec, uuid.NewString(), AppCommentCreate{Body: "x"}), is(store.ErrNotCompanion)},
		"blank body":                 {create(f.spec, f.mine.ID, AppCommentCreate{Body: "  \n"}), IsInputError},
		"missing parent":             {create(f.spec, f.mine.ID, AppCommentCreate{Body: "x", ParentID: uuid.NewString()}), is(store.ErrAppCommentNotFound)},
		"parent on another item":     {create(f.spec, f.mine.ID, AppCommentCreate{Body: "x", ParentID: onOtherItem.ID}), is(store.ErrAppCommentNotFound)},
		"tombstoned parent":          {create(f.spec, f.mine.ID, AppCommentCreate{Body: "x", ParentID: tomb.ID}), is(store.ErrCommentDeleted)},
		"attachment on another item": {create(f.spec, f.mine.ID, AppCommentCreate{Body: ref(onOther)}), is(store.ErrAppAttachmentUnavailable)},
		"unbound attachment":         {create(f.spec, f.mine.ID, AppCommentCreate{Body: ref(unbound)}), is(store.ErrAppAttachmentUnavailable)},
		"unknown attachment":         {create(f.spec, f.mine.ID, AppCommentCreate{Body: ref(uuid.NewString())}), is(store.ErrAppAttachmentUnavailable)},
		"deleted attachment":         {create(f.spec, f.mine.ID, AppCommentCreate{Body: ref(gone)}), is(store.ErrAppAttachmentUnavailable)},
		"one good and one foreign":   {create(f.spec, f.mine.ID, AppCommentCreate{Body: ref(onItem) + ref(onOther)}), is(store.ErrAppAttachmentUnavailable)},
		"a full chunk of good refs, then a foreign one": {create(f.spec, f.mine.ID, AppCommentCreate{Body: manyRefs + ref(onOther)}), is(store.ErrAppAttachmentUnavailable)},
		"foreign attachment, odd spelling":              {create(f.spec, f.mine.ID, AppCommentCreate{Body: "PAD-Attachment:" + onOther}), is(store.ErrAppAttachmentUnavailable)},
		"bad actor": {func() error {
			_, err := f.a.CreateComment(ctx, f.spec, f.mine.ID, AppCommentCreate{Body: "x"}, store.FencedActor{Kind: "system", UserID: f.owner.ID})
			return err
		}, is(store.ErrAppBadActor)},

		"update: comment on another item":    {update(f.mine.ID, onOtherItem.ID, "x", f.actor), is(store.ErrAppCommentNotFound)},
		"update: unknown comment":            {update(f.mine.ID, uuid.NewString(), "x", f.actor), is(store.ErrAppCommentNotFound)},
		"update: a human's comment":          {update(f.mine.ID, human.ID, "x", f.actor), is(store.ErrAppNotCommentAuthor)},
		"update: another install's comment":  {update(f.mine.ID, theirs.ID, "x", f.actor), is(store.ErrAppNotCommentAuthor)},
		"update: another user's app comment": {update(f.mine.ID, byUser2.ID, "x", f.actor), is(store.ErrAppNotCommentAuthor)},
		"update: tombstone":                  {update(f.mine.ID, tomb.ID, "x", f.actor), is(store.ErrCommentDeleted)},
		"update: foreign attachment":         {update(f.mine.ID, mine.ID, ref(onOther), f.actor), is(store.ErrAppAttachmentUnavailable)},
		"update: blank body":                 {update(f.mine.ID, mine.ID, "", f.actor), IsInputError},
		"update: non-companion item":         {update(f.hidden.ID, mine.ID, "x", f.actor), is(store.ErrNotCompanion)},

		"delete: comment on another item":    {del(f.mine.ID, onOtherItem.ID, f.actor), is(store.ErrAppCommentNotFound)},
		"delete: a human's comment":          {del(f.mine.ID, human.ID, f.actor), is(store.ErrAppNotCommentAuthor)},
		"delete: another install's comment":  {del(f.mine.ID, theirs.ID, f.actor), is(store.ErrAppNotCommentAuthor)},
		"delete: another user's app comment": {del(f.mine.ID, byUser2.ID, f.actor), is(store.ErrAppNotCommentAuthor)},
		"delete: tombstone":                  {del(f.mine.ID, tomb.ID, f.actor), is(store.ErrAppCommentNotFound)},
		"delete: thread crossing items":      {del(f.mine.ID, crossed, f.actor), is(store.ErrAppCrossItemThread)},
		"delete: stale epoch": {func() error {
			return f.a.DeleteComment(ctx, stale, f.mine.ID, mine.ID, f.actor)
		}, is(store.ErrFenceStale)},
	} {
		t.Run(name, func(t *testing.T) {
			var got error
			writes := storetest.CaptureWrites(t, f.s, func() { got = tc.run() })
			if got == nil || !tc.want(got) {
				t.Fatalf("got %v", got)
			}
			if len(writes) != 0 {
				t.Fatalf("a refusal wrote: %v", writes)
			}
		})
	}
	if f.stamped(t, onItem) {
		t.Error("a refused write stamped an attachment it was allowed to reference")
	}
}

// A human move of the item into a private collection, committing while an
// app comment write is in flight, must not land under it: the app write
// waits on the workspace seq lock every item move takes, then sees the move
// and refuses (codex round 3). One case per method.
func TestAppComment_ConcurrentMoveOutOfCompanionsRefuses(t *testing.T) {
	for name, run := range map[string]func(f commentFixture, c *models.Comment) error{
		"create": func(f commentFixture, _ *models.Comment) error {
			_, err := f.a.CreateComment(context.Background(), f.spec, f.mine.ID, AppCommentCreate{Body: "x"}, f.actor)
			return err
		},
		"update": func(f commentFixture, c *models.Comment) error {
			_, err := f.a.UpdateComment(context.Background(), f.spec, f.mine.ID, c.ID, "edited", f.actor)
			return err
		},
		"delete": func(f commentFixture, c *models.Comment) error {
			return f.a.DeleteComment(context.Background(), f.spec, f.mine.ID, c.ID, f.actor)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newCommentFixture(t)
			if f.s.D().Driver() != store.DriverPostgres {
				t.Skip("SQLite serializes writers at BEGIN")
			}
			c, err := f.a.CreateComment(context.Background(), f.spec, f.mine.ID, AppCommentCreate{Body: "before"}, f.actor)
			if err != nil {
				t.Fatal(err)
			}
			// The human move, held open: the seq lock MoveItem takes, then the
			// collection change.
			human, err := f.s.DB().Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = human.Rollback() }()
			if _, err := human.Exec(`SELECT pg_advisory_xact_lock(hashtext($1))`, f.ws.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := human.Exec(`UPDATE items SET collection_id = $1 WHERE id = $2`, f.private.ID, f.mine.ID); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- run(f, c) }()
			deadline := time.Now().Add(10 * time.Second)
			for {
				select {
				case err := <-done:
					t.Fatalf("the app write finished while the move was uncommitted: %v", err)
				default:
				}
				var waiting int
				if err := f.s.DB().QueryRow(`SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the app write never waited on the move")
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err := human.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, store.ErrNotCompanion) {
				t.Fatalf("got %v, want ErrNotCompanion", err)
			}
			if n := f.count(t, `SELECT COUNT(*) FROM comments WHERE item_id = ?`, f.mine.ID); n != 1 {
				t.Errorf("comments on the moved item: %d, want the 1 written before the move", n)
			}
		})
	}
}
