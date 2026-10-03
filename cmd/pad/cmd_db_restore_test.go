package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDBRestoreRejectsSameFile(t *testing.T) {
	const contents = "database contents"

	aliases := []struct {
		name string
		path func(t *testing.T, live string) string
	}{
		{"same path", func(_ *testing.T, live string) string { return live }},
		{"symlink", func(t *testing.T, live string) string {
			link := filepath.Join(filepath.Dir(live), "backup.db")
			if err := os.Symlink(live, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return link
		}},
		{"hard link", func(t *testing.T, live string) string {
			link := filepath.Join(filepath.Dir(live), "backup.db")
			if err := os.Link(live, link); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			return link
		}},
	}

	for _, tc := range aliases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			live := filepath.Join(dataDir, "pad.db")
			if err := os.WriteFile(live, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}

			setSQLiteRestoreEnv(t, dataDir)
			cmd := dbRestoreCmd()
			cmd.SetArgs([]string{"--force", tc.path(t, live)})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "is the SQLite database being restored") {
				t.Fatalf("restore error = %v, want same-file error", err)
			}
			if got, err := os.ReadFile(live); err != nil || string(got) != contents {
				t.Fatalf("database after rejected restore = %q, %v", got, err)
			}
		})
	}
}

func TestDBRestoreCopiesDistinctFile(t *testing.T) {
	dataDir := t.TempDir()
	backup := filepath.Join(dataDir, "backup.db")
	writeRestoreSQLite(t, backup, "backup contents", 4096, false)

	setSQLiteRestoreEnv(t, dataDir)
	cmd := dbRestoreCmd()
	cmd.SetArgs([]string{"--force", backup})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	assertRestoreSQLiteValue(t, filepath.Join(dataDir, "pad.db"), "backup contents")
}

