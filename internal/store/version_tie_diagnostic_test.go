package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// A tie group holding a full-content row: its order cannot be told.
	snap, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: "snapshot tie", Content: latest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM item_versions WHERE item_id = ?`), snap.ID); err != nil {
		t.Fatal(err)
	}
	for i, v := range []struct {
		content string
		isDiff  bool
	}{{diff.CreateReversePatch(middle, latest), true}, {oldest, false}} {
		if _, err := s.db.Exec(s.q(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES (?, ?, ?, ?, ?, ?)`),
			newID(), snap.ID, v.content, v.isDiff, "2026-01-01T00:00:05Z", 2-i); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := DiagnoseVersionTies(context.Background(), s.db, s.dialect.Driver() == DriverPostgres, DiagnoseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := VersionTieReport{ItemsScanned: 3, ItemsWithTies: 3, TieGroups: 3, CurrentOrderOnly: 1, Misordered: 1, WithSnapshot: 1}
	if *rep != want {
		t.Fatalf("report = %+v, want %+v", *rep, want)
	}

	// A deadline reached while an item is being classified stops the run
	// there: that item is not counted, and the report says truncated rather
	// than presenting a complete result (codex on BUG-3751).
	testHookBeforeClassify = func(scanned int) {
		if scanned == 1 {
			time.Sleep(200 * time.Millisecond)
		}
	}
	rep, err = DiagnoseVersionTies(context.Background(), s.db, s.dialect.Driver() == DriverPostgres, DiagnoseOptions{MaxDuration: 100 * time.Millisecond})
	testHookBeforeClassify = nil
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated || rep.ItemsScanned != 1 {
		t.Fatalf("a deadline inside classification: %+v, want truncated after exactly 1 item", *rep)
	}

	// A run past its deadline stops and says so.
	rep, err = DiagnoseVersionTies(context.Background(), s.db, s.dialect.Driver() == DriverPostgres, DiagnoseOptions{MaxDuration: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Truncated || rep.ItemsScanned >= 3 {
		t.Fatalf("a run past its deadline: %+v, want truncated with fewer than the 3 tied items scanned", *rep)
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

// Classification is bounded by the context, so a deadline that lands inside
// one item's tie groups ends that item's walk (codex on BUG-3751).
func TestBUG3751_ClassificationStopsWhenTheContextEnds(t *testing.T) {
	const a, b, c = "the quick brown fox jumps over the hedge", "lazy dogs sleep all afternoon in the sun", "completely different words are written here"
	versions := []tieVersion{
		{createdAt: "t5", content: diff.CreateReversePatch(b, c), isDiff: true},
		{createdAt: "t5", content: diff.CreateReversePatch(a, b), isDiff: true},
	}
	var live VersionTieReport
	if !classifyTieGroups(context.Background(), c, versions, &live) || live.TieGroups != 1 {
		t.Fatalf("instrument: a live context classifies the group: %+v", live)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var rep VersionTieReport
	if classifyTieGroups(ctx, c, versions, &rep) {
		t.Fatalf("an ended context: classification reported complete: %+v", rep)
	}
}

func TestBUG3751_StatementTimeoutNeverRoundsToZero(t *testing.T) {
	for d, want := range map[time.Duration]int64{
		time.Nanosecond:                    1,
		999 * time.Microsecond:             1,
		time.Millisecond:                   1,
		time.Millisecond + time.Nanosecond: 2,
		30 * time.Second:                   30000,
	} {
		if got := statementTimeoutMS(d); got != want {
			t.Errorf("statementTimeoutMS(%v) = %d, want %d", d, got, want)
		}
	}
}
