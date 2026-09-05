package store

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/textguard"
)

// dropAllNULTriggers turns the store's database into a pre-S2 one for the
// duration of a test, so a raw handle can plant the LEGACY rows this unit
// exists to find. Restores them before returning to the caller's control.
//
// A raw *sql.DB opened on the same file is what an old binary is: no Layer A
// wrapper. With the triggers gone too, neither enforcement layer is present,
// which is exactly the window BUG-2813 describes and the state BUG-2810's rows
// were written in.
func plantLegacyRows(t *testing.T, s *Store, plant func(raw *sql.DB)) {
	t.Helper()

	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer raw.Close()

	names, err := nulTriggersIn(raw)
	if err != nil {
		t.Fatalf("list triggers: %v", err)
	}
	for name := range names {
		if _, err := raw.Exec(`DROP TRIGGER IF EXISTS "` + name + `"`); err != nil {
			t.Fatalf("drop %s: %v", name, err)
		}
	}

	plant(raw)

	// Put the database back the way a real one is BEFORE the scan runs. A scan
	// measured against a database with no triggers would not be measuring the
	// deployment it exists for, and the repair's writes have to pass Layer B.
	if _, err := s.ensureNULTriggersReporting(); err != nil {
		t.Fatalf("restore triggers: %v", err)
	}
}

// TestScanNULFindsThePlantedPopulation drives every shape the counter has to
// tell apart, including the two it must NOT report.
func TestScanNULFindsThePlantedPopulation(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("the scan is SQLite-only; Postgres cannot hold the state")
	}

	ws := createTestWorkspace(t, s, "ScanWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	dirtyText := createTestItem(t, s, ws.ID, col.ID, "clean title", "clean body")
	dirtyJSON := createTestItem(t, s, ws.ID, col.ID, "second", "body")
	literalOnly := createTestItem(t, s, ws.ID, col.ID, "third", "body")
	cleanItem := createTestItem(t, s, ws.ID, col.ID, "fourth", "body")

	esc := textguard.EscNUL
	backslash := esc[:1]

	plantLegacyRows(t, s, func(raw *sql.DB) {
		// (1) raw NUL in a TEXT column.
		mustExec(t, raw, `UPDATE items SET title = ? WHERE id = ?`,
			"bad"+textguard.NUL+"title", dirtyText.ID)
		// (2) live escape in a JSON column.
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`,
			`{"note":"x`+esc+`y"}`, dirtyJSON.ID)
		// (3) raw NUL inside a JSON column — a defect of the stored bytes
		//     rather than of the document, and a different repair pass.
		mustExec(t, raw, `UPDATE items SET tags = ? WHERE id = ?`,
			`["a`+textguard.NUL+`b"]`, dirtyJSON.ID)
		// (4) THE NEGATIVE CONTROL. A doubled backslash makes the six
		//     characters literal text: the SQL pre-filter matches it and the
		//     predicate must not. A scan that reports this row has a false
		//     positive of exactly the kind BUG-2803's parity filter had.
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`,
			`{"note":"x`+backslash+esc+`y"}`, literalOnly.ID)
		// (5) THE OTHER NEGATIVE CONTROL. The escape in a TEXT column is six
		//     ordinary characters and is not a violation anywhere.
		mustExec(t, raw, `UPDATE items SET content = ? WHERE id = ?`,
			"writing about "+esc+" in a doc", literalOnly.ID)
		// (6) A table with NO declared primary key, addressed by rowid.
		mustExec(t, raw,
			`INSERT INTO item_wiki_links (source_item_id, target_kind, target_title, position)
			 VALUES (?, 'title', ?, 0)`,
			cleanItem.ID, "link"+textguard.NUL+"title")
	})

	report, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !report.Applicable {
		t.Fatalf("scan reported not applicable on SQLite: %s", report.Reason)
	}

	// Every protected column must exist in a freshly migrated database. A
	// non-empty ColumnsAbsent here means the list names something the schema
	// does not have, which the scan would silently skip.
	if len(report.ColumnsAbsent) != 0 {
		t.Errorf("protected columns missing from the live schema: %v", report.ColumnsAbsent)
	}
	if want := len(NULProtectedColumns()); report.ColumnsScanned != want {
		t.Errorf("scanned %d columns, list has %d", report.ColumnsScanned, want)
	}

	found := map[string]NULViolation{}
	for _, v := range report.Violations {
		found[v.Table+"."+v.Column] = v
	}

	for _, want := range []struct {
		key        string
		rawNUL     bool
		escapedNUL bool
		workspace  string
	}{
		{"items.title", true, false, ws.ID},
		{"items.fields", false, true, ws.ID},
		{"items.tags", true, false, ws.ID},
		{"item_wiki_links.target_title", true, false, ""},
	} {
		v, ok := found[want.key]
		if !ok {
			t.Errorf("%s: not reported", want.key)
			continue
		}
		if v.RawNUL != want.rawNUL || v.EscapedNUL != want.escapedNUL {
			t.Errorf("%s: kind mismatch — raw=%v escaped=%v, want raw=%v escaped=%v",
				want.key, v.RawNUL, v.EscapedNUL, want.rawNUL, want.escapedNUL)
		}
		if v.WorkspaceID != want.workspace {
			t.Errorf("%s: workspace %q, want %q", want.key, v.WorkspaceID, want.workspace)
		}
	}

	// items.content carried the escape as ordinary text and must not appear;
	// nor may the doubled-backslash document, which lives in items.fields and
	// would show up as a SECOND items.fields violation.
	if _, reported := found["items.content"]; reported {
		t.Error("items.content reported: the escape in a TEXT column is six ordinary characters")
	}
	fieldsCount := report.ByColumn()["items.fields"]
	if fieldsCount != 1 {
		t.Errorf("items.fields reported %d times, want exactly 1 — the doubled-backslash document "+
			"matches the SQL pre-filter and must be dropped by the predicate", fieldsCount)
	}

	// The rowid-addressed row must carry a usable address.
	if v, ok := found["item_wiki_links.target_title"]; ok {
		if _, hasRowid := v.Key["rowid"]; !hasRowid {
			t.Errorf("item_wiki_links declares no primary key; expected a rowid address, got %v", v.Key)
		}
	}

	if got := report.ByWorkspace()[ws.ID]; got != 3 {
		t.Errorf("workspace %s has %d violations, want 3", ws.ID, got)
	}
}

