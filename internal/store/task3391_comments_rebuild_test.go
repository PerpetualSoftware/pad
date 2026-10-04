package store

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// TASK-3391: comments.source accepts 'app'. On SQLite that is a table rebuild
// (migration 115); on Postgres a constraint swap (089). The lead required
// proof that FTS, the NUL triggers, the reaction cascade and the parent FK
// survive, on both dialects.

const migration115 = "115_comments_source_app.sql"

type commentFixture struct {
	s    *Store
	ws   string
	item string
}

func newCommentFixture(t *testing.T) commentFixture {
	t.Helper()
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Comments")
	col := createTestCollection(t, s, ws.ID, "Tickets")
	it := createTestItem(t, s, ws.ID, col.ID, "Commented", "")
	return commentFixture{s: s, ws: ws.ID, item: it.ID}
}

func (f commentFixture) insert(t *testing.T, id, body, source string, parent any) error {
	t.Helper()
	ts := now()
	_, err := f.s.db.Exec(f.s.q(`INSERT INTO comments (id, item_id, workspace_id, author, body, created_by, source, created_at, updated_at, parent_id)
		VALUES (?, ?, ?, 'a', ?, 'user', ?, ?, ?, ?)`), id, f.item, f.ws, body, source, ts, ts, parent)
	return err
}

func (f commentFixture) mustInsert(t *testing.T, id, body string, parent any) {
	t.Helper()
	if err := f.insert(t, id, body, "web", parent); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
}

