package store

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// TestBinaryColumnCensus pins the one assumption the write guard's classing
// rests on: that []byte parameters in this store are BINARY, so exempting them
// from the NUL check refuses nothing legitimate and hides nothing.
//
// It is a census rather than an argument because this unit's whole history is
// source-level instruments missing things — a Sprintf-built statement, a
// multi-line SQL literal, a write site two separate greps each half-found. A
// census over the migration SQL answers "what binary columns exist" in a way a
// reader can re-run, and it FAILS when a new one appears, which is exactly when
// someone must re-examine whether []byte is still binary-only here.
//
// If you are here because this failed: you added a BLOB/BYTEA column. Decide
// whether anything binds TEXT to it, or text to any other column as []byte,
// then add it below.
func TestBinaryColumnCensus(t *testing.T) {
	want := map[string]bool{"item_yjs_updates.update_data": true}

	// One pattern per dialect spelling, applied to the raw migration text so
	// nothing depends on a Go-side model of the schema.
	colRe := regexp.MustCompile(`(?im)^\s*([a-z_]+)\s+(BLOB|BYTEA)\b`)
	tableRe := regexp.MustCompile(`(?is)CREATE\s+TABLE(?:\s+IF\s+NOT\s+EXISTS)?\s+([a-z_]+)\s*\((.*?)\n\s*\);`)
	// ALTER TABLE ... ADD COLUMN is the OTHER way a binary column enters a
	// schema, and the first version of this census could not see it (codex
	// round 1, finding 7) — which would have let exactly the change this test
	// exists to catch walk past it.
	alterRe := regexp.MustCompile(`(?im)ALTER\s+TABLE\s+([a-z_]+)\s+ADD\s+(?:COLUMN\s+)?(?:IF\s+NOT\s+EXISTS\s+)?([a-z_]+)\s+(BLOB|BYTEA)\b`)

	found := map[string]bool{}
	for _, dir := range []string{"migrations", "pgmigrations"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		var sqlFiles int
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			sqlFiles++
			body, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			for _, tbl := range tableRe.FindAllStringSubmatch(string(body), -1) {
				for _, col := range colRe.FindAllStringSubmatch(tbl[2], -1) {
					found[tbl[1]+"."+col[1]] = true
				}
			}
			for _, alt := range alterRe.FindAllStringSubmatch(string(body), -1) {
				found[alt[1]+"."+alt[2]] = true
			}
		}
		// The instrument must have read something, or "no binary columns
		// found" is a statement about the walk rather than about the schema.
		if sqlFiles == 0 {
			t.Fatalf("no .sql files read from %s — the census walked nothing", dir)
		}
	}

	if len(found) == 0 {
		t.Fatal("census found ZERO binary columns; item_yjs_updates.update_data is known to exist, so the instrument is broken, not the schema")
	}

	var extra, missing []string
	for c := range found {
		if !want[c] {
			extra = append(extra, c)
		}
	}
	for c := range want {
		if !found[c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if len(extra) > 0 {
		t.Errorf("new binary column(s) %v — the write guard exempts every []byte parameter from the NUL "+
			"check on the grounds that []byte means binary here. Re-examine that before adding these.", extra)
	}
	if len(missing) > 0 {
		t.Errorf("expected binary column(s) %v not found by the census — either they were removed or the "+
			"instrument stopped seeing them; both need a human", missing)
	}
}

// TestWriteGuardRefusesTheCorpus is Layer A's leg of the four-way differential
// test: the SAME corpus every other layer is measured against, driven through
// a REAL write to a REAL database.
//
// It is a real write rather than a call to checkParams because the point is
// coverage of the path, not of the predicate — textguard's own test already
// covers the predicate. What this asserts is that a value reaching the driver
// is refused there, whichever of the four receivers carried it.
func TestWriteGuardRefusesTheCorpus(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "NulGuard")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Guard subject", "")

	for _, c := range textguard.Corpus {
		t.Run(c.Name, func(t *testing.T) {
			// items.content is a user-text column; items.fields is JSON. The
			// corpus case's own classing chooses which one carries it, which
			// is how this leg measures that the STORE's classing agrees with
			// the corpus's.
			//
			// A JSON-CLASSED value that is not valid JSON goes to the TEXT
			// column, matching the native-Postgres leg's routing and for the
			// same reason: the fields column would reject it as malformed
			// before the guard was ever consulted, and the case would then
			// "pass" while measuring SQLite's JSON parser instead of this
			// guard. The strengthened accepted-case assertion caught exactly
			// that (codex round 1, finding 7).
			var err error
			if c.IsJSON && json.Valid([]byte(strings.TrimSpace(c.Value))) {
				fields := c.Value
				_, err = s.UpdateItem(item.ID, models_ItemUpdateFields(fields))
			} else {
				content := c.Value
				_, err = s.UpdateItem(item.ID, models_ItemUpdateContent(content))
			}

			refused := err != nil && strings.Contains(err.Error(), ErrInvalidTextParameter.Error())
			// An ACCEPTED case must actually SUCCEED, not merely avoid the
			// guard (codex round 1, finding 7). Without this, a write failing
			// for any other reason — a schema rejection, a constraint, a typo
			// in the fixture — read as "the guard let it through" and the case
			// proved nothing.
			if !c.Refused && err != nil {
				t.Fatalf("case is expected to be accepted but the write FAILED for another reason: %v\nwhy this case exists: %s", err, c.Why)
			}
			if refused != c.Refused {
				t.Errorf("store write refused=%t, corpus says %t\nvalue: %q\nisJSON: %t\nerr: %v\nwhy this case exists: %s",
					refused, c.Refused, c.Value, c.IsJSON, err, c.Why)
			}
		})
	}
}

