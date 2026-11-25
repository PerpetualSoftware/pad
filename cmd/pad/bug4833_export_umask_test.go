//go:build !windows

package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestBUG4833_ExportIsOwnerOnlyRegardlessOfUmask(t *testing.T) {
	oldUmask := syscall.Umask(0o000)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	wantBody := []byte("owner-only-bundle-bytes")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/demo/export" || r.URL.Query().Get("format") != "tar" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Trailer", "X-Bundle-Status")
		_, _ = w.Write(wantBody)
		w.Header().Set("X-Bundle-Status", "ok")
	})

	dir := t.TempDir()

	// 1. Export to a new path.
	newDst := filepath.Join(dir, "new-backup.tar.gz")
	if _, err := runWorkspaceExport(t, handler, "-o", newDst); err != nil {
		t.Fatalf("export to new path: %v", err)
	}
	info, err := os.Stat(newDst)
	if err != nil {
		t.Fatalf("stat new path: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("new path mode = %04o, want 0600", perm)
	}

	// 2. Export over an existing file created at 0o644.
	existingDst := filepath.Join(dir, "existing-backup.tar.gz")
	if err := os.WriteFile(existingDst, []byte("world-readable-old"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}
	if err := os.Chmod(existingDst, 0o644); err != nil {
		t.Fatalf("chmod existing file: %v", err)
	}

	if _, err := runWorkspaceExport(t, handler, "-o", existingDst); err != nil {
		t.Fatalf("export over existing path: %v", err)
	}
	info, err = os.Stat(existingDst)
	if err != nil {
		t.Fatalf("stat existing path: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("replaced path mode = %04o, want 0600", perm)
	}
	got, err := os.ReadFile(existingDst)
	if err != nil {
		t.Fatalf("read replaced path: %v", err)
	}
	if !bytes.Equal(got, wantBody) {
		t.Errorf("replaced content = %q, want %q", got, wantBody)
	}
}

func TestBUG4833_ItemExportIsOwnerOnly(t *testing.T) {
	oldUmask := syscall.Umask(0o000)
	t.Cleanup(func() { syscall.Umask(oldUmask) })

	wantBody := []byte("---\nkind: playbook\n---\n# Ship\n")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/demo/items/PLAYB-3/export" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="ship.pad.md"`)
		_, _ = w.Write(wantBody)
	})

	dir := t.TempDir()
	dst := filepath.Join(dir, "ship.pad.md")

	setupPushTest(t, handler)
	cmd := itemExportCmd()
	cmd.SetArgs([]string{"PLAYB-3", "-o", dst})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var err error
	_ = captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatalf("item export failed: %v", err)
	}

	info, err := os.Stat(dst)
	if err != nil {
		t.Fatalf("stat artifact: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("item export mode = %04o, want 0600", perm)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	if !bytes.Equal(got, wantBody) {
		t.Errorf("item export content = %q, want %q", got, wantBody)
	}
}