// ftsHits returns the comment ids whose body matches word, through each
// dialect's own index (FTS5 on SQLite, search_vector on Postgres).
func (f commentFixture) ftsHits(t *testing.T, word string) []string {
	t.Helper()
	q := `SELECT c.id FROM comments_fts JOIN comments c ON c.rowid = comments_fts.rowid WHERE comments_fts MATCH ? ORDER BY c.id`
	if f.s.dialect.Driver() == DriverPostgres {
		q = `SELECT id FROM comments WHERE search_vector @@ plainto_tsquery('english', ?) ORDER BY id`
	}
	rows, err := f.s.db.Query(f.s.q(q), word)
	if err != nil {
		t.Fatalf("fts query: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestTASK3391_CommentsSurviveTheSourceMigration(t *testing.T) {
	f := newCommentFixture(t)

	// The CHECK: 'app' is accepted, anything outside the list is still refused.
	if err := f.insert(t, "c-app", "from an app", "app", nil); err != nil {
		t.Fatalf("source 'app' refused: %v", err)
	}
	if err := f.insert(t, "c-bogus", "x", "bogus", nil); err == nil {
		t.Fatal("source 'bogus' was accepted: the CHECK is gone")
	}

	// FTS follows insert, update and delete.
	f.mustInsert(t, "c1", "the wombatword comment", nil)
	f.mustInsert(t, "c2", "a quokkaword reply", "c1")
	if got := f.ftsHits(t, "wombatword"); strings.Join(got, ",") != "c1" {
		t.Fatalf("insert not indexed: %v", got)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE comments SET body = 'the numbatword comment' WHERE id = 'c1'`)); err != nil {
		t.Fatal(err)
	}
	if got := f.ftsHits(t, "wombatword"); len(got) != 0 {
		t.Fatalf("old body still indexed after update: %v", got)
	}
	if got := f.ftsHits(t, "numbatword"); strings.Join(got, ",") != "c1" {
		t.Fatalf("update not indexed: %v", got)
	}

	// The parent FK refuses a reply to a comment that does not exist.
	if err := f.insert(t, "c-orphan", "x", "web", "no-such-comment"); err == nil {
		t.Fatal("a reply to a missing parent was accepted: the parent FK is gone")
	}

	// The NUL triggers (SQLite) / native refusal (Postgres) on all four
	// protected columns.
	for _, col := range []string{"author", "body", "created_by", "source"} {
		ts := now()
		vals := map[string]string{"author": "a", "body": "b", "created_by": "user", "source": "web"}
		vals[col] = vals[col] + "\x00x"
		_, err := f.s.db.Exec(f.s.q(`INSERT INTO comments (id, item_id, workspace_id, author, body, created_by, source, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`), "c-nul-"+col, f.item, f.ws, vals["author"], vals["body"], vals["created_by"], vals["source"], ts, ts)
		if err == nil {
			t.Errorf("a NUL in comments.%s was accepted", col)
		}
	}

	// The reaction cascade: deleting a comment deletes its reactions.
	if _, err := f.s.db.Exec(f.s.q(`INSERT INTO comment_reactions (id, comment_id, actor, emoji, created_at) VALUES ('r1', 'c2', 'user', 'x', ?)`), now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM comments WHERE id = 'c2'`)); err != nil {
		t.Fatal(err)
	}
	var reactions int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM comment_reactions WHERE id = 'r1'`).Scan(&reactions); err != nil || reactions != 0 {
		t.Fatalf("reaction survived its comment's delete: %d (%v)", reactions, err)
	}
	if got := f.ftsHits(t, "quokkaword"); len(got) != 0 {
		t.Fatalf("deleted comment still indexed: %v", got)
	}
}

// commentsSchema is the comments table's shape as SQLite reports it: columns,
// foreign keys and indexes. A rebuild must leave it identical.
func commentsSchema(t *testing.T, s *Store) string {
	t.Helper()
	var parts []string
	collect := func(label, q string) {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		cols, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fieldsOut := make([]string, len(vals))
			for i, v := range vals {
				fieldsOut[i] = fmt.Sprint(v)
			}
			lines = append(lines, strings.Join(fieldsOut, " "))
		}
		sort.Strings(lines)
		parts = append(parts, label+":\n"+strings.Join(lines, "\n"))
	}
	collect("columns", `PRAGMA table_info(comments)`)
	// The fk "id" and "seq" columns number the constraints in declaration
	// order, which the rebuild may legitimately renumber; compare the rest.
	collect("fks", `SELECT "table", "from", "to", on_update, on_delete FROM pragma_foreign_key_list('comments')`)
	collect("inbound", `SELECT m.name, f."from", f.on_delete FROM sqlite_master m, pragma_foreign_key_list(m.name) f WHERE m.type = 'table' AND f."table" = 'comments'`)
	collect("indexes", `SELECT name, "unique", partial FROM pragma_index_list('comments')`)
	collect("triggers", `SELECT name FROM sqlite_master WHERE type = 'trigger' AND tbl_name = 'comments'`)
	return strings.Join(parts, "\n")
}

// SQLite: re-running the rebuild over populated data keeps every row, rowid,
// link and index posting, and leaves the schema identical. 115 rebuilds from
// whatever comments holds, so re-applying it through the real runner is a
// faithful rebuild of a populated table.
func TestTASK3391_RebuildPreservesRowsAndSchema(t *testing.T) {
	f := newCommentFixture(t)
	skipUnlessSQLite(t, f.s)

	// A deleted row leaves a rowid gap, so a rebuild that renumbered instead
	// of copying rowids would shift every later row off its FTS posting.
	f.mustInsert(t, "a-gap", "deleted before the rebuild", nil)
	f.mustInsert(t, "p1", "parent emuword", nil)
	if _, err := f.s.db.Exec(`DELETE FROM comments WHERE id = 'a-gap'`); err != nil {
		t.Fatal(err)
	}
	f.mustInsert(t, "r1", "reply dingoword", "p1")
	f.mustInsert(t, "t1", "", nil)
	if _, err := f.s.db.Exec(`UPDATE comments SET deleted_at = ? WHERE id = 't1'`, now()); err != nil {
		t.Fatal(err)
	}
	f.mustInsert(t, "t1-reply", "under a tombstone", "t1")
	if _, err := f.s.db.Exec(`INSERT INTO comment_reactions (id, comment_id, actor, emoji, created_at) VALUES ('x1', 'p1', 'user', 'x', ?)`, now()); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		rows, err := f.s.db.Query(`SELECT rowid, id, COALESCE(parent_id, ''), COALESCE(deleted_at, '') <> '', body FROM comments ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var rowid int64
			var id, parent, body string
			var deleted bool
			if err := rows.Scan(&rowid, &id, &parent, &deleted, &body); err != nil {
				t.Fatal(err)
			}
			out = append(out, fmt.Sprintf("%d %s %s %v %s", rowid, id, parent, deleted, body))
		}
		return strings.Join(out, "\n")
	}
	beforeRows, beforeSchema := snapshot(), commentsSchema(t, f.s)

	sqlText, err := migrationsFS.ReadFile("migrations/" + migration115)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, migration115); err != nil {
		t.Fatal(err)
	}
	if err := applySQLiteMigration(f.s.db, migration115, string(sqlText)); err != nil {
		t.Fatalf("apply %s: %v", migration115, err)
	}
	// ensureNULTriggers is deliberately NOT called: 115 re-creates the NUL
	// triggers inside its own transaction, and the checks below prove it.

	if after := snapshot(); after != beforeRows {
		t.Fatalf("rows changed across the rebuild:\nbefore:\n%s\nafter:\n%s", beforeRows, after)
	}
	if after := commentsSchema(t, f.s); after != beforeSchema {
		t.Fatalf("schema changed across the rebuild:\nbefore:\n%s\nafter:\n%s", beforeSchema, after)
	}
	for word, want := range map[string]string{"emuword": "p1", "dingoword": "r1"} {
		if got := f.ftsHits(t, word); strings.Join(got, ",") != want {
			t.Errorf("search %q after the rebuild: %v, want %s", word, got, want)
		}
	}
	var reactions int
	if err := f.s.db.QueryRow(`SELECT COUNT(*) FROM comment_reactions WHERE comment_id = 'p1'`).Scan(&reactions); err != nil || reactions != 1 {
		t.Fatalf("reactions after the rebuild: %d (%v)", reactions, err)
	}
	if err := f.insert(t, "c-app", "from an app", "app", nil); err != nil {
		t.Fatalf("source 'app' refused after the rebuild: %v", err)
	}
	if err := f.insert(t, "c-nul", "nul\x00body", "web", nil); err == nil {
		t.Fatal("a NUL body was accepted after the rebuild")
	}
	if err := f.insert(t, "c-orphan", "x", "web", "no-such-comment"); err == nil {
		t.Fatal("a reply to a missing parent was accepted after the rebuild")
	}
	if _, err := f.s.db.Exec(`DELETE FROM comments WHERE id = 'r1'`); err != nil {
		t.Fatal(err)
	}
	if got := f.ftsHits(t, "dingoword"); len(got) != 0 {
		t.Fatalf("deleted comment still indexed after the rebuild: %v", got)
	}
	// The live table's FKs, as the U2a-head dump recorded them.
	schema := commentsSchema(t, f.s)
	for _, want := range []string{"items item_id id", "workspaces workspace_id id", "users user_id id", "activities activity_id id", "comments parent_id id", "app_installs via_app id NO ACTION SET NULL", "comment_reactions comment_id CASCADE",
		"idx_comments_activity", "idx_comments_item", "idx_comments_parent", "idx_comments_workspace",
		"comments_fts_insert", "comments_fts_update", "comments_fts_delete",
		"pad_nul_comments_author_ins", "pad_nul_comments_author_upd", "pad_nul_comments_body_ins", "pad_nul_comments_body_upd",
		"pad_nul_comments_created_by_ins", "pad_nul_comments_created_by_upd", "pad_nul_comments_source_ins", "pad_nul_comments_source_upd"} {
		if !strings.Contains(schema, want) {
			t.Errorf("after the rebuild, the schema lacks %q:\n%s", want, schema)
		}
	}
}
