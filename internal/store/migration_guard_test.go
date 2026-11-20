package store

// TASK-2900: NO TEST IN THIS FILE MAY CALL t.Parallel().
//
// Every one of them touches a package-level EXPORTED global —
// store.AllowSchemaAhead and store.BinaryVersion — and restores it in a
// t.Cleanup. Under t.Parallel that mutation window is open across every
// concurrently-running test, and the restore fires while they are still in
// flight: AllowSchemaAhead=true would silently disarm the schema-ahead guard
// for whoever else is running, which is the assertion two of these tests exist
// to make.
//
// Missed by the first sweep, and the reason is worth recording: its
// global-mutator grep was `^var [a-z]`, which finds unexported package vars and
// silently skips exported ones. The instrument asked "which unexported globals
// exist" while the claim it was carrying was "which globals exist". Third
// instance today of a grep narrower than the claim resting on it.
//
// TestMigrationGuardTestsStaySerial enforces this file-wide.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestGuardSchemaAhead_Detection covers the pure detection logic: a DB is
// "ahead" iff it has an applied migration that sorts after the highest
// embedded one.
func TestGuardSchemaAhead_Detection(t *testing.T) {
	embedded := []string{"001_initial.sql", "002_foo.sql", "003_bar.sql"}

	cases := []struct {
		name      string
		applied   []string
		wantError bool
	}{
		{"fresh db (nothing applied)", nil, false},
		{"in sync", []string{"001_initial.sql", "002_foo.sql", "003_bar.sql"}, false},
		{"behind (missing latest)", []string{"001_initial.sql", "002_foo.sql"}, false},
		{"ahead by one", []string{"001_initial.sql", "002_foo.sql", "003_bar.sql", "004_future.sql"}, true},
		{"ahead by several", []string{"003_bar.sql", "010_way_ahead.sql", "011_more.sql"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applied := make(map[string]bool, len(tc.applied))
			for _, v := range tc.applied {
				applied[v] = true
			}
			err := guardSchemaAhead(applied, embedded)
			if tc.wantError && err == nil {
				t.Fatalf("expected schema-ahead error, got nil")
			}
			if !tc.wantError && err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if tc.wantError && !strings.Contains(err.Error(), "newer than this pad binary") {
				t.Fatalf("error should explain the downgrade, got: %v", err)
			}
		})
	}
}

// TestGuardSchemaAhead_ForceOverride verifies the escape hatch: with
// AllowSchemaAhead set, an ahead DB is allowed through (warning only).
func TestGuardSchemaAhead_ForceOverride(t *testing.T) {
	prev := AllowSchemaAhead
	t.Cleanup(func() { AllowSchemaAhead = prev })

	embedded := []string{"001_initial.sql", "002_foo.sql"}
	applied := map[string]bool{"001_initial.sql": true, "002_foo.sql": true, "003_future.sql": true}

	if err := guardSchemaAhead(applied, embedded); err == nil {
		t.Fatalf("without override, expected error")
	}

	AllowSchemaAhead = true
	if err := guardSchemaAhead(applied, embedded); err != nil {
		t.Fatalf("with override, expected nil, got %v", err)
	}
}

