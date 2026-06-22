package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteFileAtomic_WritesContent is the smoke test for the export
// command's atomic writer: the bytes land at the destination intact and
// the file carries the requested mode. The atomic temp-then-rename
// guarantee itself (no truncation on partial failure) is exercised by
// the os.Rename semantics; here we just confirm the happy path writes
// the file correctly so `pad item export` behaves like os.WriteFile did.
func TestWriteFileAtomic_WritesContent(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "ship.pad.md")
	body := []byte("---\nkind: playbook\n---\n# Ship\n")

	if err := writeFileAtomic(dst, body, 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("content = %q, want %q", got, body)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %v, want 0644", perm)
	}

	// No leftover temp files in the directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only the destination file", names)
	}
}

// TestWriteFileAtomic_OverwritesExisting confirms the writer replaces an
// existing destination (deliberate overwrite semantics, matching the
// attachment download path).
func TestWriteFileAtomic_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.pad.md")
	if err := os.WriteFile(dst, []byte("old contents that are longer"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	body := []byte("new")
	if err := writeFileAtomic(dst, body, 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}
