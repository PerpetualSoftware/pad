package store

import (
	"context"
	"fmt"
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

	// The deadline can close the rows between Next and Scan; in either row
	// loop that is a truncated report, not an error (codex on BUG-3751).
	for _, loop := range []string{"items", "versions"} {
		fired := false
		testHookBeforeScan = func(l string) {
			if l == loop && !fired {
				fired = true
				time.Sleep(200 * time.Millisecond)
			}
		}
		rep, err = DiagnoseVersionTies(context.Background(), s.db, s.dialect.Driver() == DriverPostgres, DiagnoseOptions{MaxDuration: 100 * time.Millisecond})
		testHookBeforeScan = nil
		if !fired {
			t.Fatalf("instrument: the %s scan hook never ran", loop)
		}
		if err != nil || rep == nil || !rep.Truncated {
			t.Fatalf("a deadline before a %s scan: report %+v, err %v; want a truncated report", loop, rep, err)
		}
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
		tx, err := beginDiagnosticTx(ctx, s.db, true, 1500*time.Millisecond)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback() //nolint:errcheck
		// The timeout is set first in the transaction and SET TRANSACTION
		// READ ONLY is still accepted after it (codex on BUG-3751).
		for setting, want := range map[string]string{"statement_timeout": "1500ms", "transaction_read_only": "on"} {
			var got string
			if err := tx.QueryRowContext(ctx, "SHOW "+setting).Scan(&got); err != nil || got != want {
				t.Fatalf("SHOW %s = %q (%v), want %q", setting, got, err, want)
			}
		}
		t.Log("ran on Postgres: statement_timeout and read-only verified")
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

// expiringCtx reports the deadline once expired is set.
type expiringCtx struct {
	context.Context
	expired bool
}

func (c *expiringCtx) Err() error {
	if c.expired {
		return context.DeadlineExceeded
	}
	return nil
}

// Classification is bounded by the context wherever the deadline lands
// (codex on BUG-3751): for every version application during which it can
// expire, the item is not reported complete and nothing is applied after it.
// The histories walk every path: a tie group small enough to permute,
// singletons, a group too large to permute, a group holding a snapshot, a
// chain that breaks at a singleton, and a group no order of which applies.
func TestBUG3751_ClassificationStopsWhereverTheDeadlineLands(t *testing.T) {
	words := []string{"amber", "basalt", "cobalt", "dune", "ember", "fjord", "garnet", "harbor", "indigo", "juniper", "kestrel", "lagoon", "meadow"}
	texts := make([]string, len(words))
	for i, w := range words {
		texts[i] = strings.Repeat(w+" ", 8) + fmt.Sprint(i)
	}
	// chain builds versions that walk texts[0] back through texts[1..], one
	// per entry of at; a patch from a text no walk reaches does not apply.
	chain := func(at ...string) []tieVersion {
		var vs []tieVersion
		for i := range at {
			vs = append(vs, tieVersion{createdAt: at[i], content: diff.CreateReversePatch(texts[i+1], texts[i]), isDiff: true})
		}
		return vs
	}
	stray := tieVersion{content: diff.CreateReversePatch(texts[12], texts[11]), isDiff: true}

	every := chain("t9", "t9", "t8", "t7", "t6", "t6", "t6", "t6", "t6", "t6", "t5", "t5")
	every[11] = tieVersion{createdAt: "t5", content: texts[12]}
	brokenSingle := append(chain("t9", "t8"), tieVersion{createdAt: "t7", content: stray.content, isDiff: true})
	unresolvable := append(chain("t9"), tieVersion{createdAt: "t8", content: stray.content, isDiff: true}, tieVersion{createdAt: "t8", content: stray.content, isDiff: true})

	for _, h := range []struct {
		name     string
		versions []tieVersion
		want     VersionTieReport
	}{
		{"every group shape", every, VersionTieReport{TieGroups: 3, CurrentOrderOnly: 1, SkippedLarge: 1, WithSnapshot: 1}},
		{"chain breaks at a singleton", brokenSingle, VersionTieReport{ChainBroken: 1}},
		{"no order applies", unresolvable, VersionTieReport{TieGroups: 1, Unresolvable: 1}},
	} {
		t.Run(h.name, func(t *testing.T) {
			// The deadline is tied to the work, not to the checks: it expires
			// during the n-th version application, so a missing check shows
			// up as a version applied after it, or as a complete report.
			var ctx *expiringCtx
			applies, n, appliedAfter := 0, 0, 0
			testHookApplyVersion = func() {
				if ctx.expired {
					appliedAfter++
				}
				applies++
				if applies == n {
					ctx.expired = true
				}
			}
			t.Cleanup(func() { testHookApplyVersion = nil })

			ctx = &expiringCtx{Context: context.Background()}
			var rep VersionTieReport
			if !classifyTieGroups(ctx, texts[0], h.versions, &rep) {
				t.Fatal("instrument: a live context classifies the whole history")
			}
			if rep != h.want {
				t.Fatalf("instrument: report = %+v, want %+v", rep, h.want)
			}
			total := applies

			for n = 1; n <= total; n++ {
				ctx, applies, appliedAfter = &expiringCtx{Context: context.Background()}, 0, 0
				var rep VersionTieReport
				if classifyTieGroups(ctx, texts[0], h.versions, &rep) {
					t.Errorf("deadline during application %d of %d: classification reported complete", n, total)
				}
				if appliedAfter != 0 {
					t.Errorf("deadline during application %d of %d: %d version(s) applied after it", n, total, appliedAfter)
				}
			}
		})
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

// The operator entry point refuses to run without a run deadline: it is the
// only bound on opening the transaction (codex on BUG-3751).
func TestBUG3751_OperatorRunRequiresADeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-opened.db")
	for _, d := range []time.Duration{0, -time.Second} {
		if _, err := DiagnoseVersionTiesAt(context.Background(), path, false, DiagnoseOptions{MaxDuration: d}); err == nil || !strings.Contains(err.Error(), "max duration") {
			t.Errorf("MaxDuration %v: err = %v, want a refusal naming the max duration", d, err)
		}
	}
}