// TestMigrate_RefusesSchemaAheadDB is the end-to-end guard: a DB carrying a
// migration this binary doesn't ship must fail New(), and must succeed once
// the override is set.
func TestMigrate_RefusesSchemaAheadDB(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ahead.db")

	// Fully migrate a fresh DB, then stamp a from-the-future migration row.
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := s.db.Exec(
		"INSERT INTO schema_migrations (version, applied_at) VALUES ('999_from_the_future.sql', '2099-01-01T00:00:00Z')",
	); err != nil {
		t.Fatalf("stamp future migration: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must now be refused.
	prev := AllowSchemaAhead
	t.Cleanup(func() { AllowSchemaAhead = prev })
	AllowSchemaAhead = false

	if _, err := New(dbPath); err == nil {
		t.Fatalf("expected New() to refuse a schema-ahead DB")
	} else if !strings.Contains(err.Error(), "newer than this pad binary") {
		t.Fatalf("unexpected error: %v", err)
	}

	// With the override, it must open.
	AllowSchemaAhead = true
	s2, err := New(dbPath)
	if err != nil {
		t.Fatalf("New() with override should succeed, got %v", err)
	}
	s2.Close()
}

// TestSnapshotBeforeMigrate_WritesCopy verifies the SQLite snapshot writes a
// byte-faithful copy next to the DB, named for the binary version and the
// migration the run is migrating to.
func TestSnapshotBeforeMigrate_WritesCopy(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "snap.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	prev := BinaryVersion
	t.Cleanup(func() { BinaryVersion = prev })
	BinaryVersion = "9.9.9 (deadbee)" // exercises sanitizeVersion

	s.snapshotBeforeMigrate("042_some_target.sql")

	dst := s.snapshotPath("042_some_target.sql")
	if want := dbPath + ".pre-9.9.9--deadbee--m042"; dst != want {
		t.Fatalf("snapshotPath = %q, want %q", dst, want)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("snapshot not written at %s: %v", dst, err)
	}
	if info.Size() == 0 {
		t.Fatalf("snapshot is empty")
	}

	// The snapshot must be a real SQLite DB — open it and read a known table.
	snap, err := New(dst) // migrates cleanly (already fully migrated copy)
	if err != nil {
		t.Fatalf("snapshot is not a usable DB: %v", err)
	}
	defer snap.Close()
	var n int
	if err := snap.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("query snapshot: %v", err)
	}
	if n == 0 {
		t.Fatalf("snapshot has no applied migrations recorded")
	}
}

// TestSnapshotBeforeMigrate_PreservesExisting verifies a retry of the same
// upgrade (same binary version, same target migration) doesn't clobber the
// original pre-upgrade snapshot with a later (partially-migrated) DB state.
func TestSnapshotBeforeMigrate_PreservesExisting(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "snap.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	prev := BinaryVersion
	t.Cleanup(func() { BinaryVersion = prev })
	BinaryVersion = "1.0.0"
	const target = "120_target.sql"
	dst := s.snapshotPath(target)

	// Pre-seed a sentinel snapshot as if a prior boot already wrote it.
	if err := os.WriteFile(dst, []byte("ORIGINAL-PRE-UPGRADE-STATE"), 0o600); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	s.snapshotBeforeMigrate(target) // must NOT overwrite the existing snapshot

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != "ORIGINAL-PRE-UPGRADE-STATE" {
		t.Fatalf("existing snapshot was overwritten; want sentinel, got %d bytes", len(got))
	}
}

// TestSnapshotBeforeMigrate_SkipsInMemory ensures the snapshot path is a
// no-op for a store with no on-disk file (defensive: guards the dbPath gate).
func TestSnapshotBeforeMigrate_SkipsNoFile(t *testing.T) {
	s := &Store{dialect: &sqliteDialect{}, dbPath: ""}
	s.snapshotBeforeMigrate("001_initial.sql") // must not panic; nothing to do

	// A path that doesn't exist on disk is also a silent no-op.
	s2 := &Store{dialect: &sqliteDialect{}, dbPath: filepath.Join(t.TempDir(), "does-not-exist.db")}
	s2.snapshotBeforeMigrate("001_initial.sql")
	if _, err := os.Stat(s2.snapshotPath("001_initial.sql")); !os.IsNotExist(err) {
		t.Fatalf("expected no snapshot for a missing DB file")
	}
}

// TestSnapshotBeforeMigrate_NewTargetTakesFreshSnapshot is the dev-build case:
// BinaryVersion stays "dev" across builds, so a snapshot keyed on the version
// alone was written once and then preserved forever. A run migrating to a
// newer migration must write a fresh, real snapshot under the new key and
// leave the older key's file exactly as it was.
func TestSnapshotBeforeMigrate_NewTargetTakesFreshSnapshot(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "snap.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	prev := BinaryVersion
	t.Cleanup(func() { BinaryVersion = prev })
	BinaryVersion = "dev"

	const oldTarget, newTarget = "183_old.sql", "184_new.sql"
	oldDst, newDst := s.snapshotPath(oldTarget), s.snapshotPath(newTarget)
	if oldDst == newDst {
		t.Fatalf("control leg failed: old and new targets share the snapshot path %q", oldDst)
	}
	const sentinel = "SNAPSHOT-FROM-AN-EARLIER-DEV-UPGRADE"
	if err := os.WriteFile(oldDst, []byte(sentinel), 0o600); err != nil {
		t.Fatalf("seed old snapshot: %v", err)
	}

	s.snapshotBeforeMigrate(newTarget)

	got, err := os.ReadFile(oldDst)
	if err != nil {
		t.Fatalf("read old snapshot: %v", err)
	}
	if string(got) != sentinel {
		t.Fatalf("old snapshot at %s was modified; want the sentinel, got %d bytes", oldDst, len(got))
	}

	if _, err := os.Stat(newDst); err != nil {
		t.Fatalf("no fresh snapshot at %s: %v", newDst, err)
	}
	snap, err := New(newDst)
	if err != nil {
		t.Fatalf("fresh snapshot is not a usable DB: %v", err)
	}
	defer snap.Close()
	var n int
	if err := snap.db.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatalf("query fresh snapshot: %v", err)
	}
	if n == 0 {
		t.Fatalf("fresh snapshot has no applied migrations recorded")
	}
}

