package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// TestNULTriggersRefuseAnUnguardedWriter is the test S2 exists for.
//
// It writes through a RAW sqlite connection to the same database file — no
// driver guard in the path — which is precisely BUG-2813's scenario: an older
// binary, a rollback, a staged rollout, a second instance. Layer A cannot help
// there because Layer A lives in the binary. The trigger is enforced by the
// FILE, so it holds for every writer.
//
// If this passes while Layer A's tests also pass, the invariant has moved from
// being a property of our code to being a property of the data.
func TestNULTriggersRefuseAnUnguardedWriter(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only; Postgres refuses natively")
	}
	ws := createTestWorkspace(t, s, "TriggerWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Trigger subject", "body")

	// The unguarded writer. Same file, raw driver — what an old binary is.
	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	// Control FIRST: the raw connection can write normally, so a refusal below
	// is the trigger and not a broken fixture.
	if _, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`, "clean from the old binary", item.ID); err != nil {
		t.Fatalf("the unguarded writer must be able to write clean values: %v", err)
	}

	t.Run("raw NUL in a text column", func(t *testing.T) {
		_, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`, "old"+textguard.NUL+"binary", item.ID)
		if err == nil {
			t.Fatal("an unguarded writer stored a NUL — the database does not own the invariant")
		}
		if !strings.Contains(err.Error(), "pad_nul_invariant") {
			t.Errorf("refused, but not by our trigger: %v", err)
		}
	})

	t.Run("decoded escape in a JSON column", func(t *testing.T) {
		doc := `{"status":"open","note":"x` + textguard.EscNUL + `y"}`
		_, err := raw.Exec(`UPDATE items SET fields = ? WHERE id = ?`, doc, item.ID)
		if err == nil {
			t.Fatal("an unguarded writer stored a decoded-NUL escape in a JSON column")
		}
		if !strings.Contains(err.Error(), "pad_nul_invariant") {
			t.Errorf("refused, but not by our trigger: %v", err)
		}
	})

	t.Run("decoded escape in a JSON KEY", func(t *testing.T) {
		doc := `{"k` + textguard.EscNUL + `ey":"v"}`
		_, err := raw.Exec(`UPDATE items SET fields = ? WHERE id = ?`, doc, item.ID)
		if err == nil {
			t.Fatal("a NUL in a JSON KEY was stored; json_tree exposes keys and the trigger must read them")
		}
		// Assert WHICH refusal (codex round 3). items.fields also carries
		// migration 056's JSON constraint, so "some error" would be satisfied
		// by a value that never reached the trigger at all.
		if !strings.Contains(err.Error(), nulTriggerMarker) {
			t.Errorf("refused, but not by our trigger: %v", err)
		}
	})

	t.Run("INSERT is covered, not only UPDATE", func(t *testing.T) {
		_, err := raw.Exec(
			`INSERT INTO items (id, workspace_id, collection_id, title, slug, content, fields, tags, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, '{}', '[]', '2026-01-01', '2026-01-01')`,
			"trigger-insert-probe", ws.ID, col.ID, "ins"+textguard.NUL+"title", "trigger-insert-probe", "")
		if err == nil {
			t.Fatal("an unguarded INSERT stored a NUL title")
		}
		if !strings.Contains(err.Error(), nulTriggerMarker) {
			t.Errorf("refused, but not by our trigger — a NOT NULL or FK constraint would also fail "+
				"this INSERT and prove nothing: %v", err)
		}
	})

	// The doubled-backslash control, which is the false-positive the whole
	// predicate family has to avoid. Measured literal on modernc in TASK-2824.
	t.Run("a doubled backslash is NOT refused", func(t *testing.T) {
		doc := `{"note":"x\` + textguard.EscNUL + `y"}`
		if _, err := raw.Exec(`UPDATE items SET fields = ? WHERE id = ?`, doc, item.ID); err != nil {
			t.Errorf("literal escape text was refused — the trigger has a false positive, which is the "+
				"exact trap the raw-byte pre-filter failed on: %v", err)
		}
	})

	// A JSON-classed column holding a NON-DOCUMENT must still be writable:
	// json_tree raises on invalid JSON, so the json_valid guard in the trigger
	// is load-bearing rather than defensive.
	//
	// The column is workspaces.settings, NOT items.fields, and the difference
	// is the whole point of the case. items.fields carries a JSON constraint
	// from migration 056, so a non-document write there is refused whether or
	// not our trigger exists — measured by dropping the trigger pair and
	// watching the same "malformed JSON" error. A fixture rejectable for two
	// reasons discriminates neither.
	t.Run("a JSON column with no schema constraint accepts a non-document", func(t *testing.T) {
		if _, err := raw.Exec(`UPDATE workspaces SET settings = ? WHERE id = ?`, "not json at all", ws.ID); err != nil {
			t.Errorf("a non-document value in an unconstrained JSON column was refused; without the "+
				"json_valid guard json_tree raises and every such write breaks: %v", err)
		}
	})

	// And the same column DOES refuse a real escape, so the guard above did not
	// simply disable the check.
	t.Run("the unconstrained JSON column still refuses a decoded escape", func(t *testing.T) {
		doc := `{"theme":"x` + textguard.EscNUL + `y"}`
		if _, err := raw.Exec(`UPDATE workspaces SET settings = ? WHERE id = ?`, doc, ws.ID); err == nil {
			t.Error("the json_valid guard disabled the check rather than guarding it")
		}
	})
}

