package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3751: the read-only tie-order diagnostic counts what it should on a
// seeded history. Each version replaces the whole body with unrelated text,
// so a patch applies only to the body it was made from and fuzzy matching
// cannot rescue a wrong order.
func TestBUG3751_VersionTieDiagnosticCounts(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "Ties")
	coll := createTestCollection(t, s, ws.ID, "Tasks")

	const (
		oldest = "the quick brown fox jumps over the hedge"
		middle = "lazy dogs sleep all afternoon in the sun"
		latest = "completely different words are written here"
	)
	seed := func(title string, oldestSeq, middleSeq int, sameSecond bool) {
		t.Helper()
		it, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: title, Content: latest})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(s.q(`DELETE FROM item_versions WHERE item_id = ?`), it.ID); err != nil {
			t.Fatal(err)
		}
		middleAt, oldestAt := "2026-01-01T00:00:05Z", "2026-01-01T00:00:05Z"
		if !sameSecond {
			oldestAt = "2026-01-01T00:00:04Z"
		}
		for _, v := range []struct {
			id, patch, at string
			seq           int
		}{
			{newID(), diff.CreateReversePatch(middle, latest), middleAt, middleSeq},
			{newID(), diff.CreateReversePatch(oldest, middle), oldestAt, oldestSeq},
		} {
			if _, err := s.db.Exec(s.q(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?)`),
				v.id, it.ID, v.patch, true, v.at, v.seq); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed("in order", 1, 2, true)   // the newer version holds the higher seq
	seed("misordered", 2, 1, true) // what a ctid backfill can produce
	seed("not a tie", 1, 2, false) // different seconds: never a tie group

	rep, err := DiagnoseVersionTies(context.Background(), s.db, s.dialect.Driver() == DriverPostgres, DiagnoseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := VersionTieReport{ItemsScanned: 2, TieGroups: 2, CurrentOrderOnly: 1, Misordered: 1}
	if *rep != want {
		t.Fatalf("report = %+v, want %+v", *rep, want)
	}
}

// The diagnostic cannot write, by construction: on Postgres its transaction is
// READ ONLY at the server, and on SQLite its connection is query_only. A write
// attempted the same way the diagnostic reads is refused.
func TestBUG3751_DiagnosticCannotWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if s.dialect.Driver() == DriverPostgres {
		tx, err := beginDiagnosticTx(ctx, s.db, true, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		_, err = tx.ExecContext(ctx, `UPDATE items SET title = title`)
		if err == nil || !strings.Contains(err.Error(), "25006") {
			t.Fatalf("a write inside the diagnostic transaction: err = %v, want SQLSTATE 25006 (read-only transaction)", err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "ro.db")
	w, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	db, err := openDiagnosticDB(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := beginDiagnosticTx(ctx, db, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, err := tx.ExecContext(ctx, `UPDATE items SET title = title`); err == nil || !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Fatalf("a write on the diagnostic's SQLite connection: err = %v, want a read-only refusal", err)
	}
}