func TestSnapshotTargetKey(t *testing.T) {
	cases := map[string]string{
		"183_foo.sql":     "183",
		"007_a_b_c.sql":   "007",
		"noprefix.sql":    "noprefix",
		"noprefix":        "noprefix",
		"12 3/x_rest.sql": "12-3-x",
		"weird name.sql":  "weird-name",
		"":                "unknown",
	}
	for in, want := range cases {
		if got := snapshotTargetKey(in); got != want {
			t.Errorf("snapshotTargetKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitizeVersion(t *testing.T) {
	cases := map[string]string{
		"dev":             "dev",
		"1.2.3":           "1.2.3",
		"v1.2.3":          "v1.2.3",
		"1.2.3 (abc1234)": "1.2.3--abc1234-",
		"  ":              "unknown",
		"weird/../path":   "weird-..-path",
	}
	for in, want := range cases {
		if got := sanitizeVersion(in); got != want {
			t.Errorf("sanitizeVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// seedSnapshotFile writes content at path and stamps its mtime, so the prune
// order (newest first by mtime) is fixed by the test rather than by how fast
// the files were written.
func seedSnapshotFile(t *testing.T, path, content string, mtime time.Time) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("%s should still exist: %v", filepath.Base(path), err)
	}
}

func assertGone(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s should have been removed (lstat err=%v)", filepath.Base(path), err)
	}
}

// TestPruneOldSnapshots_KeepsCurrentAndNewestOther: of four builder-shaped
// snapshots, the current key and the newest other survive; the older two go,
// and so do the -wal/-shm siblings of a removed one. The siblings of the two
// kept snapshots stay.
func TestPruneOldSnapshots_KeepsCurrentAndNewestOther(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pad.db")
	t0 := time.Now().Add(-48 * time.Hour)

	oldest := dbPath + ".pre-dev-m180"
	older := dbPath + ".pre-dev-m181"
	newestOther := dbPath + ".pre-0.21.0-m182"
	keep := dbPath + ".pre-0.22.0-m186"
	seedSnapshotFile(t, oldest, "s180", t0)
	seedSnapshotFile(t, oldest+"-wal", "w180", t0)
	seedSnapshotFile(t, oldest+"-shm", "h180", t0)
	seedSnapshotFile(t, older, "s181", t0.Add(1*time.Hour))
	seedSnapshotFile(t, newestOther, "s182", t0.Add(2*time.Hour))
	seedSnapshotFile(t, newestOther+"-wal", "w182", t0.Add(2*time.Hour))
	seedSnapshotFile(t, newestOther+"-shm", "h182", t0.Add(2*time.Hour))
	// keep is deliberately OLDER than the others: it is kept by name, not
	// by mtime, and must not count as the "newest other".
	seedSnapshotFile(t, keep, "s186", t0.Add(-1*time.Hour))
	seedSnapshotFile(t, keep+"-wal", "w186", t0.Add(-1*time.Hour))

	pruneOldSnapshots(dbPath, keep)

	for _, p := range []string{keep, keep + "-wal", newestOther, newestOther + "-wal", newestOther + "-shm"} {
		assertPresent(t, p)
	}
	for _, p := range []string{oldest, oldest + "-wal", oldest + "-shm", older} {
		assertGone(t, p)
	}
}

// TestPruneOldSnapshots_TieBrokenByNameDescending: equal mtimes keep the
// candidate whose name sorts last.
func TestPruneOldSnapshots_TieBrokenByNameDescending(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pad.db")
	t0 := time.Now().Add(-time.Hour)
	a, b := dbPath+".pre-dev-m100", dbPath+".pre-dev-m101"
	keep := dbPath + ".pre-dev-m186"
	seedSnapshotFile(t, a, "a", t0)
	seedSnapshotFile(t, b, "b", t0)
	seedSnapshotFile(t, keep, "k", t0)

	pruneOldSnapshots(dbPath, keep)

	assertPresent(t, keep)
	assertPresent(t, b)
	assertGone(t, a)
}

// TestPruneOldSnapshots_LeavesUnrecognisedNamesAlone: only names the snapshot
// builder produces are candidates. Every other file, however snapshot-like,
// is left byte-identical, even when it is older than everything else.
func TestPruneOldSnapshots_LeavesUnrecognisedNamesAlone(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pad.db")
	ancient := time.Now().Add(-365 * 24 * time.Hour)
	t0 := time.Now().Add(-48 * time.Hour)

	unrecognised := []string{
		"pad.db.pre-dev",
		"pad.db.pre-0.20.0",
		"pad.db.pre-commitments-0302e01f",
		"pad.db.pre-my-migration",
		"pad.db.pre-feature-x-m12",
		"pad.db.pre-dev-m183.tmp",
		"pad.db.pre-dev-m183.bak",
		"pad.db.pre-dev-m",
		"pad.db.pre-dev-mabc",
		"other.db.pre-dev-m100",
		"padxdb.pre-dev-m100",
		"pad.db.pre-dev-m100-wal",
	}
	for _, name := range unrecognised {
		seedSnapshotFile(t, filepath.Join(dir, name), "KEEP:"+name, ancient)
	}

	// A symlink with a builder-shaped name, pointing at a builder-shaped
	// file in another directory: neither the link nor its target may go.
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "pad.db.pre-dev-m099")
	seedSnapshotFile(t, target, "SYMLINK-TARGET", ancient)
	link := filepath.Join(dir, "pad.db.pre-dev-m099")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A directory with a builder-shaped name.
	subdir := filepath.Join(dir, "pad.db.pre-dev-m098")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Builder-shaped files, so the prune has real work to do.
	shapedOld := dbPath + ".pre-dev-m184"
	shapedNewer := dbPath + ".pre-1.2.3--abc1234--m185"
	keep := dbPath + ".pre-dev-m186"
	seedSnapshotFile(t, shapedOld, "s184", t0)
	seedSnapshotFile(t, shapedNewer, "s185", t0.Add(time.Hour))
	seedSnapshotFile(t, keep, "s186", t0.Add(2*time.Hour))

	pruneOldSnapshots(dbPath, keep)

	assertGone(t, shapedOld) // control: the prune did run
	assertPresent(t, shapedNewer)
	assertPresent(t, keep)
	for _, name := range unrecognised {
		p := filepath.Join(dir, name)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("unrecognised %s was removed: %v", name, err)
			continue
		}
		if string(got) != "KEEP:"+name {
			t.Errorf("unrecognised %s was modified", name)
		}
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("symlink %s was removed or replaced (err=%v)", link, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "SYMLINK-TARGET" {
		t.Errorf("symlink target was removed or modified (err=%v)", err)
	}
	if fi, err := os.Lstat(subdir); err != nil || !fi.IsDir() {
		t.Errorf("directory %s was removed (err=%v)", subdir, err)
	}
}

// TestPruneOldSnapshots_MissingKeepRemovesNothing: if this run's snapshot is
// not on disk (the copy failed), the older snapshots are the only rollback
// point and none may be removed.
func TestPruneOldSnapshots_MissingKeepRemovesNothing(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "pad.db")
	t0 := time.Now().Add(-48 * time.Hour)
	a, b, c := dbPath+".pre-dev-m180", dbPath+".pre-dev-m181", dbPath+".pre-dev-m182"
	seedSnapshotFile(t, a, "a", t0)
	seedSnapshotFile(t, b, "b", t0.Add(time.Hour))
	seedSnapshotFile(t, c, "c", t0.Add(2*time.Hour))

	pruneOldSnapshots(dbPath, dbPath+".pre-dev-m186") // absent

	for _, p := range []string{a, b, c} {
		assertPresent(t, p)
	}
}

// prepareUpgrade builds a fully migrated SQLite DB at <dir>/pad.db, then
// un-applies one migration so the next New() is an upgrade that takes a
// snapshot. It returns the db path and the snapshot path that upgrade will
// write (BinaryVersion is pinned to "dev" for the test).
func prepareUpgrade(t *testing.T, unapply string) (dbPath, keep string) {
	t.Helper()
	prev := BinaryVersion
	t.Cleanup(func() { BinaryVersion = prev })
	BinaryVersion = "dev"

	dbPath = filepath.Join(t.TempDir(), "pad.db")
	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := s.db.Exec(`DELETE FROM schema_migrations WHERE version = ?`, unapply)
	if err != nil {
		t.Fatalf("un-apply %s: %v", unapply, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("control leg failed: %s was not applied (rows=%d), so nothing would be pending", unapply, n)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	embedded, err := readMigrationNames(migrationsFS, "migrations")
	if err != nil || len(embedded) == 0 {
		t.Fatalf("read embedded migrations: %v (n=%d)", err, len(embedded))
	}
	keep = snapshotPathFor(dbPath, embedded[len(embedded)-1])
	if _, err := os.Stat(keep); err == nil {
		t.Fatalf("control leg failed: %s existed before the upgrade", keep)
	}
	return dbPath, keep
}

// TestPruneSnapshots_AfterSuccessfulMigrate drives a real upgrade through
// New()/migrate(): the run takes a snapshot, succeeds, and leaves exactly two
// builder-shaped snapshots, its own and the newest earlier one.
func TestPruneSnapshots_AfterSuccessfulMigrate(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	// 087 is idempotent (CREATE UNIQUE INDEX IF NOT EXISTS), so re-applying
	// it succeeds.
	dbPath, keep := prepareUpgrade(t, "087_collection_trait_uniqueness.sql")
	t0 := time.Now().Add(-72 * time.Hour)
	old1, old2, old3 := dbPath+".pre-dev-m100", dbPath+".pre-0.20.1-m150", dbPath+".pre-0.21.0-m180"
	seedSnapshotFile(t, old1, "s100", t0)
	seedSnapshotFile(t, old2, "s150", t0.Add(time.Hour))
	seedSnapshotFile(t, old3, "s180", t0.Add(2*time.Hour))

	s, err := New(dbPath)
	if err != nil {
		t.Fatalf("upgrade New: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	assertPresent(t, keep)
	assertPresent(t, old3)
	assertGone(t, old1)
	assertGone(t, old2)
	matches, err := filepath.Glob(dbPath + ".pre-*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("snapshots after upgrade = %v, want exactly 2 (%s and %s)", matches, filepath.Base(keep), filepath.Base(old3))
	}
}

// TestPruneSnapshots_NotAfterFailedMigrate: a migrate run that fails AFTER
// the snapshot is taken removes nothing. 176 opens with a bare CREATE TABLE
// (no IF NOT EXISTS), so re-applying it to a DB that already has the table
// fails. (An ALTER TABLE ADD COLUMN would not do: the runner tolerates a
// duplicate column.)
func TestPruneSnapshots_NotAfterFailedMigrate(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") != "" {
		t.Skip("SQLite-specific path")
	}
	dbPath, keep := prepareUpgrade(t, "176_calendar_feeds.sql")
	t0 := time.Now().Add(-72 * time.Hour)
	old1, old2, old3 := dbPath+".pre-dev-m100", dbPath+".pre-0.20.1-m150", dbPath+".pre-0.21.0-m180"
	seedSnapshotFile(t, old1, "s100", t0)
	seedSnapshotFile(t, old2, "s150", t0.Add(time.Hour))
	seedSnapshotFile(t, old3, "s180", t0.Add(2*time.Hour))

	s, err := New(dbPath)
	if err == nil {
		s.Close()
		t.Fatal("control leg failed: re-applying 176 should make the upgrade fail")
	}
	if !strings.Contains(err.Error(), "176_calendar_feeds.sql") {
		t.Fatalf("upgrade failed, but not in 176: %v", err)
	}

	assertPresent(t, keep) // control: the snapshot WAS taken before the failure
	for _, p := range []string{old1, old2, old3} {
		assertPresent(t, p)
	}
}