// TestNULTriggersMatchTheList pins the committed migration against the Go list
// it is generated from — BYTE FOR BYTE.
//
// The first version compared trigger NAMES and counts, which codex round 1
// showed would pass a wrong BEFORE UPDATE OF clause, a wrong predicate, or a
// changed RAISE marker — all of which are the parts that do the work. Names are
// the one thing a hand edit is least likely to get wrong.
//
// If this fails: decide which side is right. If the LIST is right, regenerate
// with GEN_NUL_TRIGGERS=1 go test ./internal/store/ -run
// TestGenerateNULTriggerMigration. If the FILE is right, the list is wrong and
// regenerating would erase the evidence.
func TestNULTriggersMatchTheList(t *testing.T) {
	committed, err := os.ReadFile("migrations/" + nulTriggerMigration)
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	rendered := renderNULTriggerMigration()
	if string(committed) == rendered {
		return
	}

	// A diff a human can act on, rather than 2000 lines of "not equal".
	cl := strings.Split(string(committed), "\n")
	rl := strings.Split(rendered, "\n")
	for i := 0; i < len(cl) || i < len(rl); i++ {
		var c, r string
		if i < len(cl) {
			c = cl[i]
		}
		if i < len(rl) {
			r = rl[i]
		}
		if c != r {
			t.Fatalf("the committed migration and the list disagree, first at line %d:\n  committed: %q\n  rendered:  %q\n"+
				"(committed has %d lines, the list renders %d)", i+1, c, r, len(cl), len(rl))
		}
	}
	t.Fatalf("migration and list differ but no differing line was found — the comparison is broken")
}

