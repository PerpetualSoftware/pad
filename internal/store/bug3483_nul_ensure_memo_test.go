package store

import (
	"database/sql"
	"testing"
)

// BUG-3483: the NUL-trigger ensure caches its RENDERING of the expected
// triggers, never its CHECK. With the cache already warm, a trigger tampered
// with in the file is still found and restored by the next open, exactly as a
// cold process would find it.
func TestNULTriggerEnsureMemoNeverSkipsTheCheck(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverSQLite {
		t.Skip("Layer B is SQLite-only")
	}
	// Warm: this open rendered and checked, and nothing needed restoring.
	if restored, err := s.ensureNULTriggersReporting(); err != nil || restored {
		t.Fatalf("precondition: a fresh store needs no restore (restored=%v err=%v)", restored, err)
	}
	want := renderedNULTriggersFor(nulTriggerMigrations)["pad_nul_items_content_upd"]
	if want == "" {
		t.Fatal("precondition: the rendering names pad_nul_items_content_upd")
	}

	// Tamper with one trigger, keeping its NAME: a defanged body. The set of
	// names is unchanged, so only a definition comparison can catch it.
	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`DROP TRIGGER pad_nul_items_content_upd`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TRIGGER pad_nul_items_content_upd BEFORE UPDATE OF content ON items BEGIN SELECT 1; END`); err != nil {
		t.Fatal(err)
	}

	// The next open, in a process whose memo is warm.
	s2, err := New(s.dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	var stored string
	if err := s2.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='trigger' AND name='pad_nul_items_content_upd'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if normalizeTriggerSQL(stored) != want {
		t.Fatalf("the tampered trigger survived an open with a warm memo:\n got %s\nwant %s", normalizeTriggerSQL(stored), want)
	}
	// And a direct call on the original store, also warm, finds nothing left.
	if restored, err := s.ensureNULTriggersReporting(); err != nil || restored {
		t.Fatalf("after the reopen restored it, nothing should be missing (restored=%v err=%v)", restored, err)
	}
}
