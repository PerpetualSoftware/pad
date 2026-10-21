package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/links"
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
	// New rows carry the comment's own created_at, not a fresh now().
	commentCol := func(id, col string) string {
		var v string
		if err := s.db.QueryRow(s.q(`SELECT `+col+` FROM comments WHERE id = ?`), id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if _, err := s.db.Exec(s.q(`UPDATE comments SET created_at = '2002-02-02T00:00:00Z' WHERE id = ?`), c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM comment_mentions WHERE comment_id = ?`), c.ID); err != nil {
		t.Fatal(err)
	}
	// (the backfill door is pinned in the backfill test; here a create is
	// compared against its own comment row)
	c1b, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{Body: "@alice"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := mentionRows(t, s, c1b.ID)[alice.ID], commentCol(c1b.ID, "created_at"); got != want {
		t.Fatalf("create: mention created_at %q, comment created_at %q", got, want)
	}
	if _, err := s.UpdateComment(c.ID, "@alice @bob"); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c.ID, "reseed", alice, bob)
	for _, u := range []*models.User{alice, bob} {
		if got, want := mentionRows(t, s, c.ID)[u.ID], commentCol(c.ID, "updated_at"); got != want {
			t.Fatalf("edit: new mention created_at %q, edit time %q", got, want)
		}
	}

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

	if err := s.DeleteComment(c1b.ID); err != nil {
		t.Fatal(err)
	}
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

// Guard limits, by decision (TASK-3917 r2): the census matches SQL text in Go
// source, so it does not see a table named with a schema (public.comments) or
// a statement assembled from string pieces; the INDEXED check ignores // comment
// lines only.
//
// commentWriteSite classifies one top-level declaration that writes comments.
// n is the number of comment-writing statements it holds: a new statement in
// an already classified func changes the count and fails the guard until it is
// classified here.
type commentWriteSite struct {
	class string
	n     int
}

// Keys are "<path from the repo root>:<enclosing top-level decl>".
var commentWriteSites = map[string]commentWriteSite{
	"internal/store/comments.go:createCommentTx":                         {"INDEXED: insert", 1},
	"internal/store/comments.go:updateComment":                           {"INDEXED: edit, behind UpdateComment and UpdateCommentAs (skips imported comments by ruling)", 1},
	"internal/store/comments.go:DeleteComment":                           {"INDEXED: tombstone; the hard DELETE cascades on comment_id", 2},
	"internal/store/comments.go:reapTombstonesTx":                        {"CASCADE: deletes emptied tombstones, which have no rows (the tombstone write already cleared them)", 1},
	"internal/store/fenced_comments.go:CreateComment":                    {"INDEXED: an app's insert", 1},
	"internal/store/fenced_comments.go:UpdateComment":                    {"INDEXED: an app's edit (skips imported comments by ruling)", 1},
	"internal/store/fenced_comments.go:DeleteComment":                    {"INDEXED: an app's tombstone; the hard DELETE cascades on comment_id", 2},
	"internal/store/export.go:importWorkspace":                           {"BYPASS: imported history is never indexed; matches needsOthersComment (needs_snooze.go) and #2231 r1, TASK-3917 lead ruling bb0c9fc6: a bundle's @handles name SOURCE-instance users", 1},
	"internal/store/attachments.go:RemapAttachmentReferencesInWorkspace": {"BYPASS: rewrites only pad-attachment ids inside the body; it cannot add or remove an @-mention", 1},
	"internal/store/users.go:eraseUserTx":                                {"BYPASS: nulls comments.user_id (the author); mentions are keyed on the MENTIONED user, whose rows cascade when the user row goes", 1},
	"internal/store/workspace_purge.go:purgeWorkspaceChildDeletes":       {"CASCADE: DELETE FROM comments in the purge table; the comment_id FK removes the rows", 1},
	"internal/store/workspace_purge.go:purgeWorkspaceDataOnce":           {"BYPASS: 'detach comment parents' only nulls parent_id", 1},
}

var (
	commentWriteRe = regexp.MustCompile(`(?is)(?:INSERT|REPLACE)\s+(?:OR\s+\w+\s+)?INTO\s+comments\b|UPDATE\s+(?:ONLY\s+)?comments\b|DELETE\s+FROM\s+comments\b`)
	topDeclRe      = regexp.MustCompile(`(?m)^(?:func (?:\([^)]*\) )?(\w+)|var (\w+)|const (\w+)|type (\w+))`)
)

// commentWriteCensus returns key -> count of comment-writing statements for
// the given Go source files (key = path:enclosing top-level declaration).
func commentWriteCensus(t *testing.T, files map[string]string) map[string]int {
	t.Helper()
	got := map[string]int{}
	for path, src := range files {
		decls := topDeclRe.FindAllStringSubmatchIndex(src, -1)
		for _, m := range commentWriteRe.FindAllStringIndex(src, -1) {
			name := "(top level)"
			for _, d := range decls {
				if d[0] > m[0] {
					break
				}
				for g := 1; g <= 4; g++ {
					if d[2*g] >= 0 {
						name = src[d[2*g]:d[2*g+1]]
					}
				}
			}
			got[path+":"+name]++
		}
	}
	return got
}

func readNonTestGo(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, root := range []string{"../../internal", "../../cmd"} {
		if err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel("../..", p)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// funcBody returns the source of the top-level decl named name in src.
func funcBody(src, name string) string {
	decls := topDeclRe.FindAllStringSubmatchIndex(src, -1)
	for i, d := range decls {
		for g := 1; g <= 4; g++ {
			if d[2*g] >= 0 && src[d[2*g]:d[2*g+1]] == name {
				end := len(src)
				if i+1 < len(decls) {
					end = decls[i+1][0]
				}
				return src[d[0]:end]
			}
		}
	}
	return ""
}

// callsReindex reports whether the top-level decl name in src CALLS the
// helper. Lines that are // comments are ignored: a doc comment naming the
// helper (including the doc comment of the NEXT decl, which funcBody's span
// includes) is not a call (TASK-3917 r2 F5). It is a text match, so the helper
// named in a trailing // comment, a /* */ comment or a string literal still
// reads as a call: that direction fails OPEN (a decl that lacks the call
// passes), accepted because it needs the helper's exact name with "(" written
// beside code that does not call it (TASK-3917 r3 F4).
func callsReindex(src, name string) bool {
	var code []string
	for _, line := range strings.Split(funcBody(src, name), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	return strings.Contains(strings.Join(code, "\n"), "reindexCommentMentionsTx(")
}

// The INDEXED check ignores comment lines. Each leg is isolated, so each fails
// alone if the filter goes: innerComment names the helper only in a comment
// inside its body (the decl after it has no doc); nextDoc's span holds only the
// following decl's doc comment naming the helper; real is the control.
func TestCommentMentions_GuardIgnoresCommentLines(t *testing.T) {
	src := "package store\n\n" +
		"func (s *Store) innerComment() {\n\t// reindexCommentMentionsTx( is mentioned in prose\n}\n\n" +
		"func (s *Store) sep() {\n}\n\n" +
		"func (s *Store) nextDoc() {\n}\n\n" +
		"// docd's doc names reindexCommentMentionsTx( too.\n" +
		"func (s *Store) docd() {\n}\n\n" +
		"func (s *Store) real() {\n\ts.reindexCommentMentionsTx(tx, 1)\n}\n"
	if callsReindex(src, "innerComment") {
		t.Error("a comment inside the body was read as a call (innerComment)")
	}
	if callsReindex(src, "nextDoc") {
		t.Error("the next decl's doc comment was read as a call (nextDoc)")
	}
	if !callsReindex(src, "real") {
		t.Error("a real call was not seen (control)")
	}
}

func TestCommentMentions_InsertSiteGuard(t *testing.T) {
	files := readNonTestGo(t)
	got := commentWriteCensus(t, files)
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		site, ok := commentWriteSites[k]
		if !ok {
			t.Errorf("%s writes comments (%d statement(s)) and is not classified in commentWriteSites: decide INDEXED, CASCADE or BYPASS with a reason, with the statement count (TASK-3917)", k, got[k])
			continue
		}
		if got[k] != site.n {
			t.Errorf("%s holds %d comment-writing statements, classified for %d: classify the new statement (INDEXED must reindex it) and update the count (TASK-3917)", k, got[k], site.n)
		}
		if strings.HasPrefix(site.class, "INDEXED") {
			path, name, _ := strings.Cut(k, ":")
			if !callsReindex(files[path], name) {
				t.Errorf("%s is classified INDEXED but never calls reindexCommentMentionsTx(", k)
			}
		}
	}
	var stale []string
	for k := range commentWriteSites {
		if _, ok := got[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("stale classifications (no write site found): %v", stale)
	}
}

// The guard itself: its regex and census see spaced, newline-split and
// lower-case statements, in a scratch file standing in for a store file.
func TestCommentMentions_GuardSeesVariants(t *testing.T) {
	src := "package store\n\nfunc (s *Store) sneaky() {\n\ttx.Exec(`insert  into\n comments (id) VALUES (1)`)\n\ttx.Exec(`REPLACE INTO comments (id) VALUES (1)`)\n\ttx.Exec(`update only comments SET x = 1`)\n\ttx.Exec(`delete\nfrom comments`)\n}\n"
	got := commentWriteCensus(t, map[string]string{"internal/store/scratch.go": src})
	if got["internal/store/scratch.go:sneaky"] != 4 {
		t.Fatalf("census = %v, want 4 statements in sneaky", got)
	}
}

func TestCommentMentions_CopyAndImportNotIndexed(t *testing.T) {
	s := testStore(t)
	alice := mentionUser(t, s, "alice")
	ws := createTestWorkspace(t, s, "MentionSrc")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "T", "")
	live, err := s.CreateComment(ws.ID, item.ID, alice.ID, models.CommentCreate{Body: "cc @alice @nobody-here"})
	if err != nil {
		t.Fatal(err)
	}
	// CONTROL: a native comment with the same resolvable handle IS indexed.
	wantMentions(t, s, live.ID, "native control", alice)
	data, err := s.ExportWorkspace(ws.Slug)
	if err != nil || len(data.Comments) != 1 {
		t.Fatalf("export: %v (%d comments)", err, len(data.Comments))
	}
	dst, err := s.ImportWorkspace(data, "MentionDst", "", "api")
	if err != nil {
		t.Fatal(err)
	}
	var newID string
	if err := s.db.QueryRow(s.q(`SELECT id FROM comments WHERE workspace_id = ? AND imported = 1`), dst.ID).Scan(&newID); err != nil {
		t.Fatal(err)
	}
	// Imported comments are never indexed (TASK-3917 lead ruling bb0c9fc6).
	wantMentions(t, s, newID, "import")
	if n := mentionCount(t, s, `workspace_id = ?`, dst.ID); n != 0 {
		t.Fatalf("import wrote %d mention rows", n)
	}

	// Editing an imported comment does not index it either (the reindex
	// covers the whole body, so the unedited source handles would bind).
	bob := mentionUser(t, s, "bob")
	if _, err := s.UpdateComment(newID, "cc @alice and now @bob"); err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, newID, "edit of an imported comment")

	// The backfill skips imported comments and indexes the native one.
	if _, err := s.db.Exec(s.q(`UPDATE comments SET created_at = '2003-03-03T00:00:00Z' WHERE id = ?`), live.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`DELETE FROM comment_mentions`); err != nil {
		t.Fatal(err)
	}
	res, err := s.BackfillCommentMentions()
	if err != nil || res.Skipped || res.CommentsScanned != 1 {
		t.Fatalf("backfill = %+v, %v; want a real pass over the 1 native comment", res, err)
	}
	wantMentions(t, s, newID, "backfill of an imported comment")
	wantMentions(t, s, live.ID, "backfill of the native comment", alice)
	// A backfilled row carries the comment's created_at.
	var cat string
	if err := s.db.QueryRow(s.q(`SELECT created_at FROM comments WHERE id = ?`), live.ID).Scan(&cat); err != nil {
		t.Fatal(err)
	}
	if got := mentionRows(t, s, live.ID)[alice.ID]; got != cat {
		t.Fatalf("backfilled created_at %q, comment %q", got, cat)
	}
	_ = bob
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

// Unresolvable names that sort early must not evict a real user: the cap
// applies to RESOLVED mentions (TASK-3917 r2 F4).
func TestCommentMentions_CapAfterResolution(t *testing.T) {
	f := task3408Fixture(t, "inst-capres")
	s := f.s
	author := mentionUser(t, s, "author")
	real := mentionUser(t, s, "zed")
	col := createTestCollection(t, s, f.ws.ID, "CapRes")
	item := createTestItem(t, s, f.ws.ID, col.ID, "Thread", "")
	var body strings.Builder
	for i := 0; i < 60; i++ {
		fmt.Fprintf(&body, "@aaa%03d ", i) // no such users; sort before "zed"
	}
	body.WriteString("@zed")
	c, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{Body: body.String()})
	if err != nil {
		t.Fatal(err)
	}
	wantMentions(t, s, c.ID, "real user behind 60 unresolvable names", real)
}

func TestCommentMentions_CapPerComment(t *testing.T) {
	f := task3408Fixture(t, "inst-cap")
	s := f.s
	author := mentionUser(t, s, "author")
	col := createTestCollection(t, s, f.ws.ID, "Cap")
	item := createTestItem(t, s, f.ws.ID, col.ID, "Thread", "")
	var body strings.Builder
	n := links.MaxMentionsPerComment + 1
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("user%03d", i)
		mentionUser(t, s, name)
		body.WriteString("@" + name + " ")
	}
	c, err := s.CreateComment(f.ws.ID, item.ID, author.ID, models.CommentCreate{Body: body.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(mentionRows(t, s, c.ID)); got != links.MaxMentionsPerComment {
		t.Fatalf("%d mentions indexed for %d distinct resolvable names, want the cap %d", got, n, links.MaxMentionsPerComment)
	}
}