// Copy a quiescent committed WAL tuple while its scratch connection is open,
// then close the scratch DB. The fixture itself has no live connection.
func writeRestoreSQLite(t *testing.T, path, value string, pageSize int, wal bool) {
	t.Helper()
	scratch := filepath.Join(t.TempDir(), "scratch.db")
	db, err := sql.Open("sqlite", scratch)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	statements := []string{fmt.Sprintf("PRAGMA page_size=%d", pageSize)}
	if wal {
		statements = append(statements, "PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0")
	}
	statements = append(statements, "CREATE TABLE recovery_probe(value TEXT)")
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("INSERT INTO recovery_probe VALUES(?)", "checkpointed "+value); err != nil {
		t.Fatal(err)
	}
	if wal {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("UPDATE recovery_probe SET value=?", value); err != nil {
		t.Fatal(err)
	}
	for suffix, data := range restoreSQLiteTuple(t, scratch) {
		if err := os.WriteFile(path+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func restoreSQLiteTuple(t *testing.T, path string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result[suffix] = data
	}
	return result
}

func assertRestoreSQLiteTuple(t *testing.T, path string, want map[string][]byte) {
	t.Helper()
	got := restoreSQLiteTuple(t, path)
	for _, suffix := range []string{"", "-wal", "-shm"} {
		wantData, wantPresent := want[suffix]
		gotData, gotPresent := got[suffix]
		if wantPresent != gotPresent || !bytes.Equal(wantData, gotData) {
			t.Errorf("database%s changed: before=%d bytes, after=%d bytes; presence %v -> %v", suffix, len(wantData), len(gotData), wantPresent, gotPresent)
		}
	}
}

func openRestoreSQLiteCopy(t *testing.T, path string) *sql.DB {
	t.Helper()
	// Inspect a copy so opening/closing SQLite cannot checkpoint the fixture.
	copyPath := filepath.Join(t.TempDir(), "inspection.db")
	for suffix, data := range restoreSQLiteTuple(t, path) {
		if err := os.WriteFile(copyPath+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite", copyPath)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func assertRestoreSQLiteValue(t *testing.T, path, want string) {
	t.Helper()
	db := openRestoreSQLiteCopy(t, path)
	defer db.Close()
	var got string
	if err := db.QueryRow("SELECT value FROM recovery_probe").Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("committed value=%q, want %q", got, want)
	}
}

func executeSQLiteRestore(t *testing.T, backup string) error {
	t.Helper()
	cmd := dbRestoreCmd()
	cmd.SetArgs([]string{"--force", backup})
	return cmd.Execute()
}

func TestDBRestoreInputFailurePreservesCommittedWAL(t *testing.T) {
	for _, tc := range []struct{ name, suffix, failure string }{
		{"main directory", "", "directory"},
		{"invalid main", "", "corrupt"},
		{"empty main", "", "empty"},
		{"WAL directory", "-wal", "directory"},
		{"SHM directory", "-shm", "directory"},
		{"unreadable WAL", "-wal", "unreadable"},
		{"unreadable SHM", "-shm", "unreadable"},
		{"WAL stat error", "-wal", "symlink loop"},
		{"SHM stat error", "-shm", "symlink loop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.failure == "unreadable" && (runtime.GOOS == "windows" || os.Geteuid() == 0) {
				t.Skip("Unix permission denial requires a non-root process")
			}
			dataDir := t.TempDir()
			live := filepath.Join(dataDir, "pad.db")
			backup := filepath.Join(t.TempDir(), "backup.db")
			writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
			writeRestoreSQLite(t, backup, "restored committed WAL row", 4096, true)
			before := restoreSQLiteTuple(t, live)
			source := restoreSQLiteTuple(t, backup)
			assertRestoreSQLiteValue(t, live, "original committed WAL row")
			input := backup + tc.suffix
			switch tc.failure {
			case "directory":
				delete(source, tc.suffix)
				if err := os.Remove(input); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(input, 0700); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				source[""] = []byte("not a SQLite database")
				if err := os.WriteFile(input, []byte("not a SQLite database"), 0600); err != nil {
					t.Fatal(err)
				}
			case "empty":
				source[""] = nil
				if err := os.WriteFile(input, nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				if err := os.Chmod(input, 0); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(input, 0600)
			case "symlink loop":
				delete(source, tc.suffix)
				if err := os.Remove(input); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(input), input); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			setSQLiteRestoreEnv(t, dataDir)
			if err := executeSQLiteRestore(t, backup); err == nil {
				t.Error("invalid backup restored successfully")
			}
			if tc.failure == "unreadable" {
				if err := os.Chmod(input, 0600); err != nil {
					t.Fatal(err)
				}
			}
			for suffix, want := range source {
				got, err := os.ReadFile(backup + suffix)
				if err != nil || !bytes.Equal(got, want) {
					t.Errorf("source backup%s changed: %v", suffix, err)
				}
			}
			assertRestoreSQLiteTuple(t, live, before)
			assertRestoreSQLiteValue(t, live, "original committed WAL row")
		})
	}
}

func TestDBRestoreStandaloneAndLegacyWAL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wal      bool
		pageSize int
	}{
		{"standalone", false, 4096}, {"legacy WAL", true, 4096},
		{"different standalone page size", false, 8192}, {"different WAL page size", true, 8192},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			live := filepath.Join(dataDir, "pad.db")
			backup := filepath.Join(t.TempDir(), "backup.db")
			writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
			writeRestoreSQLite(t, backup, "restored row", tc.pageSize, tc.wal)
			source := restoreSQLiteTuple(t, backup)
			setSQLiteRestoreEnv(t, dataDir)
			if err := executeSQLiteRestore(t, backup); err != nil {
				t.Fatal(err)
			}
			assertRestoreSQLiteValue(t, live, "restored row")
			assertRestoreSQLiteTuple(t, backup, source)
		})
	}
}

func TestDBRestoreNonWritableTargetPreservesCommittedWAL(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("Unix directory permission denial requires a non-root process")
	}
	dataDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dataDir, "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dataDir, "pad.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	before := restoreSQLiteTuple(t, live)
	source := restoreSQLiteTuple(t, backup)
	if err := os.Chmod(dataDir, 0555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dataDir, 0755)
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err == nil {
		t.Error("non-writable target directory accepted")
	}
	assertRestoreSQLiteTuple(t, live, before)
	assertRestoreSQLiteValue(t, live, "original committed WAL row")
	assertRestoreSQLiteTuple(t, backup, source)
}

