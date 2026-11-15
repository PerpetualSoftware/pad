package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPostgresBackupAtomicOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake pg_dump uses a POSIX shell")
	}
	bin := t.TempDir()
	script := `#!/bin/sh
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--file" ]; then shift; file=$1; fi
  shift
done
[ -n "$file" ] || exit 90
if [ "$file" = - ]; then
  printf '%s' "$FAKE_DUMP_CONTENT" || exit 91
else
  printf '%s' "$FAKE_DUMP_CONTENT" > "$file" || exit 91
fi
if [ -n "$FAKE_BLOCK_TARGET" ]; then mkdir "$FAKE_BLOCK_TARGET" || exit 92; fi
exit "$FAKE_DUMP_STATUS"
`
	if err := os.WriteFile(filepath.Join(bin, "pg_dump"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PAD_DB_DRIVER", "postgres")
	t.Setenv("PAD_DATABASE_URL", "postgres://user@localhost/test")
	t.Setenv("PGSERVICE", "")
	t.Setenv("FAKE_BLOCK_TARGET", "")

	for _, tc := range []struct {
		name, initial, dump, status string
		exists, fail                bool
	}{
		{name: "failure preserves existing", initial: "previous complete backup\n", dump: "partial", status: "23", exists: true, fail: true},
		{name: "failure leaves absent", dump: "partial", status: "23", fail: true},
		{name: "success replaces existing", initial: "previous complete backup\n", dump: "complete SQL\n", status: "0", exists: true},
		{name: "success creates absent", dump: "complete SQL\n", status: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			output := filepath.Join(dir, "backup with spaces.sql")
			if tc.exists {
				if err := os.WriteFile(output, []byte(tc.initial), 0640); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(output, 0640); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("FAKE_DUMP_CONTENT", tc.dump)
			t.Setenv("FAKE_DUMP_STATUS", tc.status)
			cmd := dbBackupCmd()
			cmd.SetArgs([]string{"--output", output})
			err := cmd.Execute()
			if tc.fail {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 || !strings.Contains(err.Error(), "pg_dump failed") {
					t.Errorf("expected wrapped pg_dump exit 23, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("backup failed: %v", err)
			}
			got, readErr := os.ReadFile(output)
			if tc.fail && !tc.exists {
				if !os.IsNotExist(readErr) {
					t.Errorf("output should be absent: contents %q, error %v", got, readErr)
				}
			} else {
				want := tc.dump
				if tc.fail {
					want = tc.initial
				}
				if readErr != nil || string(got) != want {
					t.Errorf("output = %q, error %v; want %q", got, readErr, want)
				}
			}
			if tc.exists {
				info, statErr := os.Stat(output)
				if statErr != nil || info.Mode().Perm() != 0640 {
					t.Errorf("output permissions: info %v, error %v; want 0640", info, statErr)
				}
			}
			entries, readDirErr := os.ReadDir(dir)
			if readDirErr != nil {
				t.Fatal(readDirErr)
			}
			wantEntries := 1
			if tc.fail && !tc.exists {
				wantEntries = 0
			}
			if len(entries) != wantEntries {
				t.Errorf("unexpected files after backup: %v", entries)
			}
		})
	}

	t.Setenv("FAKE_DUMP_CONTENT", "complete SQL\n")
	t.Setenv("FAKE_DUMP_STATUS", "0")

	t.Run("symlink remains and target is replaced", func(t *testing.T) {
		dir := t.TempDir()
		target, link := filepath.Join(dir, "target.sql"), filepath.Join(dir, "backup.sql")
		if err := os.WriteFile(target, []byte("old"), 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0640); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("target.sql", link); err != nil {
			t.Fatal(err)
		}
		cmd := dbBackupCmd()
		cmd.SetArgs([]string{"--output", link})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if got, err := os.Readlink(link); err != nil || got != "target.sql" {
			t.Errorf("symlink = %q, error %v", got, err)
		}
		if got, err := os.ReadFile(target); err != nil || string(got) != "complete SQL\n" {
			t.Errorf("target = %q, error %v", got, err)
		}
		if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0640 {
			t.Errorf("target permissions: info %v, error %v", info, err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
			t.Errorf("directory entries = %v, error %v", entries, err)
		}
	})

	t.Run("publish failure cleans staging file", func(t *testing.T) {
		dir := t.TempDir()
		output := filepath.Join(dir, "backup.sql")
		t.Setenv("FAKE_BLOCK_TARGET", output)
		cmd := dbBackupCmd()
		cmd.SetArgs([]string{"--output", output})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "publish PostgreSQL backup") {
			t.Errorf("expected publication failure, got %v", err)
		}
		if info, err := os.Stat(output); err != nil || !info.IsDir() {
			t.Errorf("blocking directory: info %v, error %v", info, err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 1 {
			t.Errorf("directory entries = %v, error %v", entries, err)
		}
	})

	t.Run("dash streams to stdout", func(t *testing.T) {
		dir := t.TempDir()
		t.Chdir(dir)
		cmd := dbBackupCmd()
		cmd.SetArgs([]string{"--output", "-"})
		var err error
		got := captureStdout(t, func() {
			err = cmd.Execute()
		})
		if err != nil || got != "complete SQL\n" {
			t.Fatalf("stdout = %q, error %v", got, err)
		}
		if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
			t.Errorf("directory entries = %v, error %v", entries, err)
		}
	})
}