// TestRepairNULRepairsThePopulationAndIsIdempotent is the other half: after the
// repair the database satisfies the invariant, and the values that were never
// violating are byte-identical.
func TestRepairNULRepairsThePopulationAndIsIdempotent(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	ws := createTestWorkspace(t, s, "RepairWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	dirty := createTestItem(t, s, ws.ID, col.ID, "clean", "body")
	literalOnly := createTestItem(t, s, ws.ID, col.ID, "second", "body")

	esc := textguard.EscNUL
	backslash := esc[:1]
	literalDoc := `{"note":"x` + backslash + esc + `y"}`

	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `UPDATE items SET title = ? WHERE id = ?`, "bad"+textguard.NUL+"title", dirty.ID)
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`, `{"note":"x`+esc+`y"}`, dirty.ID)
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`, literalDoc, literalOnly.ID)
	})

	report, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("repair failures: %+v", report.Failed)
	}
	if len(report.Repaired) != 2 {
		t.Errorf("repaired %d values, want 2 (%+v)", len(report.Repaired), report.Repaired)
	}

	// The database now satisfies the invariant.
	after, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("re-scan: %v", err)
	}
	if after.Total() != 0 {
		t.Errorf("scan after repair still reports %d violations: %v", after.Total(), after.Violations)
	}

	// The repaired values carry U+FFFD where the NUL was, and nothing else
	// changed. Asserting the CONTENT rather than only the count is what stops a
	// repair that blanks the column from passing.
	var title, fields string
	if err := s.db.QueryRow(`SELECT title, fields FROM items WHERE id = ?`, dirty.ID).
		Scan(&title, &fields); err != nil {
		t.Fatalf("read repaired row: %v", err)
	}
	if want := "bad" + textguard.Replacement + "title"; title != want {
		t.Errorf("title = %q, want %q", title, want)
	}
	if want := `{"note":"x` + textguard.ReplacementEscape + `y"}`; fields != want {
		t.Errorf("fields = %q, want %q", fields, want)
	}

	// The row that only ever held literal text is untouched, BYTE FOR BYTE.
	var untouched string
	if err := s.db.QueryRow(`SELECT fields FROM items WHERE id = ?`, literalOnly.ID).Scan(&untouched); err != nil {
		t.Fatalf("read literal row: %v", err)
	}
	if untouched != literalDoc {
		t.Errorf("a value nobody complained about was rewritten\n  before: %q\n  after:  %q", literalDoc, untouched)
	}

	// Running it again does nothing, which is what makes a partial run
	// resumable rather than a state somebody has to reason about.
	second, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("second repair: %v", err)
	}
	if len(second.Repaired) != 0 || len(second.Failed) != 0 {
		t.Errorf("second pass was not a no-op: repaired=%d failed=%+v", len(second.Repaired), second.Failed)
	}
}

