package store

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3917 (IDEA-3906): comment_mentions is a derived index of which humans a
// live comment @-mentions. These tests pin the lifecycle, the population of
// writers, and the import and backfill doors.

func mentionUser(t *testing.T, s *Store, username string) *models.User {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Email: username + "@example.com", Username: username, Name: username, Password: "password-1234"})
	if err != nil {
		t.Fatalf("CreateUser %s: %v", username, err)
	}
	return u
}

// mentionRows returns user_id -> created_at for a comment.
func mentionRows(t *testing.T, s *Store, commentID string) map[string]string {
	t.Helper()
	rows, err := s.db.Query(s.q(`SELECT user_id, created_at FROM comment_mentions WHERE comment_id = ?`), commentID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var u, c string
		if err := rows.Scan(&u, &c); err != nil {
			t.Fatal(err)
		}
		out[u] = c
	}
	return out
}

func mentionCount(t *testing.T, s *Store, where string, args ...any) int {
	t.Helper()
	return task3394Count(t, s, `SELECT COUNT(*) FROM comment_mentions WHERE `+where, args...)
}

func wantMentions(t *testing.T, s *Store, commentID, label string, want ...*models.User) {
	t.Helper()
	got := mentionRows(t, s, commentID)
	if len(got) != len(want) {
		t.Fatalf("%s: %d mention rows %v, want %d", label, len(got), got, len(want))
	}
	for _, u := range want {
		if _, ok := got[u.ID]; !ok {
			t.Fatalf("%s: no row for %s (%v)", label, u.Username, got)
		}
	}
}