// Small constructors, so the corpus loop reads as the assertion rather than as
// struct plumbing.
func models_ItemUpdateContent(v string) (u models.ItemUpdate) { u.Content = &v; return }
func models_ItemUpdateFields(v string) (u models.ItemUpdate)  { u.Fields = &v; return }

// TestWriteGuardCoversTheQueryPath is the leg the lead required before this
// guard's coverage claim could be written, and a mutation is why it exists in
// this shape: removing checkParams from guardConn.QueryContext SURVIVED the
// corpus leg above, because every write that leg makes goes through Exec.
//
// Writes DO ride the Query path in this store — three of them today:
//
//	password_resets.go     UPDATE ... RETURNING user_id   (s.db.QueryRow)
//	email_verification.go  UPDATE ... RETURNING user_id   (tx.QueryRow)
//	yjs_updates.go         INSERT ... RETURNING id        (s.db.QueryRow, Postgres)
//
// so an Exec-only guard would have left a real hole rather than a theoretical
// one. This drives a RETURNING write directly, because what is under test is
// the PATH: the predicate is covered by textguard's own tests, and the corpus
// leg covers Exec.
//
// It also answers the lead's other branch. If those three writes are ever
// rewritten away, this test does not become vacuous — it constructs its own
// RETURNING statement, so it keeps guarding the path against the day someone
// adds the next one.
func TestWriteGuardCoversTheQueryPath(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "NulGuardQueryPath")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Query path subject", "")

	// A NUL-bearing value through UPDATE ... RETURNING — the exact shape
	// password_resets.go and email_verification.go use.
	var returned string
	err := s.db.QueryRow(
		s.q(`UPDATE items SET content = ? WHERE id = ? RETURNING id`),
		"poisoned"+textguard.NUL+"content", item.ID,
	).Scan(&returned)
	if err == nil {
		t.Fatal("a NUL-bearing parameter on the QUERY path was accepted; the guard covers Exec only")
	}
	if !strings.Contains(err.Error(), ErrInvalidTextParameter.Error()) {
		t.Errorf("refused, but not by the guard: %v", err)
	}

	// The row must be untouched — a guard that refuses after writing is not a
	// guard.
	after, gerr := s.GetItem(item.ID)
	if gerr != nil {
		t.Fatalf("GetItem: %v", gerr)
	}
	if textguard.ContainsNUL(after.Content) {
		t.Error("the refused value reached the row anyway")
	}

	// CONTROL: a clean value on the same path still works, so the leg
	// discriminates rather than proving that RETURNING is simply broken here.
	var ok string
	if err := s.db.QueryRow(
		s.q(`UPDATE items SET content = ? WHERE id = ? RETURNING id`),
		"clean content", item.ID,
	).Scan(&ok); err != nil {
		t.Fatalf("a clean value on the query path must succeed: %v", err)
	}
	if ok != item.ID {
		t.Errorf("RETURNING gave %q, want %q", ok, item.ID)
	}
}

