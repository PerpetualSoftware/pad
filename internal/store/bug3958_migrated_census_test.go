package store

import (
	"sort"
	"testing"
)

// BUG-3958: TestMigratedRefusalTablesAllExist checks listed => exists, never
// exists => classified, which is how workspace_read_cursors and app_installs
// went unrefused. This is the other direction: every table in the live schema
// (sqlite_master on a fresh store, the instrument migratedRefusalTables names)
// is either refused, or excluded with a group, or an FTS shadow table. A new
// table fails here until someone decides which.
func TestMigratedEveryLiveTableIsClassified(t *testing.T) {
	s := testStoreSQLite(t)
	rows, err := s.db.Query(`SELECT name FROM sqlite_master WHERE type='table'`)
	if err != nil {
		t.Fatal(err)
	}
	var live []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		live = append(live, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	sort.Strings(live)

	refused := map[string]bool{}
	for _, n := range MigratedRefusalTables() {
		refused[n] = true
	}
	var unclassified []string
	for _, n := range live {
		if !refused[n] && migratedExcludedTables[n] == "" && !isFTSShadowTable(n) {
			unclassified = append(unclassified, n)
		}
	}
	if len(unclassified) > 0 {
		t.Errorf("%d live table(s) are neither in migratedRefusalTables nor in migratedExcludedTables: %v. "+
			"Decide whether a write to the abandoned SQLite file after migrate-to-pg would lose something "+
			"(refuse it) or not (exclude it, with its group)", len(unclassified), unclassified)
	}
	// Premise: the instrument sees the schema (a broken query would pass above).
	if len(live) < 60 {
		t.Fatalf("premise: only %d live tables read from sqlite_master", len(live))
	}
}

// The two lists must not overlap: a table both refused and excluded means
// someone classified it twice and one of the reasons is wrong.
func TestMigratedRefusedAndExcludedAreDisjoint(t *testing.T) {
	for _, n := range MigratedRefusalTables() {
		if g := migratedExcludedTables[n]; g != "" {
			t.Errorf("%q is in migratedRefusalTables and excluded as %q", n, g)
		}
		if isFTSShadowTable(n) {
			t.Errorf("%q is in migratedRefusalTables and matches the FTS shadow pattern", n)
		}
	}
}

// An exclusion naming a table that no longer exists is stale prose in code.
// The marker table is created by the migration, not by store.New, so it is the
// one exclusion absent from a fresh store.
func TestMigratedExcludedTablesAllExist(t *testing.T) {
	s := testStoreSQLite(t)
	for n := range migratedExcludedTables {
		if n == migratedMarkerTable {
			continue
		}
		var c int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, n).Scan(&c); err != nil {
			t.Fatalf("look up %s: %v", n, err)
		}
		if c != 1 {
			t.Errorf("migratedExcludedTables names %q, which is not a table in the live schema", n)
		}
	}
}

func TestIsFTSShadowTable(t *testing.T) {
	for n, want := range map[string]bool{
		"items_fts": true, "items_fts_data": true, "comments_fts_idx": true, "documents_fts_docsize": true,
		"items_fts_config": true, "items": false, "fts_items": false, "items_ftsx": false, "_fts": false,
		// BUG-3958 r1: a non-FTS table that merely ends like a shadow table.
		"user_fts_data": false, "items_fts_content": false,
	} {
		if got := isFTSShadowTable(n); got != want {
			t.Errorf("isFTSShadowTable(%q) = %v, want %v", n, got, want)
		}
	}
}
