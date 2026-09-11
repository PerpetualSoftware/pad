package main

import (
	"os"
	"path/filepath"
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
	if err := os.WriteFile(backup, []byte("backup contents"), 0o600); err != nil {
		t.Fatal(err)
	}

	setSQLiteRestoreEnv(t, dataDir)
	cmd := dbRestoreCmd()
	cmd.SetArgs([]string{"--force", backup})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(dataDir, "pad.db")); err != nil || string(got) != "backup contents" {
		t.Fatalf("restored database = %q, %v", got, err)
	}
}

func setSQLiteRestoreEnv(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("PAD_DB_DRIVER", "")
	t.Setenv("PAD_DATABASE_URL", "")
	t.Setenv("PAD_DB_PATH", "")
	t.Setenv("PAD_DATA_DIR", dataDir)
	t.Setenv("PAD_PORT", "0")
}
