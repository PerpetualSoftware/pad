package store

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// TestNULColumnCensus is the guard test TASK-2825 specified: it fails when a
// text column joins the schema without being either PROTECTED or EXPLICITLY
// EXCLUDED.
//
// It runs the census query against a real migrated database rather than
// against a parsed copy of the migration files, because the schema a migration
// chain actually produces is the thing enforcement has to match — table
// rebuilds, later ALTERs and dialect quirks all land here and in no source
// file.
//
// If you are here because this failed, the message says which side you are on:
// add the column to nulColumns (with its class) or to nulExcluded (with the
// reason). Do not delete the test.
func TestNULColumnCensus(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("census runs against the SQLite schema; Layer B is SQLite-only (Postgres refuses natively)")
	}

	// The instrument from TASK-2825, verbatim in spirit: every column of every
	// table, so the walk cannot miss one by knowing where to look.
	rows, err := s.db.Query(`
		SELECT m.name, ti.name, ti.type
		FROM sqlite_master m, pragma_table_info(m.name) ti
		WHERE m.type = 'table'
	`)
	if err != nil {
		t.Fatalf("census query: %v", err)
	}
	defer rows.Close()

	protected := map[string]bool{}
	for _, c := range NULProtectedColumns() {
		protected[c.Table+"."+c.Column] = true
	}

	var textCols, unaccounted, listedButAbsent []string
	present := map[string]bool{}
	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// FTS5 shadow tables are derived storage regenerated from base
		// columns, so enforcement at the base covers them.
		if isDerivedTable(table) {
			continue
		}
		key := table + "." + col
		present[key] = true

		// TEXT-affinity columns are the population. SQLite's declared type is
		// what the census used and what a reader can re-run.
		if !isTextAffinity(typ) {
			continue
		}
		textCols = append(textCols, key)
		if !protected[key] {
			if _, excused := nulExcluded[key]; !excused {
				unaccounted = append(unaccounted, key)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// The instrument must have seen the schema, or "nothing unaccounted for"
	// is a statement about the walk rather than about the database.
	if len(textCols) < 100 {
		t.Fatalf("census found only %d text columns; TASK-2825 counted 406 on the v0.15.0 schema, so the "+
			"instrument is broken rather than the schema being small", len(textCols))
	}

	for key := range protected {
		if !present[key] {
			listedButAbsent = append(listedButAbsent, key)
		}
	}

	sort.Strings(unaccounted)
	sort.Strings(listedButAbsent)

	if len(listedButAbsent) > 0 {
		t.Errorf("these columns are in the protected list but NOT in the schema — a typo here becomes a "+
			"trigger that silently protects nothing:\n  %s", strings.Join(listedButAbsent, "\n  "))
	}

	// THE BASELINE, and why it is a baseline rather than a demand that every
	// text column be classified.
	//
	// The schema has ~405 text columns; TASK-2825's census classified the 86
	// that can carry caller text and left the rest — ids, timestamps stored as
	// TEXT, hashes, enums — unenumerated. Requiring an entry for all of them
	// would be 300 lines of noise that nobody reads, and a test nobody reads
	// is a test that gets deleted.
	//
	// What the task actually asked for is an instrument that "fails when a new
	// text/JSON column joins the schema unlisted". A checked-in baseline does
	// exactly that and nothing else: the 301 columns known to be outside the
	// population are recorded, and ANY change to that set — a column added,
	// renamed, or removed — fails here and asks for a decision.
	//
	// Regenerate with GEN_NUL_BASELINE=1 only AFTER deciding each new column's
	// class; the file is evidence of a judgement, not a snapshot to refresh.
	// The regeneration path the comment above promises. It lived only in
	// prose until IDEA-2641 hit the guard and found the flag did nothing —
	// an instruction naming a mechanism that does not exist sends the next
	// reader to hand-edit the file, which is the one form of "regeneration"
	// that can silently drop an entry it did not mean to. It writes the
	// CURRENT unaccounted set, so it records the judgement the developer just
	// made rather than merging into whatever was there before.
	if os.Getenv("GEN_NUL_BASELINE") == "1" {
		if err := os.WriteFile("nul_unprotected_baseline.txt", []byte(strings.Join(unaccounted, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write baseline: %v", err)
		}
		t.Logf("regenerated nul_unprotected_baseline.txt with %d entries — review the diff before committing", len(unaccounted))
		return
	}

	baseline, err := os.ReadFile("nul_unprotected_baseline.txt")
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(string(baseline)), "\n") {
		if line != "" {
			known[line] = true
		}
	}
	if len(known) == 0 {
		t.Fatal("the baseline is empty — the instrument would accept anything")
	}

	var newlyUnaccounted, goneFromBaseline []string
	for _, key := range unaccounted {
		if !known[key] {
			newlyUnaccounted = append(newlyUnaccounted, key)
		}
	}
	for key := range known {
		if !present[key] {
			goneFromBaseline = append(goneFromBaseline, key)
		}
	}
	sort.Strings(goneFromBaseline)

	if len(newlyUnaccounted) > 0 {
		t.Errorf("%d NEW text column(s) joined the schema and are neither protected nor recorded:\n  %s\n\n"+
			"Decide for each: does it carry caller text? If yes, add it to nulColumns with its class (JSON "+
			"if any parser reads the value, text otherwise) and write the migration. If no, regenerate the "+
			"baseline with GEN_NUL_BASELINE=1 — but decide first.",
			len(newlyUnaccounted), strings.Join(newlyUnaccounted, "\n  "))
	}
	// A baseline entry that has become PROTECTED is a stale record, and leaving
	// it there is what lets a protection REGRESSION pass silently: the column
	// would drop out of the protected set, land back in `unaccounted`, find
	// itself still listed in the baseline, and read as expected (codex round
	// 2). The two sets must stay disjoint.
	var protectedButBaselined []string
	for key := range known {
		if protected[key] {
			protectedButBaselined = append(protectedButBaselined, key)
		}
	}
	sort.Strings(protectedButBaselined)
	if len(protectedButBaselined) > 0 {
		t.Errorf("%d column(s) are BOTH protected and recorded as unprotected:\n  %s\n\n"+
			"Remove them from nul_unprotected_baseline.txt. While they are listed there, losing their "+
			"protection would not fail this test.",
			len(protectedButBaselined), strings.Join(protectedButBaselined, "\n  "))
	}

	if len(goneFromBaseline) > 0 {
		t.Errorf("%d baseline column(s) are no longer in the schema:\n  %s\n\n"+
			"A removed or renamed column is worth a look — a RENAME means the old name's exemption now "+
			"covers nothing and the new name is unclassified.",
			len(goneFromBaseline), strings.Join(goneFromBaseline, "\n  "))
	}

	t.Logf("census: %d text columns, %d protected, %d explicitly excluded",
		len(textCols), len(protected), len(nulExcluded))
}

// TestNULColumnListIsWellFormed checks the list itself before anything is
// generated from it.
func TestNULColumnListIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	var jsonN, textN int
	for _, c := range NULProtectedColumns() {
		key := c.Table + "." + c.Column
		if seen[key] {
			t.Errorf("duplicate entry %s — the generator would emit two triggers with the same name", key)
		}
		seen[key] = true
		if c.Table == "" || c.Column == "" {
			t.Errorf("entry with an empty table or column: %#v", c)
		}
		switch c.Class {
		case classJSON:
			jsonN++
		case classText:
			textN++
		default:
			t.Errorf("%s has an unknown class %d", key, c.Class)
		}
	}
	if jsonN == 0 || textN == 0 {
		t.Errorf("classes are %d JSON / %d text; both must be represented or one code path is unexercised",
			jsonN, textN)
	}
	// A column cannot be both protected and excluded — that would mean two
	// people disagreed and neither noticed.
	for key := range nulExcluded {
		if seen[key] {
			t.Errorf("%s is both protected and excluded", key)
		}
	}
	t.Logf("list: %d protected (%d JSON, %d text), %d excluded", len(seen), jsonN, textN, len(nulExcluded))
}

// isTextAffinity reports whether a declared column type can hold text.
//
// WIDER than the first version, which matched TEXT/CHAR/CLOB only (codex round
// 1). SQLite gives BLOB affinity to a column with NO declared type and to one
// declared JSON — both hold text perfectly well — so a future column declared
// either way would have slipped past the census entirely. Matching on what
// CANNOT hold text is the safer direction: a false positive costs one baseline
// line, a false negative costs an unprotected column.
func isTextAffinity(declared string) bool {
	u := strings.ToUpper(strings.TrimSpace(declared))
	switch {
	case u == "":
		return true
	case strings.Contains(u, "INT"),
		strings.Contains(u, "REAL"),
		strings.Contains(u, "FLOA"),
		strings.Contains(u, "DOUB"),
		strings.Contains(u, "NUMERIC"),
		strings.Contains(u, "DECIMAL"),
		strings.Contains(u, "BOOL"),
		strings.Contains(u, "DATE"):
		return false
	}
	return true
}

// isDerivedTable reports whether a table is FTS5 shadow storage rather than
// something written directly.
//
// It matches the shapes SQLite actually creates rather than the substring
// "_fts" anywhere in the name (codex round 1): a real table someone names
// "user_fts_settings" would otherwise be skipped, and skipping a real table is
// exactly the failure this census exists to prevent.
func isDerivedTable(name string) bool {
	if strings.HasPrefix(name, "sqlite_") {
		return true
	}
	if strings.HasSuffix(name, "_fts") {
		return true
	}
	for _, suffix := range []string{"_data", "_idx", "_content", "_docsize", "_config"} {
		if strings.Contains(name, "_fts") && strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// TestAttributionColumnsAreAllProtected keeps the class swept.
//
// Rounds 1, 2, 3, 6 and 7 of review each named unprotected attribution columns.
// Round 6's answer was a sweep — but by a hand-written list of NAMES, which is
// an enumeration one level up, and round 7 found the two names nobody thought
// to write down (event_outbox.claimed_by, oauth_connection_workspaces.added_by).
//
// So the sweep is a PATTERN and it is a test, which is the difference between
// having done it once and it staying done. A new *_by column fails here the day
// it is added.
func TestAttributionColumnsAreAllProtected(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("schema census runs against SQLite")
	}
	rows, err := s.db.Query(`SELECT m.name, ti.name, ti.type FROM sqlite_master m, pragma_table_info(m.name) ti WHERE m.type='table'`)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	defer rows.Close()

	protected := map[string]bool{}
	for _, c := range NULProtectedColumns() {
		protected[c.Table+"."+c.Column] = true
	}

	var unprotected []string
	matched := 0
	for rows.Next() {
		var table, col, typ string
		if err := rows.Scan(&table, &col, &typ); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if isDerivedTable(table) || !isTextAffinity(typ) {
			continue
		}
		if !strings.HasSuffix(col, "_by") && col != "actor" && col != "author" && col != "source" && col != "owner" {
			continue
		}
		matched++
		key := table + "." + col
		if protected[key] {
			continue
		}
		if _, excused := nulExcluded[key]; excused {
			continue
		}
		unprotected = append(unprotected, key)
	}
	if matched == 0 {
		t.Fatal("the pattern matched no columns at all — the instrument is broken, not the schema")
	}
	sort.Strings(unprotected)
	if len(unprotected) > 0 {
		t.Errorf("%d attribution-style column(s) are unprotected:\n  %s\n\n"+
			"These carry a writer identity that a request body can often set. Add them to nulColumns as "+
			"classText, or to nulExcluded with the reason they cannot carry caller text.",
			len(unprotected), strings.Join(unprotected, "\n  "))
	}
}