// TestTriggerRefusalIsIndistinguishableFromLayerA discharges Ruling 2's
// condition.
//
// The second ring — user agents, IP addresses, attachment filenames — was
// admitted to Layer B on the condition that a header-derived value hitting a
// trigger must not surface as a 500 or a broken login. It cannot, because the
// trigger's abort is classified into the SAME typed error Layer A produces, so
// the handler's existing 400 mapping catches it without knowing which layer
// refused.
//
// Reaching the trigger from a GUARDED connection needs a column Layer A does
// not inspect, and the second ring is exactly that on the CURRENT binary only
// in the sense that Layer A checks values, not columns — so the honest way to
// exercise this is to classify the driver error directly and, separately, to
// prove an unguarded write produces a message the classifier recognises.
func TestTriggerRefusalIsIndistinguishableFromLayerA(t *testing.T) {
	// WHY THERE IS NO END-TO-END LEG HERE, stated rather than faked.
	//
	// Codex round 2 was right that testing classifyTriggerRefusal with a
	// synthetic error would pass even if the wrapper stopped calling it. My
	// first answer was an "integration" leg that wrote a nested-document value
	// through a guarded connection and asserted the typed error came back. It
	// PASSED — and for the wrong reason: the refusal came from LAYER A, whose
	// message ("parameter 1: value is a JSON document...") is the same TYPE, so
	// errors.As succeeded while the trigger was never involved.
	//
	// Reaching a trigger through a guarded connection needs a value Layer A
	// ACCEPTS and Layer B REFUSES. By construction there is none: both
	// implement the same predicate, and the four-way differential test asserts
	// exactly that they agree on the whole corpus. The unreachability IS the
	// property, so a test that manufactured a reachable case would be testing a
	// disagreement we have gone to some trouble to prevent.
	//
	// What is testable, and is: (a) the classifier's behaviour, below; (b) that
	// the marker it parses is the one the migration emits, below; and (c) that
	// every error path in the wrapper passes through it, structurally — the
	// arm-parity shape from the item-title unit, which exists because
	// "somebody will add a fifth return and forget" is the failure that
	// actually happens.
	// A FAKE DRIVER, driven through the real wrapper (codex round 3).
	//
	// The previous version grepped nulguard.go for the classifier's name, which
	// a dead or commented call would satisfy. This wraps a driver that returns
	// a marker-bearing error from each of the four entry points and asserts the
	// caller receives the TYPED error — so the routing is exercised rather than
	// read.
	t.Run("each wrapper entry point classifies a marker error", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			run  func(*sql.DB) error
		}{
			{"conn Exec", func(db *sql.DB) error { _, e := db.Exec("UPDATE t SET a = ?", "x"); return e }},
			{"conn Query", func(db *sql.DB) error { _, e := db.Query("SELECT ?", "x"); return e }},
			{"stmt Exec", func(db *sql.DB) error {
				st, e := db.Prepare("UPDATE t SET a = ?")
				if e != nil {
					return e
				}
				defer st.Close()
				_, e = st.Exec("x")
				return e
			}},
			{"stmt Query", func(db *sql.DB) error {
				st, e := db.Prepare("SELECT ?")
				if e != nil {
					return e
				}
				defer st.Close()
				_, e = st.Query("x")
				return e
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db := openFakeGuarded(t)
				defer db.Close()
				err := tc.run(db)
				if err == nil {
					t.Fatal("the fake driver must return an error")
				}
				var typed *InvalidTextParameterError
				if !errors.As(err, &typed) {
					t.Errorf("a marker error through %s did NOT classify — a Layer B refusal down this "+
						"path reaches the handler as a 500 instead of a 400; got %T: %v", tc.name, err, err)
				}
			})
		}
	})

	t.Run("a trigger abort classifies as the Layer A error", func(t *testing.T) {
		raw := errors.New("SQL logic error: pad_nul_invariant: activities.user_agent must not contain a NUL (1)")
		got := classifyTriggerRefusal(raw)
		var typed *InvalidTextParameterError
		if !errors.As(got, &typed) {
			t.Fatalf("trigger abort did not classify; got %T: %v", got, got)
		}
		if !strings.Contains(typed.Reason, "activities.user_agent") {
			t.Errorf("reason %q should name the column the rule is about", typed.Reason)
		}
	})

	t.Run("an unrelated driver error is left alone", func(t *testing.T) {
		// The control. Without it, a classifier that claimed every error would
		// pass the case above and turn every database fault into a 400.
		raw := errors.New("database is locked")
		if got := classifyTriggerRefusal(raw); got != raw {
			t.Errorf("an unrelated error was reclassified: %v", got)
		}
		if classifyTriggerRefusal(nil) != nil {
			t.Error("nil must stay nil")
		}
	})

	t.Run("the marker the classifier looks for is the one the triggers emit", func(t *testing.T) {
		// The two halves are in different files and different languages, so
		// this is the only thing keeping them in step. A migration regenerated
		// with a different message would otherwise silently stop being
		// classifiable, and every trigger refusal would become a 500.
		body, err := os.ReadFile("migrations/084_nul_invariant_triggers.sql")
		if err != nil {
			t.Fatalf("read migration: %v", err)
		}
		if !strings.Contains(string(body), nulTriggerMarker) {
			t.Fatalf("the migration does not contain %q — the classifier would never fire", nulTriggerMarker)
		}
		// And the exact shape the parser depends on.
		if !strings.Contains(string(body), nulTriggerMarker+": items.title must not") {
			t.Error("the RAISE message shape changed; classifyTriggerRefusal parses '<marker>: <table>.<col> must not'")
		}
	})
}

