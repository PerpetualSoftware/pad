package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExportCmdNoVersionsRefusedWithoutCapability(t *testing.T) {
	cases := []struct {
		name       string
		capStatus  int
		capBody    string
		wantRefuse bool
	}{
		{
			name:       "capability false",
			capStatus:  http.StatusOK,
			capBody:    `{"export_include_versions":false}`,
			wantRefuse: true,
		},
		{
			name:       "capability absent on older server",
			capStatus:  http.StatusOK,
			capBody:    `{"collection_resolution":true}`,
			wantRefuse: true,
		},
		{
			name:       "capabilities endpoint 404",
			capStatus:  http.StatusNotFound,
			capBody:    `{"error":{"code":"not_found","message":"not found"}}`,
			wantRefuse: true,
		},
		{
			name:       "capabilities probe indeterminate 500",
			capStatus:  http.StatusInternalServerError,
			capBody:    `{"error":{"code":"internal_error","message":"boom"}}`,
			wantRefuse: true,
		},
		{
			name:       "capability true succeeds",
			capStatus:  http.StatusOK,
			capBody:    `{"export_include_versions":true}`,
			wantRefuse: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exportCalled := false
			var gotQuery string
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/v1/server/capabilities":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.capStatus)
					_, _ = w.Write([]byte(tc.capBody))
				case "/api/v1/workspaces/demo/export":
					exportCalled = true
					gotQuery = r.URL.RawQuery
					w.Header().Set("Content-Type", "application/gzip")
					w.Header().Set("Trailer", "X-Bundle-Status")
					_, _ = w.Write([]byte("bundle-bytes"))
					w.Header().Set("X-Bundle-Status", "ok")
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
					w.WriteHeader(http.StatusNotFound)
				}
			})

			dst := filepath.Join(t.TempDir(), "out.tar.gz")
			_, err := runWorkspaceExport(t, handler, "--no-versions", "-o", dst)
			if tc.wantRefuse {
				if err == nil {
					t.Fatal("expected refusal error, got nil")
				}
				if !strings.Contains(err.Error(), "this server cannot export without version history; upgrade it") {
					t.Errorf("error = %q, want upgrade refusal message", err.Error())
				}
				if exportCalled {
					t.Error("export endpoint was called despite missing capability")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !exportCalled {
				t.Fatal("expected export endpoint to be called")
			}
			if !strings.Contains(gotQuery, "format=tar") || !strings.Contains(gotQuery, "include_versions=false") {
				t.Errorf("query = %q, want format=tar and include_versions=false", gotQuery)
			}
		})
	}
}

func TestSweepExportSpoolDirRemovesLeftoverSpools(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, "pad-export-12345.json")
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(leftover, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}

	sweepExportSpoolDir(dir)

	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("leftover spool file still exists: err=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated file should remain: %v", err)
	}
}