// TestRepairNULRefusesToRewriteAPrimaryKey pins the one shape the repair
// deliberately leaves alone.
//
// email_optouts(email) is both the primary key and a protected column. A
// U+FFFD substitution there rewrites the row's identity and can land on top of
// an existing row — silently merging two opt-out records, which in this table
// means somebody starts receiving mail again.
func TestRepairNULRefusesToRewriteAPrimaryKey(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	bad := "someone" + textguard.NUL + "@example.com"
	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `INSERT INTO email_optouts (email) VALUES (?)`, bad)
	})

	report, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}

	var skipped *NULRepairSkip
	for i := range report.Skipped {
		if report.Skipped[i].Violation.Table == "email_optouts" {
			skipped = &report.Skipped[i]
		}
	}
	if skipped == nil {
		t.Fatalf("email_optouts.email was not skipped; buckets: repaired=%+v skipped=%+v failed=%+v",
			report.Repaired, report.Skipped, report.Failed)
	}
	if !strings.Contains(skipped.Reason, "primary key") {
		t.Errorf("skip reason does not name the cause: %q", skipped.Reason)
	}

	// And the row is genuinely untouched, rather than reported as skipped
	// while having been written anyway.
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM email_optouts WHERE instr(email, char(0)) > 0`,
	).Scan(&count); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if count != 1 {
		t.Errorf("the skipped row was modified: %d rows still carry the NUL, want 1", count)
	}
}

// TestScanNULOnPostgresSaysWhyItDidNotLook guards the one thing a zero report
// must never be mistaken for.
func TestScanNULOnPostgresSaysWhyItDidNotLook(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() == DriverSQLite {
		t.Skip("this leg is about the Postgres arm")
	}
	report, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if report.Applicable {
		t.Error("the scan claims to have scanned a Postgres database")
	}
	if report.Reason == "" {
		t.Error("a not-applicable report with no reason is indistinguishable from a clean one")
	}
	if report.Total() != 0 {
		t.Errorf("not-applicable report carries %d violations", report.Total())
	}
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %s: %v", query, err)
	}
}

// TestScanCostFiguresMatchTheList keeps ScanNUL's doc comment honest.
//
// That comment tells an operator how much work the scan is — one unindexed
// scan per protected column — and quotes the number. A figure in prose that
// nothing checks is a figure that silently stops being true the next time a
// column joins the list, which in this cluster has happened in almost every
// round. If this fails, update the comment rather than the numbers here: the
// list is the source and the comment is the copy.
func TestScanCostFiguresMatchTheList(t *testing.T) {
	const (
		wantTotal = 132
		wantJSON  = 24
	)

	cols := NULProtectedColumns()
	gotJSON := 0
	for _, c := range cols {
		if c.Class == classJSON {
			gotJSON++
		}
	}

	if len(cols) != wantTotal || gotJSON != wantJSON {
		t.Errorf("ScanNUL's doc comment says %d protected columns (%d JSON, %d text); the list now has "+
			"%d (%d JSON, %d text). Update the comment.",
			wantTotal, wantJSON, wantTotal-wantJSON, len(cols), gotJSON, len(cols)-gotJSON)
	}
}

// TestRepairNULAddressesARowidOnlyTable covers the one row shape whose ADDRESS
// is not an id.
//
// item_wiki_links declares no primary key, so the scan addresses it by `rowid`
// — an INTEGER column, scanned into a Go string and bound back as one. That
// round trip is the only place in this unit where the address could silently
// stop selecting the row, and an UPDATE matching nothing would otherwise commit
// and be reported as a repair.
//
// Driven separately from the main repair test because the main one plants only
// id-addressed rows, and a leg that never exercises the rowid path would let
// that binding break without a failure.
func TestRepairNULAddressesARowidOnlyTable(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	ws := createTestWorkspace(t, s, "RowidWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	item := createTestItem(t, s, ws.ID, col.ID, "linked", "body")

	plantLegacyRows(t, s, func(raw *sql.DB) {
		// Two rows, so a repair that matched the WRONG one — or all of them —
		// is distinguishable from a repair that matched its own.
		mustExec(t, raw,
			`INSERT INTO item_wiki_links (source_item_id, target_kind, target_title, position)
			 VALUES (?, 'title', ?, 0)`, item.ID, "bad"+textguard.NUL+"link")
		mustExec(t, raw,
			`INSERT INTO item_wiki_links (source_item_id, target_kind, target_title, position)
			 VALUES (?, 'title', ?, 1)`, item.ID, "innocent bystander")
	})

	report, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("repair failures: %+v", report.Failed)
	}
	if len(report.Repaired) != 1 {
		t.Fatalf("repaired %d values, want 1: %+v", len(report.Repaired), report.Repaired)
	}
	if _, addressed := report.Repaired[0].Key["rowid"]; !addressed {
		t.Errorf("the row was addressed by %v, not by rowid", report.Repaired[0].Key)
	}

	var repaired, bystander string
	if err := s.db.QueryRow(
		`SELECT target_title FROM item_wiki_links WHERE position = 0`).Scan(&repaired); err != nil {
		t.Fatalf("read repaired: %v", err)
	}
	if err := s.db.QueryRow(
		`SELECT target_title FROM item_wiki_links WHERE position = 1`).Scan(&bystander); err != nil {
		t.Fatalf("read bystander: %v", err)
	}
	if want := "bad" + textguard.Replacement + "link"; repaired != want {
		t.Errorf("repaired title = %q, want %q", repaired, want)
	}
	if bystander != "innocent bystander" {
		t.Errorf("the neighbouring row was rewritten too: %q", bystander)
	}
}

// TestScanNULSurvivesANullWorkspaceID is a regression test for a scan that
// could not run on the databases it exists for.
//
// Several protected tables carry a NULLABLE workspace_id — activities,
// api_tokens, mcp_audit_log — and the scan selected it into a plain *string,
// which fails with "converting NULL to string is unsupported" and takes the
// whole scan down with it, along with the repair and the migrate-to-pg
// preflight that call it. The failure needs a VIOLATING row in such a table, so
// it was invisible to every fixture that planted its rows in items.
//
// Verified to fail against the unfixed code: the scan returned
// `scan activities.actor row: sql: Scan error ... converting NULL to string`.
func TestScanNULSurvivesANullWorkspaceID(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	plantLegacyRows(t, s, func(raw *sql.DB) {
		// workspace_id omitted, so it is NULL — the shape an instance-wide
		// activity row has.
		mustExec(t, raw,
			`INSERT INTO activities (id, action, actor, source, metadata, created_at)
			 VALUES ('act-nul', 'created', ?, 'cli', '{}', '2026-01-01')`,
			"agent"+textguard.NUL+"name")
	})

	report, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan failed on a row with a NULL workspace_id: %v", err)
	}

	var found *NULViolation
	for i := range report.Violations {
		if report.Violations[i].Table == "activities" && report.Violations[i].Column == "actor" {
			found = &report.Violations[i]
		}
	}
	if found == nil {
		t.Fatalf("activities.actor not reported: %v", report.Violations)
	}
	if found.WorkspaceID != "" {
		t.Errorf("workspace attribution = %q, want empty for a NULL workspace_id", found.WorkspaceID)
	}
	if found.Key["id"] != "act-nul" {
		t.Errorf("row address = %v, want id=act-nul", found.Key)
	}

	// And it repairs, so the NULL only affected attribution rather than
	// addressing.
	rep, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(rep.Failed) != 0 {
		t.Fatalf("repair failures: %+v", rep.Failed)
	}
	var actor string
	if err := s.db.QueryRow(`SELECT actor FROM activities WHERE id = 'act-nul'`).Scan(&actor); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if want := "agent" + textguard.Replacement + "name"; actor != want {
		t.Errorf("actor = %q, want %q", actor, want)
	}
}

// TestRepairNULSkipsARowItCannotAddress covers the second way a row's address
// can be unusable: a NUL in a key column the LIST does not protect, on a row
// whose violation is somewhere else.
//
// The address is then unbindable — Layer A checks every parameter, including a
// WHERE clause's — so the repair explains it instead of letting the driver
// return "invalid text parameter: parameter 2", which is the same information
// phrased as a fault in the repair rather than a property of the row.
//
// platform_settings is the fixture because its key IS its primary key and is
// NOT in the protected list, while its `value` column is: exactly the shape.
func TestRepairNULSkipsARowItCannotAddress(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw,
			`INSERT INTO platform_settings (key, value, updated_at) VALUES (?, ?, '2026-01-01')`,
			"branding"+textguard.NUL+"key", "site"+textguard.NUL+"name")
	})

	report, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}

	if len(report.Failed) != 0 {
		t.Errorf("the row was reported as a FAILURE rather than an explained skip: %+v", report.Failed)
	}

	var skipped *NULRepairSkip
	for i := range report.Skipped {
		if report.Skipped[i].Violation.Table == "platform_settings" {
			skipped = &report.Skipped[i]
		}
	}
	if skipped == nil {
		t.Fatalf("platform_settings.value not skipped; buckets: repaired=%+v skipped=%+v failed=%+v",
			report.Repaired, report.Skipped, report.Failed)
	}
	if !strings.Contains(skipped.Reason, "contains a NUL") || !strings.Contains(skipped.Reason, "address") {
		t.Errorf("the skip reason does not explain what an operator is looking at: %q", skipped.Reason)
	}

	// And it is genuinely untouched, rather than reported as skipped while
	// having been written anyway.
	var count int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM platform_settings WHERE instr(value, char(0)) > 0`).Scan(&count); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if count != 1 {
		t.Errorf("the skipped row was modified: %d rows still carry the NUL, want 1", count)
	}
}