// TestLayerBAgreesWithTheCorpus lights the FOURTH leg of the four-way
// differential test — the one S1 built the harness for and left dark.
//
// The other three drive the same corpus through the HTTP gate (classed by
// request key), Layer A at the driver (classed by value shape), and native
// Postgres. This one drives it through a REAL SQLite write on an UNGUARDED
// connection, so what answers is the trigger and nothing else.
//
// With this leg live, one corpus is measured against four independent
// enforcers, which is what licenses two enforcement layers to coexist — the
// property DOC-2823 named as the deliverable rather than the guard itself.
func TestLayerBAgreesWithTheCorpus(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	ws := createTestWorkspace(t, s, "LayerBCorpus")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Corpus subject", "")

	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			// Same routing rule as the store and Postgres legs: a JSON-classed
			// value that is not valid JSON goes to the text column, because the
			// JSON column would reject it as malformed before the trigger was
			// ever consulted.
			var werr error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(c.Value))) {
				werr = execRaw(raw, `UPDATE workspaces SET settings = ? WHERE id = ?`, c.Value, ws.ID)
			} else {
				werr = execRaw(raw, `UPDATE items SET content = ? WHERE id = ?`, c.Value, item.ID)
			}

			refused := werr != nil && strings.Contains(werr.Error(), nulTriggerMarker)
			if refused != c.Refused {
				t.Errorf("LAYER B refused=%t, corpus says %t\nvalue: %q\nisJSON: %t\nerr: %v\n"+
					"why this case exists: %s", refused, c.Refused, c.Value, c.IsJSON, werr, c.Why)
			}
			if !c.Refused {
				if werr != nil {
					t.Fatalf("expected accepted, but the write failed for another reason: %v", werr)
				}
				// READ IT BACK. The comment used to claim persistence and the
				// code only checked that no error came back (codex round 1) —
				// which a trigger that silently discarded the row would also
				// satisfy, and which says nothing about whether the value
				// survived intact.
				var stored string
				var rerr error
				if c.IsJSON && json.Valid([]byte(strings.TrimSpace(c.Value))) {
					rerr = raw.QueryRow(`SELECT settings FROM workspaces WHERE id = ?`, ws.ID).Scan(&stored)
				} else {
					rerr = raw.QueryRow(`SELECT content FROM items WHERE id = ?`, item.ID).Scan(&stored)
				}
				if rerr != nil {
					t.Fatalf("read back: %v", rerr)
				}
				if stored != c.Value {
					t.Errorf("stored value differs from what was written\n  wrote: %q\n  read:  %q", c.Value, stored)
				}
			}
		})
	}
}

func execRaw(db *sql.DB, q string, args ...any) error {
	_, err := db.Exec(q, args...)
	return err
}