// TestWriteGuardCoversPreparedStatements covers the route that JUSTIFIES this
// guard living at the driver rather than at *sql.DB.
//
// A wrapper one layer up cannot see a prepared statement's arguments: the
// statement is prepared once and executed against the driver directly. That is
// the measurement that retired the in-package seam shape, and leaving it
// untested would mean the design's central reason was the one thing unverified.
// Both prepared mutants — dropping the check from guardStmt.ExecContext and
// from guardStmt.QueryContext — SURVIVED until this existed.
//
// The store prepares statements today in agent_roles.go (two sites, binding
// ids and ints). Those carry no user text, so this test constructs its own
// rather than borrowing one: what is under test is the ROUTE, and it must stay
// covered when the next prepared statement does carry text.
func TestWriteGuardCoversPreparedStatements(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "NulGuardPrepared")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Prepared subject", "")

	t.Run("prepared Exec", func(t *testing.T) {
		stmt, err := s.db.Prepare(s.q(`UPDATE items SET content = ? WHERE id = ?`))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer stmt.Close()

		if _, err := stmt.Exec("poisoned"+textguard.NUL+"content", item.ID); err == nil {
			t.Error("a NUL-bearing parameter through a PREPARED statement was accepted — a *sql.DB-level wrapper would miss exactly this")
		} else if !strings.Contains(err.Error(), ErrInvalidTextParameter.Error()) {
			t.Errorf("refused, but not by the guard: %v", err)
		}

		// Control on the same statement handle.
		if _, err := stmt.Exec("clean content", item.ID); err != nil {
			t.Errorf("a clean value through the same prepared statement must succeed: %v", err)
		}
	})

	t.Run("prepared Query", func(t *testing.T) {
		stmt, err := s.db.Prepare(s.q(`SELECT id FROM items WHERE workspace_id = ? AND content = ?`))
		if err != nil {
			t.Fatalf("prepare: %v", err)
		}
		defer stmt.Close()

		// A READ carrying a NUL is refused too. That is deliberate and worth
		// stating: on Postgres such a parameter is a query error rather than a
		// miss, so refusing it uniformly is what stops the two dialects
		// answering differently — the BUG-2831 lesson applied to reads.
		rows, err := stmt.Query(ws.ID, "looking"+textguard.NUL+"for")
		if err == nil {
			_ = rows.Close()
			t.Error("a NUL-bearing parameter through a prepared QUERY was accepted")
		} else if !strings.Contains(err.Error(), ErrInvalidTextParameter.Error()) {
			t.Errorf("refused, but not by the guard: %v", err)
		}

		clean, err := stmt.Query(ws.ID, "clean content")
		if err != nil {
			t.Fatalf("a clean read through the same prepared statement must succeed: %v", err)
		}
		_ = clean.Close()
	})

	// The row survived every refusal above.
	after, err := s.GetItem(item.ID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if textguard.ContainsNUL(after.Content) {
		t.Error("a refused value reached the row anyway")
	}
}

// TestStoreOverRefusalsStillOverRefuse pins the known divergence between this
// guard and every other layer: values that are legitimate TEXT and are refused
// anyway, because the guard classes by value shape rather than by column.
//
// It fails when one STOPS being refused, which is the interesting event — it
// means the guard gained real column knowledge, and the case should move into
// textguard.Corpus where every layer is held to it.
func TestStoreOverRefusalsStillOverRefuse(t *testing.T) {
	// NOT a skip, for the same reason as KnownGaps: an empty slice would let
	// someone delete the record of a divergence and see green (codex round 2).
	const wantOverRefusals = 1
	if len(textguard.StoreOverRefusals) != wantOverRefusals {
		t.Fatalf("StoreOverRefusals has %d entries, expected %d. If one was PAID DOWN, move its case into "+
			"textguard.Corpus and update this count in the same edit.",
			len(textguard.StoreOverRefusals), wantOverRefusals)
	}
	s := testStore(t)
	ws := createTestWorkspace(t, s, "OverRefusal")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Over-refusal subject", "")

	for _, c := range textguard.StoreOverRefusals {
		t.Run(c.Name, func(t *testing.T) {
			content := c.Value
			_, err := s.UpdateItem(item.ID, models.ItemUpdate{Content: &content})
			refused := err != nil && strings.Contains(err.Error(), ErrInvalidTextParameter.Error())
			if !refused {
				t.Errorf("this over-refusal has been PAID DOWN: the store now accepts %q as text, which "+
					"is the correct answer. Move this case into textguard.Corpus so every layer is held "+
					"to it, and delete it from StoreOverRefusals.\nwhy it was recorded: %s", c.Value, c.Why)
			}
		})
	}
}