// TestScanNULInheritsTheRecordedKnownGaps pins a MISS, on purpose.
//
// textguard.KnownGaps are values every layer currently answers wrong together —
// today, a JSON document with LITERAL duplicate keys, where the decode keeps
// the last one and a NUL in the first is never seen. DOC-2823 requires the
// layers to share that blind spot rather than diverge, and says Layer A "must
// NOT quietly fix either gap on its own". The scan shares the same predicate,
// so it inherits it, and that is the designed behaviour rather than an
// oversight (raised as a finding in codex round 3).
//
// The CONSEQUENCE is handled elsewhere rather than accepted here. PostgreSQL
// refuses such a value, so letting the class fall on the floor would have meant
// the migrate-to-pg preflight promising a migration that then failed mid-copy —
// which is what an earlier version of this unit did, until the day-54 ruling on
// PR #1233. Those rows are now the SUSPECT class, and the destination decides
// them. What stays true, and is what this test asserts, is that the PREDICATE
// still does not see them: the fix is a second mechanism beside the predicate,
// not a change to it.
//
// This test fails when the gap CLOSES, which is the signal that BUG-2812 has
// landed and this file should move the case into the covered set — the same
// direction, and the same reason, as textguard's TestKnownGapsStillGap. See
// also TestSuspectsCollapseWhenBUG2812Lands, which names what to delete.
func TestScanNULInheritsTheRecordedKnownGaps(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}
	if len(textguard.KnownGaps) == 0 {
		t.Skip("no recorded gaps")
	}

	ws := createTestWorkspace(t, s, "GapWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	for _, gap := range textguard.KnownGaps {
		if !gap.IsJSON {
			continue
		}
		t.Run(gap.Name, func(t *testing.T) {
			item := createTestItem(t, s, ws.ID, col.ID, "gap subject", "")
			plantLegacyRows(t, s, func(raw *sql.DB) {
				if _, err := raw.Exec(
					`UPDATE items SET fields = ? WHERE id = ?`, gap.Value, item.ID); err != nil {
					t.Skipf("the gap value is not storable in this column: %v", err)
				}
			})

			report, err := s.ScanNUL()
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			for _, v := range report.Violations {
				if v.Table == "items" && v.Column == "fields" && v.Key["id"] == item.ID {
					t.Fatalf("the scan now DETECTS a recorded known gap (%s). That is good news and this "+
						"test is the notification: BUG-2812 has landed, so move this case into the "+
						"covered set and drop the residual note from ScanNUL's doc comment and "+
						"docs/backup.md.\n  why the gap exists: %s", gap.Name, gap.Why)
				}
			}
		})
	}
}

