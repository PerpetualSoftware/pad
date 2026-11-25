package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runWorkspaceExport(t *testing.T, handler http.Handler, args ...string) (string, error) {
	t.Helper()
	setupPushTest(t, handler)
	cmd := exportCmd()
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	var err error
	stdout := captureStdout(t, func() { err = cmd.Execute() })
	return stdout, err
}

func assertNoLeftoverTempFiles(t *testing.T, dir, expectedName string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	if len(entries) != 1 || entries[0].Name() != expectedName {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir entries = %v, want only [%s]", names, expectedName)
	}
}

func TestBUG4833_FailedStreamKeepsExistingFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "backup.tar.gz")
	wantOld := []byte("known-good-previous-backup-bytes")
	if err := os.WriteFile(dst, wantOld, 0o600); err != nil {
		t.Fatalf("seed existing backup: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/demo/export" || r.URL.Query().Get("format") != "tar" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"export failed"}}`))
	})

	_, err := runWorkspaceExport(t, handler, "-o", dst)
	if err == nil {
		t.Fatal("expected error on 500 export response, got nil")
	}

	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("existing destination disappeared or unreadable: %v", readErr)
	}
	if !bytes.Equal(got, wantOld) {
		t.Errorf("destination bytes = %q, want %q", got, wantOld)
	}
	assertNoLeftoverTempFiles(t, dir, "backup.tar.gz")
}

func TestBUG4833_BadTrailerKeepsExistingFile(t *testing.T) {
	cases := []struct {
		name           string
		declareTrailer bool
		trailerValue   string
	}{
		{
			name:           "error_trailer",
			declareTrailer: true,
			trailerValue:   "error",
		},
		{
			name:           "missing_trailer",
			declareTrailer: false,
			trailerValue:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dst := filepath.Join(dir, "backup.tar.gz")
			wantOld := []byte("known-good-previous-backup-bytes")
			if err := os.WriteFile(dst, wantOld, 0o600); err != nil {
				t.Fatalf("seed existing backup: %v", err)
			}

			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/demo/export" || r.URL.Query().Get("format") != "tar" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
				}
				w.Header().Set("Content-Type", "application/gzip")
				if tc.declareTrailer {
					w.Header().Set("Trailer", "X-Bundle-Status")
				}
				_, _ = w.Write([]byte("partial-corrupt-bundle-data"))
				if tc.trailerValue != "" {
					w.Header().Set("X-Bundle-Status", tc.trailerValue)
				}
			})

			_, err := runWorkspaceExport(t, handler, "-o", dst)
			if err == nil {
				t.Fatal("expected error on bad/missing bundle trailer, got nil")
			}
			if !strings.Contains(err.Error(), "incomplete") {
				t.Errorf("error = %q, want it to mention \"incomplete\"", err.Error())
			}

			got, readErr := os.ReadFile(dst)
			if readErr != nil {
				t.Fatalf("existing destination disappeared or unreadable: %v", readErr)
			}
			if !bytes.Equal(got, wantOld) {
				t.Errorf("destination bytes = %q, want %q", got, wantOld)
			}
			assertNoLeftoverTempFiles(t, dir, "backup.tar.gz")
		})
	}
}

func TestBUG4833_SuccessReplacesFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "backup.tar.gz")
	if err := os.WriteFile(dst, []byte("old-backup-bytes"), 0o600); err != nil {
		t.Fatalf("seed existing backup: %v", err)
	}

	wantNew := []byte("new-complete-bundle-bytes")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/demo/export" || r.URL.Query().Get("format") != "tar" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Trailer", "X-Bundle-Status")
		_, _ = w.Write(wantNew)
		w.Header().Set("X-Bundle-Status", "ok")
	})

	_, err := runWorkspaceExport(t, handler, "-o", dst)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, wantNew) {
		t.Errorf("destination bytes = %q, want %q", got, wantNew)
	}
	assertNoLeftoverTempFiles(t, dir, "backup.tar.gz")
}

func TestBUG4833_JSONExportReplacesFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.json")
	if err := os.WriteFile(dst, []byte(`{"old":true}`), 0o600); err != nil {
		t.Fatalf("seed existing json: %v", err)
	}

	wantNew := []byte(`{"workspace":"demo","items":[]}`)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/workspaces/demo/export" || r.URL.Query().Get("format") != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(wantNew)
	})

	_, err := runWorkspaceExport(t, handler, "--json", "-o", dst)
	if err != nil {
		t.Fatalf("json export failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, wantNew) {
		t.Errorf("destination bytes = %q, want %q", got, wantNew)
	}
	assertNoLeftoverTempFiles(t, dir, "out.json")
}