// TestWrapperAdvertisesExactlyWhatTheBaseDoes is the guard against the mistake
// this wrapper has now made twice.
//
// database/sql BRANCHES on whether an optional interface is present, so a
// wrapper advertising one the base lacks changes behaviour rather than adding a
// no-op — it told database/sql that pgx validates connections (it does not) and
// that sqlite converts its own arguments (it does not). The fix for THAT then
// advertised DriverContext for sqlite, which lacks it, and every SQLite open
// failed instantly. Loud that time; the same mistake on a branch-only interface
// is silent.
//
// So the property is pinned rather than reasoned about: for every optional
// interface the wrapper varies over, wrapped and base must agree exactly.
func TestWrapperAdvertisesExactlyWhatTheBaseDoes(t *testing.T) {
	if err := registerGuardedDrivers(); err != nil {
		t.Fatalf("register: %v", err)
	}

	type pair struct{ base, guarded, dsn string }
	pairs := []pair{{"sqlite", guardedSQLiteDriver, t.TempDir() + "/parity.db"}}
	if dsn := os.Getenv("PAD_TEST_POSTGRES_URL"); dsn != "" {
		pairs = append(pairs, pair{"pgx", guardedPostgresDriver, dsn})
	} else {
		t.Log("pgx leg skipped: PAD_TEST_POSTGRES_URL not set")
	}

	for _, p := range pairs {
		t.Run(p.base, func(t *testing.T) {
			baseDB, err := sql.Open(p.base, p.dsn)
			if err != nil {
				t.Fatalf("open base: %v", err)
			}
			defer baseDB.Close()
			wrapDB, err := sql.Open(p.guarded, p.dsn)
			if err != nil {
				t.Fatalf("open guarded: %v", err)
			}
			defer wrapDB.Close()

			// Driver level.
			_, baseCtx := baseDB.Driver().(driver.DriverContext)
			_, wrapCtx := wrapDB.Driver().(driver.DriverContext)
			if baseCtx != wrapCtx {
				t.Errorf("DriverContext: base=%t wrapped=%t — they must match", baseCtx, wrapCtx)
			}

			// Connection level.
			ifaces := func(db *sql.DB) map[string]bool {
				conn, err := db.Conn(t.Context())
				if err != nil {
					t.Fatalf("conn: %v", err)
				}
				defer conn.Close()
				out := map[string]bool{}
				if rerr := conn.Raw(func(dc any) error {
					out["Pinger"] = isIface[driver.Pinger](dc)
					out["SessionResetter"] = isIface[driver.SessionResetter](dc)
					out["Validator"] = isIface[driver.Validator](dc)
					out["NamedValueChecker"] = isIface[driver.NamedValueChecker](dc)
					out["ExecerContext"] = isIface[driver.ExecerContext](dc)
					out["QueryerContext"] = isIface[driver.QueryerContext](dc)
					out["ConnPrepareContext"] = isIface[driver.ConnPrepareContext](dc)
					out["ConnBeginTx"] = isIface[driver.ConnBeginTx](dc)
					return nil
				}); rerr != nil {
					t.Fatalf("raw: %v", rerr)
				}
				return out
			}
			b, w := ifaces(baseDB), ifaces(wrapDB)
			if len(b) == 0 {
				t.Fatal("the probe read no interfaces — the instrument is broken, not the wrapper")
			}
			for name, want := range b {
				if w[name] != want {
					t.Errorf("conn %s: base=%t wrapped=%t — advertising an interface the base lacks (or "+
						"hiding one it has) changes database/sql's behaviour", name, want, w[name])
				}
			}
		})
	}
}

// TestWriteGuardSeesThroughDriverValuer regresses codex round 2's second P1.
//
// checkParams originally type-asserted `string`. pgx implements
// driver.NamedValueChecker and ACCEPTS a sql.NullString unchanged rather than
// letting database/sql's default converter unwrap it — so the guard never saw
// the text. Measured before the fix: on Postgres a NUL-bearing sql.NullString
// passed the guard entirely and was refused by the SERVER with SQLSTATE 22021,
// i.e. as a 500, while the identical value on SQLite got the typed 400. The
// dialect split, reappearing in the response shape.
//
// internal/store/wiki_links.go binds sql.NullString today, so this is a live
// parameter shape rather than a constructed one.
func TestWriteGuardSeesThroughDriverValuer(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "ValuerGuard")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "Valuer subject", "")

	poisoned := sql.NullString{String: "poisoned" + textguard.NUL + "content", Valid: true}
	_, err := s.db.Exec(s.q(`UPDATE items SET content = ? WHERE id = ?`), poisoned, item.ID)
	if err == nil {
		t.Fatal("a NUL-bearing sql.NullString was accepted")
	}
	if !strings.Contains(err.Error(), ErrInvalidTextParameter.Error()) {
		t.Errorf("refused, but NOT by the guard: %v\nOn Postgres this is the tell — the server's own "+
			"SQLSTATE 22021 instead of our typed refusal means the value went past Layer A and the "+
			"caller gets a 500 where SQLite gives a 400", err)
	}

	// Controls: a clean NullString still writes, and a NULL one is untouched.
	clean := sql.NullString{String: "clean content", Valid: true}
	if _, err := s.db.Exec(s.q(`UPDATE items SET content = ? WHERE id = ?`), clean, item.ID); err != nil {
		t.Errorf("a clean sql.NullString must still write: %v", err)
	}
	null := sql.NullString{Valid: false}
	if _, err := s.db.Exec(s.q(`UPDATE items SET content = ? WHERE id = ?`), null, item.ID); err != nil {
		t.Errorf("a NULL sql.NullString must still write: %v", err)
	}
}