func TestDBRestoreTargetPreparationFailurePreservesCommittedWAL(t *testing.T) {
	dataDir := t.TempDir()
	live := filepath.Join(dataDir, "pad.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	// A stale SHM path obstructs SQLite's target preparation. Keep the
	// committed WAL intact; SHM is a rebuildable index, not the data.
	if err := os.Remove(live + "-shm"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(live+"-shm", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live+"-shm", "obstruction"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	source := restoreSQLiteTuple(t, backup)
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err == nil {
		t.Error("obstructed target restored successfully")
	}
	// Remove the obstructing cache path to inspect recoverable committed data.
	if err := os.RemoveAll(live + "-shm"); err != nil {
		t.Fatal(err)
	}
	assertRestoreSQLiteValue(t, live, "original committed WAL row")
	assertRestoreSQLiteTuple(t, backup, source)
}

func TestDBRestoreCorruptLiveDatabase(t *testing.T) {
	for _, hasWAL := range []bool{false, true} {
		t.Run(fmt.Sprintf("WAL=%v", hasWAL), func(t *testing.T) {
			dataDir := t.TempDir()
			live := filepath.Join(dataDir, "pad.db")
			backup := filepath.Join(t.TempDir(), "backup.db")
			writeRestoreSQLite(t, live, "original row", 4096, hasWAL)
			if err := os.WriteFile(live, []byte("corrupt live database"), 0600); err != nil {
				t.Fatal(err)
			}
			writeRestoreSQLite(t, backup, "restored row", 4096, false)
			before, source := restoreSQLiteTuple(t, live), restoreSQLiteTuple(t, backup)
			setSQLiteRestoreEnv(t, dataDir)
			err := executeSQLiteRestore(t, backup)
			if hasWAL {
				if err == nil {
					t.Error("corrupt target with potentially committed WAL accepted")
				}
				assertRestoreSQLiteTuple(t, live, before)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertRestoreSQLiteValue(t, live, "restored row")
			}
			assertRestoreSQLiteTuple(t, backup, source)
		})
	}
}

func TestDBRestorePreservesDestinationSymlinkAndMode(t *testing.T) {
	dataDir := t.TempDir()
	live := filepath.Join(dataDir, "pad.db")
	referent := filepath.Join(t.TempDir(), "referent.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, referent, "original row", 4096, true)
	if err := os.Chmod(referent, 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(referent, live); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(live)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination symlink replaced: %v, %v", info, err)
	}
	info, err = os.Stat(referent)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Errorf("restored mode=%o, want 640", info.Mode().Perm())
	}
	assertRestoreSQLiteValue(t, referent, "restored row")
}

func TestDBRestoreAtomicReplacementLeavesOtherHardLinkUnchanged(t *testing.T) {
	dataDir := t.TempDir()
	live := filepath.Join(dataDir, "pad.db")
	other := filepath.Join(dataDir, "other.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, live, "original row", 4096, false)
	if err := os.Link(live, other); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err != nil {
		t.Fatal(err)
	}
	assertRestoreSQLiteValue(t, live, "restored row")
	assertRestoreSQLiteValue(t, other, "original row")
}

func TestDBRestoreReservedCharactersInTargetDirectory(t *testing.T) {
	characters := "?#%"
	if runtime.GOOS == "windows" {
		characters = "#%"
	}
	dataDir := filepath.Join(t.TempDir(), "data "+characters)
	if err := os.Mkdir(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dataDir, "pad.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err != nil {
		t.Fatal(err)
	}
	assertRestoreSQLiteValue(t, live, "restored row")
}

func TestDBRestoreHeldWriteLockPreservesCommittedWAL(t *testing.T) {
	dataDir := t.TempDir()
	live := filepath.Join(dataDir, "pad.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	source := restoreSQLiteTuple(t, backup)
	db, err := sql.Open("sqlite", live)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if _, err := conn.ExecContext(context.Background(), "UPDATE recovery_probe SET value='uncommitted row'"); err != nil {
		t.Fatal(err)
	}
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err == nil {
		t.Error("restore accepted a target with a held write lock")
	}
	if _, err := conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	assertRestoreSQLiteValue(t, live, "original committed WAL row")
	assertRestoreSQLiteTuple(t, backup, source)
}

func TestDBRestorePreservesLegacyWALRowIDsAndExternalFTS(t *testing.T) {
	for _, idDefinition := range []string{"id TEXT PRIMARY KEY", "id TEXT"} {
		t.Run(idDefinition, func(t *testing.T) {
			dataDir := t.TempDir()
			live := filepath.Join(dataDir, "pad.db")
			backup := filepath.Join(t.TempDir(), "backup.db")
			writeRestoreSQLite(t, live, "original committed WAL row", 4096, true)
			scratch := filepath.Join(t.TempDir(), "fts.db")
			db, err := sql.Open("sqlite", scratch)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			for _, statement := range []string{
				"PRAGMA journal_mode=WAL",
				"PRAGMA wal_autocheckpoint=0",
				fmt.Sprintf("CREATE TABLE recovery_probe(%s, value TEXT NOT NULL)", idDefinition),
				"CREATE VIRTUAL TABLE recovery_fts USING fts5(value, content='recovery_probe', content_rowid='rowid')",
				"CREATE TRIGGER recovery_ai AFTER INSERT ON recovery_probe BEGIN INSERT INTO recovery_fts(rowid,value) VALUES(new.rowid,new.value); END",
				"CREATE TRIGGER recovery_ad AFTER DELETE ON recovery_probe BEGIN INSERT INTO recovery_fts(recovery_fts,rowid,value) VALUES('delete',old.rowid,old.value); END",
				"CREATE TRIGGER recovery_au AFTER UPDATE ON recovery_probe BEGIN INSERT INTO recovery_fts(recovery_fts,rowid,value) VALUES('delete',old.rowid,old.value); INSERT INTO recovery_fts(rowid,value) VALUES(new.rowid,new.value); END",
				"INSERT INTO recovery_probe VALUES('removed','discarded'),('retained','searchable retained row')",
				"DELETE FROM recovery_probe WHERE id='removed'",
			} {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			for suffix, data := range restoreSQLiteTuple(t, scratch) {
				if err := os.WriteFile(backup+suffix, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			source := restoreSQLiteTuple(t, backup)
			setSQLiteRestoreEnv(t, dataDir)
			if err := executeSQLiteRestore(t, backup); err != nil {
				t.Fatal(err)
			}
			restored := openRestoreSQLiteCopy(t, live)
			defer restored.Close()
			var rowID int
			if err := restored.QueryRow("SELECT rowid FROM recovery_probe WHERE id='retained'").Scan(&rowID); err != nil {
				t.Fatal(err)
			}
			if rowID != 2 {
				t.Errorf("restored hidden rowid=%d, want 2", rowID)
			}
			var match string
			if err := restored.QueryRow("SELECT p.value FROM recovery_probe p JOIN recovery_fts f ON f.rowid=p.rowid WHERE recovery_fts MATCH 'searchable'").Scan(&match); err != nil || match != "searchable retained row" {
				t.Errorf("external-content FTS lookup=%q, err=%v", match, err)
			}
			if _, err := restored.Exec("INSERT INTO recovery_fts(recovery_fts,rank) VALUES('integrity-check',1)"); err != nil {
				t.Errorf("external-content FTS integrity: %v", err)
			}
			assertRestoreSQLiteTuple(t, backup, source)
		})
	}
}

func TestDBRestorePreservesDanglingDestinationSymlink(t *testing.T) {
	for _, chain := range []bool{false, true} {
		t.Run(fmt.Sprintf("chain=%v", chain), func(t *testing.T) {
			dataDir := t.TempDir()
			live := filepath.Join(dataDir, "pad.db")
			referent := filepath.Join(t.TempDir(), "missing.db")
			backup := filepath.Join(t.TempDir(), "backup.db")
			target, err := filepath.Rel(dataDir, referent)
			if err != nil {
				t.Fatal(err)
			}
			if chain {
				link := filepath.Join(dataDir, "intermediate.db")
				if err := os.Symlink(target, link); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				target = filepath.Base(link)
			}
			if err := os.Symlink(target, live); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			writeRestoreSQLite(t, backup, "restored row", 4096, false)
			setSQLiteRestoreEnv(t, dataDir)
			if err := executeSQLiteRestore(t, backup); err != nil {
				t.Fatal(err)
			}
			got, err := os.Readlink(live)
			if err != nil || got != target {
				t.Fatalf("dangling destination link replaced: target=%q err=%v", got, err)
			}
			assertRestoreSQLiteValue(t, referent, "restored row")
		})
	}
}

func TestDBRestorePublicationFailurePreservesDatabase(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS user immutable file flags")
	}
	dataDir := t.TempDir()
	live := filepath.Join(dataDir, "pad.db")
	backup := filepath.Join(t.TempDir(), "backup.db")
	writeRestoreSQLite(t, live, "original row", 4096, false)
	writeRestoreSQLite(t, backup, "restored row", 4096, false)
	before, source := restoreSQLiteTuple(t, live), restoreSQLiteTuple(t, backup)
	if out, err := exec.Command("chflags", "uchg", live).CombinedOutput(); err != nil {
		t.Skipf("immutable flags unavailable: %v: %s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("chflags", "nouchg", live).CombinedOutput(); err != nil {
			t.Errorf("clear immutable flag: %v: %s", err, out)
		}
	})
	setSQLiteRestoreEnv(t, dataDir)
	if err := executeSQLiteRestore(t, backup); err == nil {
		t.Error("restore succeeded despite immutable destination")
	}
	assertRestoreSQLiteTuple(t, live, before)
	assertRestoreSQLiteValue(t, live, "original row")
	assertRestoreSQLiteTuple(t, backup, source)
}

func setSQLiteRestoreEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("PAD_DB_DRIVER", "")
	t.Setenv("PAD_DATABASE_URL", "")
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", dataDir)
	t.Setenv("PAD_PORT", "0")
}