func TestCommentMentions_IndexLifecycle(t *testing.T) {
	f := task3408Fixture(t, "inst-mention")
	s := f.s
	alice := mentionUser(t, s, "alice")
	bob := mentionUser(t, s, "bob")
	author := mentionUser(t, s, "author")
	col := createTestCollection(t, s, f.ws.ID, "Mentions")
	item := createTestItem(t, s, f.ws.ID, col.ID, "Thread", "")

	c, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{
		Body: "hi @alice and @Bob, @ghost-nobody too, and @" + f.bot.Username + " and mail me@alice.example",
	})
	if err != nil {
		t.Fatal(err)
	}
	// alice and bob resolve; ghost-nobody is unknown; the bot is an app
	// principal; me@alice.example is an email.
	wantMentions(t, s, c.ID, "create", alice, bob)

	// Edit: drop bob, keep alice. alice's row keeps its created_at.
	before := mentionRows(t, s, c.ID)[alice.ID]
	if _, err := s.db.Exec(s.q(`UPDATE comment_mentions SET created_at = '2001-01-01T00:00:00Z' WHERE comment_id = ?`), c.ID); err != nil {
		t.Fatal(err)
	}
	_ = before
	if _, err := s.UpdateComment(c.ID, "only @alice now"); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c.ID, "edit remove", alice)
	if got := mentionRows(t, s, c.ID)[alice.ID]; got != "2001-01-01T00:00:00Z" {
		t.Fatalf("a kept mention's created_at moved to %q on edit", got)
	}
	// Edit: add bob back.
	if _, err := s.UpdateComment(c.ID, "@alice and @bob"); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c.ID, "edit add", alice, bob)

	// A reply keeps the tombstone path: delete the parent while a reply stands.
	reply, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{Body: "re @bob", ParentID: c.ID})
	if err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, reply.ID, "reply", bob)
	if err := s.DeleteComment(c.ID); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c.ID, "tombstone clears")
	wantMentions(t, s, reply.ID, "reply survives the parent's tombstone", bob)

	// Hard delete cascades; the reap of the emptied tombstone does too.
	if err := s.DeleteComment(reply.ID); err != nil {
		t.Fatal(err)
	}
	if n := mentionCount(t, s, `item_id = ?`, item.ID); n != 0 {
		t.Fatalf("%d rows left after the thread was deleted", n)
	}

	// Hard delete of a live comment with mentions, directly.
	c2, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{Body: "@alice"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteComment(c2.ID); err != nil {
		t.Fatal(err)
	}
	if n := mentionCount(t, s, `comment_id = ?`, c2.ID); n != 0 {
		t.Fatalf("hard delete left %d rows", n)
	}

	// Deleting the mentioned user cascades their rows.
	c3, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{Body: "@bob"})
	if err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c3.ID, "pre-user-delete", bob)
	if _, err := s.db.Exec(s.q(`DELETE FROM comment_mentions WHERE user_id = ?`), alice.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.eraseUserTx(tx, bob.ID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c3.ID, "mentioned user erased")

	// The fenced (app) doors index too.
	var epoch int64
	if err := s.db.QueryRow(s.q(`SELECT auth_epoch FROM app_installs WHERE id = ?`), f.installID).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	actor := FencedActor{Kind: "user", UserID: f.bot.ID}
	fenced := func(fn func(*FencedTx)) {
		t.Helper()
		ft, err := s.BeginFenced(context.Background(), FenceSpec{InstallID: f.installID, WorkspaceID: f.ws.ID, Epoch: epoch, Companions: []string{f.companion.ID}})
		if err != nil {
			t.Fatal(err)
		}
		fn(ft)
		if err := ft.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	var ticket *models.Item
	fenced(func(ft *FencedTx) {
		var err error
		ticket, err = ft.CreateItem(FencedItemCreate{CollectionID: f.companion.ID, Title: "Ticket", Fields: `{"status":"open"}`, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
	})
	var fc *models.Comment
	fenced(func(ft *FencedTx) {
		var err error
		fc, err = ft.CreateComment(FencedCommentCreate{ItemID: ticket.ID, Body: "ping @alice", Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
	})
	wantMentions(t, s, fc.ID, "fenced create", alice)
	fenced(func(ft *FencedTx) {
		if _, err := ft.UpdateComment(ticket.ID, fc.ID, "ping @author", actor); err != nil {
			t.Fatal(err)
		}
	})
	wantMentions(t, s, fc.ID, "fenced edit", author)
	// A reply makes the parent's delete a tombstone.
	var fr *models.Comment
	fenced(func(ft *FencedTx) {
		var err error
		fr, err = ft.CreateComment(FencedCommentCreate{ItemID: ticket.ID, Body: "x", ParentID: fc.ID, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
	})
	fenced(func(ft *FencedTx) {
		if err := ft.DeleteComment(ticket.ID, fc.ID, actor); err != nil {
			t.Fatal(err)
		}
	})
	_ = fr
	wantMentions(t, s, fc.ID, "fenced tombstone clears")
}

// classification of every statement that writes comments.
var commentWriteSites = map[string]string{
	"comments.go:createCommentTx":                         "INDEXED: insert",
	"comments.go:UpdateComment":                           "INDEXED: edit",
	"comments.go:DeleteComment":                           "INDEXED: tombstone; the hard DELETE cascades on comment_id",
	"comments.go:reapTombstonesTx":                        "CASCADE: deletes emptied tombstones, which have no rows (the tombstone write already cleared them)",
	"fenced_comments.go:CreateComment":                    "INDEXED: an app's insert",
	"fenced_comments.go:UpdateComment":                    "INDEXED: an app's edit",
	"fenced_comments.go:DeleteComment":                    "INDEXED: an app's tombstone; the hard DELETE cascades on comment_id",
	"export.go:importWorkspace":                           "INDEXED: restore/import, under the new comment id",
	"attachments.go:RemapAttachmentReferencesInWorkspace": "BYPASS: rewrites only pad-attachment ids inside the body; it cannot add or remove an @-mention",
	"users.go:eraseUserTx":                                "BYPASS: nulls comments.user_id (the author); mentions are keyed on the MENTIONED user, whose rows cascade when the user row goes",
}

var commentWriteRe = regexp.MustCompile("(?i)(INSERT INTO comments\\b|UPDATE comments\\b|DELETE FROM comments\\b)")

// A var-table entry rather than a func: classified by the table's file.
var commentWriteTableSites = map[string]string{
	"workspace_purge.go": "CASCADE: DELETE FROM comments in the purge table; the comment_id FK removes the rows (and 'detach comment parents' only nulls parent_id)",
}

func TestCommentMentions_InsertSiteGuard(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	funcRe := regexp.MustCompile(`(?m)^func (?:\([^)]*\) )?(\w+)`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		funcs := funcRe.FindAllStringSubmatchIndex(src, -1)
		for _, m := range commentWriteRe.FindAllStringIndex(src, -1) {
			// Enclosing func: the last declaration before the match.
			name, start, end := "", 0, len(src)
			for i, fm := range funcs {
				if fm[0] > m[0] {
					break
				}
				name, start = src[fm[2]:fm[3]], fm[0]
				end = len(src)
				if i+1 < len(funcs) {
					end = funcs[i+1][0]
				}
			}
			if _, ok := commentWriteTableSites[f]; ok {
				seen[f] = true
				continue
			}
			key := f + ":" + name
			class, ok := commentWriteSites[key]
			if !ok {
				t.Errorf("%s writes comments (%s) and is not classified in commentWriteSites: decide INDEXED, CASCADE or BYPASS with a reason (TASK-3917)", key, src[m[0]:m[1]])
				continue
			}
			seen[key] = true
			if strings.HasPrefix(class, "INDEXED") && !strings.Contains(src[start:end], "reindexCommentMentionsTx(") {
				t.Errorf("%s is classified INDEXED but never calls reindexCommentMentionsTx(", key)
			}
		}
	}
	var stale []string
	for k := range commentWriteSites {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	for k := range commentWriteTableSites {
		if !seen[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale classifications (no write site found): %v", stale)
	}

	// No other package inserts comments behind the store's back.
	for _, dir := range []string{"../appstore", "../../cmd"} {
		_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, _ := os.ReadFile(p)
			if regexp.MustCompile(`(?i)INSERT INTO comments\b`).Match(b) {
				t.Errorf("%s inserts into comments outside internal/store", p)
			}
			return nil
		})
	}
}

func TestCommentMentions_CopyAndImportIndexed(t *testing.T) {
	s := testStore(t)
	alice := mentionUser(t, s, "alice")
	ws := createTestWorkspace(t, s, "MentionSrc")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "T", "")
	if _, err := s.CreateComment(ws.ID, item.ID, alice.ID, models.CommentCreate{Body: "cc @alice @nobody-here"}); err != nil {
		t.Fatal(err)
	}
	data, err := s.ExportWorkspace(ws.Slug)
	if err != nil || len(data.Comments) != 1 {
		t.Fatalf("export: %v (%d comments)", err, len(data.Comments))
	}
	dst, err := s.ImportWorkspace(data, "MentionDst", "", "api")
	if err != nil {
		t.Fatal(err)
	}
	var newID string
	if err := s.db.QueryRow(s.q(`SELECT id FROM comments WHERE workspace_id = ?`), dst.ID).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, newID, "import", alice)

	// Backfill rebuilds from bodies, sets the marker, and a second run is a no-op.
	if _, err := s.db.Exec(`DELETE FROM comment_mentions`); err != nil {
		t.Fatal(err)
	}
	res, err := s.BackfillCommentMentions()
	if err != nil || res.Skipped || res.CommentsScanned != 2 {
		t.Fatalf("backfill = %+v, %v; want a real pass over 2 comments", res, err)
	}
	if n := mentionCount(t, s, `user_id = ?`, alice.ID); n != 2 {
		t.Fatalf("backfill rebuilt %d rows, want 2", n)
	}
	var marker string
	if err := s.db.QueryRow(s.q(`SELECT value FROM platform_settings WHERE key = ?`), commentMentionsBackfilledFlag).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker = %q, %v", marker, err)
	}
	if _, err := s.db.Exec(`DELETE FROM comment_mentions`); err != nil {
		t.Fatal(err)
	}
	res, err = s.BackfillCommentMentions()
	if err != nil || !res.Skipped {
		t.Fatalf("second run = %+v, %v; want Skipped", res, err)
	}
	if n := mentionCount(t, s, `1 = 1`); n != 0 {
		t.Fatalf("a skipped run wrote %d rows", n)
	}

	// Cross-workspace item copy carries no comments, so no mentions.
	f := newCopyStorageFixture(t, "free", 100, 50)
	owner := f.owner
	carol := mentionUser(t, f.s, "carol")
	if _, err := f.s.CreateComment(f.wsID, f.itemID, owner.ID, models.CommentCreate{Body: "@" + carol.Username}); err != nil {
		t.Fatal(err)
	}
	// CONTROL: the source really has a mention, so the zero below is a copy
	// property and not an unmentionable owner.
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM comment_mentions WHERE workspace_id = ?`, f.wsID); n != 1 {
		t.Fatalf("control: source has %d mention rows, want 1 ", n)
	}
	if err := f.copy(false); err != nil {
		t.Fatal(err)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM comments WHERE workspace_id = ?`, f.dst.wsID); n != 0 {
		t.Fatalf("copy carried %d comments", n)
	}
	if n := task3394Count(t, f.s, `SELECT COUNT(*) FROM comment_mentions WHERE workspace_id = ?`, f.dst.wsID); n != 0 {
		t.Fatalf("copy carried %d mentions", n)
	}
}