// TestNULTriggersSurviveATableRebuild regresses codex round 1's sharpest
// finding: SQLite drops a table's triggers when the table is dropped, this
// codebase rebuilds tables to change constraints (migrations 025, 055, 056,
// 057, 068, 072 all do), and migration 084 would never run again because it is
// recorded as applied.
//
// The consequence is worse than the FTS equivalent that warns and moves on. A
// missing FTS trigger breaks search visibly; a missing NUL trigger is silently
// no protection at all, against exactly the old-binary writer Layer B exists
// for.
//
// This simulates the rebuild by dropping the triggers, then runs the same
// re-assertion path startup uses.
func TestNULTriggersSurviveATableRebuild(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	ws := createTestWorkspace(t, s, "RebuildWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Rebuild subject", "")

	countTriggers := func() int {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='trigger' AND name LIKE 'pad_nul_%'`,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	before := countTriggers()
	if before == 0 {
		t.Fatal("no NUL triggers to begin with — the fixture proves nothing")
	}

	// The rebuild, simulated on one table.
	for _, tr := range []string{"pad_nul_items_content_ins", "pad_nul_items_content_upd"} {
		if _, err := s.db.Exec("DROP TRIGGER IF EXISTS " + tr); err != nil {
			t.Fatalf("drop %s: %v", tr, err)
		}
	}
	if countTriggers() != before-2 {
		t.Fatalf("the fixture did not actually drop the triggers")
	}

	// PROVE protection is gone first, so the restoration below is measured
	// against a real loss rather than against nothing.
	raw, err := sql.Open("sqlite", s.dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`,
		"unprotected"+textguard.NUL+"write", item.ID); err != nil {
		t.Fatalf("with the triggers dropped the write should succeed, showing the loss is real: %v", err)
	}

	// Now the startup path.
	if err := s.ensureNULTriggers(); err != nil {
		t.Fatalf("ensureNULTriggers: %v", err)
	}
	if got := countTriggers(); got != before {
		t.Errorf("after re-assertion there are %d triggers, want %d", got, before)
	}
	if _, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`,
		"still"+textguard.NUL+"bad", item.ID); err == nil {
		t.Error("protection was not actually restored — the trigger count matched but the rule does not hold")
	}

	// A STALE trigger — right name, wrong body — is the case a name-only check
	// can never repair, because CREATE TRIGGER IF NOT EXISTS sees the name and
	// does nothing forever (codex round 3). A mutation reverting the definition
	// comparison survived until this existed.
	t.Run("a same-name trigger with a no-op body is replaced", func(t *testing.T) {
		for _, tr := range []string{"pad_nul_items_content_ins", "pad_nul_items_content_upd"} {
			if _, err := s.db.Exec("DROP TRIGGER IF EXISTS " + tr); err != nil {
				t.Fatalf("drop %s: %v", tr, err)
			}
		}
		// A trigger that fires on the right table and event and does nothing.
		if _, err := s.db.Exec(`CREATE TRIGGER pad_nul_items_content_upd
			BEFORE UPDATE OF content ON items
			FOR EACH ROW WHEN 0
			BEGIN SELECT 1; END`); err != nil {
			t.Fatalf("install the stale trigger: %v", err)
		}
		if _, err := s.db.Exec(`CREATE TRIGGER pad_nul_items_content_ins
			BEFORE INSERT ON items
			FOR EACH ROW WHEN 0
			BEGIN SELECT 1; END`); err != nil {
			t.Fatalf("install the stale trigger: %v", err)
		}

		// The COUNT is now correct, which is exactly why a count check passed.
		if got := countTriggers(); got != before {
			t.Fatalf("fixture: expected the count to be restored to %d, got %d", before, got)
		}
		// And protection is gone.
		if _, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`,
			"stale"+textguard.NUL+"passes", item.ID); err != nil {
			t.Fatalf("with a no-op trigger the write should succeed, showing the loss is real: %v", err)
		}

		if err := s.ensureNULTriggers(); err != nil {
			t.Fatalf("ensureNULTriggers: %v", err)
		}
		if _, err := raw.Exec(`UPDATE items SET content = ? WHERE id = ?`,
			"still"+textguard.NUL+"bad", item.ID); err == nil {
			t.Error("a stale same-name trigger was not replaced — the check compares names, not definitions")
		}
	})

	// And the no-op case: re-asserting when nothing is missing must not fail
	// or duplicate.
	if err := s.ensureNULTriggers(); err != nil {
		t.Fatalf("re-asserting when nothing is missing must be a no-op: %v", err)
	}
	if got := countTriggers(); got != before {
		t.Errorf("a second re-assertion changed the count to %d", got)
	}
}

// ---- the fake driver, for the classifier routing test ----
//
// It implements exactly the optional interfaces the guard's registration
// asserts, so wrapConn takes the ordinary path, and returns a marker-bearing
// error from every execute/query entry point.

type fakeConn struct{}

func (fakeConn) Prepare(string) (driver.Stmt, error) { return fakeStmt{}, nil }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("no tx") } //nolint:staticcheck

func (fakeConn) PrepareContext(context.Context, string) (driver.Stmt, error) { return fakeStmt{}, nil }
func (fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return nil, errors.New("no tx")
}
func (fakeConn) Ping(context.Context) error         { return nil }
func (fakeConn) ResetSession(context.Context) error { return nil }

func (fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, errFakeTrigger
}
func (fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errFakeTrigger
}

type fakeStmt struct{}

func (fakeStmt) Close() error  { return nil }
func (fakeStmt) NumInput() int { return -1 }
func (fakeStmt) Exec([]driver.Value) (driver.Result, error) { //nolint:staticcheck
	return nil, errFakeTrigger
}
func (fakeStmt) Query([]driver.Value) (driver.Rows, error) { //nolint:staticcheck
	return nil, errFakeTrigger
}
func (fakeStmt) ExecContext(context.Context, []driver.NamedValue) (driver.Result, error) {
	return nil, errFakeTrigger
}
func (fakeStmt) QueryContext(context.Context, []driver.NamedValue) (driver.Rows, error) {
	return nil, errFakeTrigger
}

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

var errFakeTrigger = errors.New("SQL logic error: " + nulTriggerMarker + ": items.title must not contain a NUL (1)")

var fakeRegisterOnce sync.Once

func openFakeGuarded(t *testing.T) *sql.DB {
	t.Helper()
	fakeRegisterOnce.Do(func() { sql.Register("pad-fake-nulguard", guardDriver{base: fakeDriver{}}) })
	db, err := sql.Open("pad-fake-nulguard", "")
	if err != nil {
		t.Fatalf("open fake: %v", err)
	}
	return db
}