// TestScanNULReportsSuspectsSeparately covers the class the day-54 ruling
// added: pre-filter matches the predicate does not refuse.
//
// The two members that matter are opposites, and the whole point is that NO
// CHECK HERE can tell them apart — a doubled-backslash literal and a NUL hidden
// behind a repeated key both decode to no NUL. Both are listed; the destination
// decides. This test pins that both LAND in the suspect list and neither lands
// in the violations, which is what makes the preflight's oracle reachable.
func TestScanNULReportsSuspectsSeparately(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	ws := createTestWorkspace(t, s, "SuspectWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	literal := createTestItem(t, s, ws.ID, col.ID, "literal", "body")
	shadowed := createTestItem(t, s, ws.ID, col.ID, "shadowed", "body")
	clean := createTestItem(t, s, ws.ID, col.ID, "clean", "body")

	esc := textguard.EscNUL
	backslash := esc[:1]

	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`,
			`{"note":"x`+backslash+esc+`y"}`, literal.ID)
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`,
			`{"a":"`+esc+`","a":"clean"}`, shadowed.ID)
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`,
			`{"note":"ordinary"}`, clean.ID)
	})

	report, err := s.ScanNUL()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if report.Total() != 0 {
		t.Errorf("a suspect was reported as a VIOLATION — the predicate has changed, which is what "+
			"DOC-2823 forbids doing in one layer: %v", report.Violations)
	}

	got := map[string]bool{}
	for _, sus := range report.Suspects {
		got[sus.Key["id"]] = true
		if sus.Table != "items" || sus.Column != "fields" {
			t.Errorf("unexpected suspect column %s.%s", sus.Table, sus.Column)
		}
		if sus.WorkspaceID != ws.ID {
			t.Errorf("suspect %v has workspace %q, want %q", sus.Key, sus.WorkspaceID, ws.ID)
		}
	}
	if !got[literal.ID] {
		t.Error("the doubled-backslash literal is not listed as a suspect")
	}
	if !got[shadowed.ID] {
		t.Error("the shadowed-duplicate row is not listed as a suspect — this is the row the preflight " +
			"exists to catch, and dropping it here is the defect the ruling corrects")
	}
	if got[clean.ID] {
		t.Error("a value with no escape at all was listed as a suspect; the pre-filter is over-matching")
	}
}

// TestRepairSuspectFixesOnlyTheFatalShape is the measurement the ruling asked
// for, turned into a guard.
//
// MEASURED FIRST, and the answer decided the design: `textguard.Repair` leaves
// the shadowed-duplicate value completely untouched, because its scanner is
// gated on a map-model question that answers false for exactly this shape. So a
// preflight that refused the row and pointed at `pad db repair-nul` would have
// been pointing at a command that does nothing to it. The repair reaches the
// class through the token-level scanner instead.
//
// The literal leg is the other half and the one that would fail a careless fix:
// a repair broad enough to catch the shadowed value must still leave a value
// that merely writes ABOUT the escape byte-identical.
func TestRepairSuspectFixesOnlyTheFatalShape(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("SQLite only")
	}

	ws := createTestWorkspace(t, s, "SuspectRepairWS")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	literal := createTestItem(t, s, ws.ID, col.ID, "literal", "body")
	shadowed := createTestItem(t, s, ws.ID, col.ID, "shadowed", "body")

	esc := textguard.EscNUL
	backslash := esc[:1]
	literalDoc := `{"note":"x` + backslash + esc + `y"}`
	shadowedDoc := `{"a":"` + esc + `","a":"clean"}`

	// The premise, asserted rather than assumed: the ordinary repair does
	// nothing to the shadowed value. If this ever stops being true, the whole
	// suspect-repair path is redundant and should go.
	if got := textguard.Repair(shadowedDoc, true); got != shadowedDoc {
		t.Fatalf("textguard.Repair now changes the shadowed value (%q). The predicate has gained "+
			"duplicate-key awareness — BUG-2812 has landed. Collapse suspects into violations and "+
			"delete this path.", got)
	}

	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`, literalDoc, literal.ID)
		mustExec(t, raw, `UPDATE items SET fields = ? WHERE id = ?`, shadowedDoc, shadowed.ID)
	})

	report, err := s.RepairNUL()
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	if len(report.SuspectsFailed) != 0 {
		t.Fatalf("suspect failures: %+v", report.SuspectsFailed)
	}
	if len(report.SuspectsRepaired) != 1 {
		t.Fatalf("repaired %d suspects, want 1: %+v", len(report.SuspectsRepaired), report.SuspectsRepaired)
	}
	if report.SuspectsRepaired[0].Key["id"] != shadowed.ID {
		t.Errorf("the wrong suspect was repaired: %v", report.SuspectsRepaired[0].Key)
	}
	if len(report.SuspectsClean) != 1 || report.SuspectsClean[0].Key["id"] != literal.ID {
		t.Errorf("the literal was not reported as needing nothing: %+v", report.SuspectsClean)
	}

	var gotLiteral, gotShadowed string
	if err := s.db.QueryRow(`SELECT fields FROM items WHERE id = ?`, literal.ID).Scan(&gotLiteral); err != nil {
		t.Fatalf("read literal: %v", err)
	}
	if err := s.db.QueryRow(`SELECT fields FROM items WHERE id = ?`, shadowed.ID).Scan(&gotShadowed); err != nil {
		t.Fatalf("read shadowed: %v", err)
	}
	if gotLiteral != literalDoc {
		t.Errorf("a value that merely writes about the escape was rewritten\n  before: %q\n  after:  %q",
			literalDoc, gotLiteral)
	}
	want := `{"a":"` + textguard.ReplacementEscape + `","a":"clean"}`
	if gotShadowed != want {
		t.Errorf("shadowed value = %q, want %q", gotShadowed, want)
	}

	// And the violation counts are untouched: the suspect work must not make
	// the dry run disagree with the run.
	if len(report.Repaired) != 0 || report.Scan.Total() != 0 {
		t.Errorf("suspects leaked into the violation buckets: repaired=%d scanTotal=%d",
			len(report.Repaired), report.Scan.Total())
	}
}

// TestSuspectsCollapseWhenBUG2812Lands is the notification test the ruling
// asked for.
//
// The suspect class exists ONLY because the shared predicate cannot see a NUL
// behind a repeated key. When BUG-2812's token-walk lands, that value becomes
// an ordinary violation, the destination oracle becomes redundant, and this
// whole path — the suspect bucket, the preflight cast, RepairSuspectValue —
// should be deleted rather than left as a second mechanism nobody needs.
//
// Nothing would otherwise tell anyone. This fails at that moment and says what
// to remove.
func TestSuspectsCollapseWhenBUG2812Lands(t *testing.T) {
	shadowed := `{"a":"` + textguard.EscNUL + `","a":"clean"}`

	if textguard.ParameterRefused(shadowed, true) {
		t.Fatalf("the shared predicate now refuses a NUL behind a repeated key, so the SUSPECT class is " +
			"obsolete. BUG-2812 has landed. Remove: NULSuspect and the Suspects bucket in nulscan.go, " +
			"CheckJSONBAcceptable and RepairSuspectValue in nulsuspect.go, the destination cast in " +
			"cmd/pad/cmd_db.go's preflight, the suspect heading in cmd_db_nul.go, and the residual " +
			"paragraphs in ScanNUL's doc comment and docs/backup.md.")
	}
}
