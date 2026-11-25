package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func seedExportLimitWorkspace(t *testing.T, srv *Server, slug string) {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/docs/items",
		map[string]any{"title": "History Item", "content": "initial body content"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d %s", rr.Code, rr.Body.String())
	}
	var item models.Item
	if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode item: %v", err)
	}
	for _, body := range []string{"second revision body", "third revision body"} {
		rr = doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
			map[string]any{"content": body})
		if rr.Code != http.StatusOK {
			t.Fatalf("update item: %d %s", rr.Code, rr.Body.String())
		}
	}
	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+item.Slug+"/comments",
		map[string]any{"body": "a workspace comment"})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create comment: %d %s", rr.Code, rr.Body.String())
	}
}

func makeTarGzWithExport(t *testing.T, exportJSON []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gzw)
	if err := tw.WriteHeader(&tar.Header{
		Name: "pad-export.json",
		Mode: 0o644,
		Size: int64(len(exportJSON)),
	}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(exportJSON); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	manifest := []byte(`{"version":1,"entries":[]}`)
	if err := tw.WriteHeader(&tar.Header{
		Name: "attachments/manifest.json",
		Mode: 0o644,
		Size: int64(len(manifest)),
	}); err != nil {
		t.Fatalf("manifest header: %v", err)
	}
	if _, err := tw.Write(manifest); err != nil {
		t.Fatalf("manifest write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func assertSpoolDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("spool dir %s not empty: %v", dir, names)
	}
}

func TestExportOverLimitRefusesBeforeAnyByte(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)
	seedExportLimitWorkspace(t, srv, slug)

	// Measure baseline sizes for both formats.
	jsonRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if jsonRR.Code != http.StatusOK {
		t.Fatalf("measure json export: %d %s", jsonRR.Code, jsonRR.Body.String())
	}
	jsonSize := int64(jsonRR.Body.Len())

	tarRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if tarRR.Code != http.StatusOK {
		t.Fatalf("measure tar export: %d %s", tarRR.Code, tarRR.Body.String())
	}
	tarFiles := readBundle(t, tarRR.Body.Bytes())
	tarExportSize := int64(len(tarFiles["pad-export.json"]))

	cases := []struct {
		name string
		path string
		size int64
	}{
		{name: "json", path: "/api/v1/workspaces/" + slug + "/export", size: jsonSize},
		{name: "tar", path: "/api/v1/workspaces/" + slug + "/export?format=tar", size: tarExportSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.SetExportMetadataMaxBytes(tc.size - 1)
			defer srv.SetExportMetadataMaxBytes(0)

			rr := doRequest(srv, "GET", tc.path, nil)
			if rr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body=%s", rr.Code, rr.Body.String())
			}
			if got := rr.Header().Get("Content-Disposition"); got != "" {
				t.Errorf("Content-Disposition = %q, want empty on 422", got)
			}
			if got := rr.Header().Get("Trailer"); got != "" {
				t.Errorf("Trailer = %q, want empty on 422", got)
			}
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}

			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Details struct {
						Part            string `json:"part"`
						SizeReached     int64  `json:"size_reached"`
						LimitBytes      int64  `json:"limit_bytes"`
						IncludeVersions bool   `json:"include_versions"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode 422 body: %v (raw=%s)", err, rr.Body.String())
			}
			if env.Error.Code != "export_too_large" {
				t.Errorf("code = %q, want export_too_large", env.Error.Code)
			}
			if env.Error.Details.Part != "export" {
				t.Errorf("part = %q, want export", env.Error.Details.Part)
			}
			if env.Error.Details.LimitBytes != tc.size-1 {
				t.Errorf("limit_bytes = %d, want %d", env.Error.Details.LimitBytes, tc.size-1)
			}
			if env.Error.Details.SizeReached <= env.Error.Details.LimitBytes {
				t.Errorf("size_reached = %d, want > %d", env.Error.Details.SizeReached, env.Error.Details.LimitBytes)
			}
			if !env.Error.Details.IncludeVersions {
				t.Errorf("include_versions = false, want true")
			}
			assertSpoolDirEmpty(t, spoolDir)
		})
	}
}

func TestExportAtLimitImportsOnSameServer(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)
	seedExportLimitWorkspace(t, srv, slug)

	t.Run("json", func(t *testing.T) {
		srv.SetExportMetadataMaxBytes(0)
		measure := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
		if measure.Code != http.StatusOK {
			t.Fatalf("measure json: %d", measure.Code)
		}
		limit := int64(measure.Body.Len())
		srv.SetExportMetadataMaxBytes(limit)
		defer srv.SetExportMetadataMaxBytes(0)

		expRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
		if expRR.Code != http.StatusOK {
			t.Fatalf("export at exact limit %d: got %d %s", limit, expRR.Code, expRR.Body.String())
		}
		req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=AtLimitJSON", bytes.NewReader(expRR.Body.Bytes()))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:1234"
		impRR := httptest.NewRecorder()
		srv.ServeHTTP(impRR, req)
		if impRR.Code != http.StatusCreated {
			t.Fatalf("import at exact limit %d: got %d %s", limit, impRR.Code, impRR.Body.String())
		}
		assertSpoolDirEmpty(t, spoolDir)
	})

	t.Run("tar", func(t *testing.T) {
		srv.SetExportMetadataMaxBytes(0)
		measure := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
		if measure.Code != http.StatusOK {
			t.Fatalf("measure tar: %d", measure.Code)
		}
		files := readBundle(t, measure.Body.Bytes())
		limit := int64(len(files["pad-export.json"]))
		srv.SetExportMetadataMaxBytes(limit)
		defer srv.SetExportMetadataMaxBytes(0)

		expRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
		if expRR.Code != http.StatusOK {
			t.Fatalf("tar export at exact limit %d: got %d %s", limit, expRR.Code, expRR.Body.String())
		}
		impRR := postBundle(srv, "AtLimitTar", expRR.Body.Bytes())
		if impRR.Code != http.StatusCreated {
			t.Fatalf("tar import at exact limit %d: got %d %s", limit, impRR.Code, impRR.Body.String())
		}
		assertSpoolDirEmpty(t, spoolDir)
	})
}

func TestImportLimitIsTheExportLimit(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	seedExportLimitWorkspace(t, srv, slug)

	baseRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if baseRR.Code != http.StatusOK {
		t.Fatalf("export: %d %s", baseRR.Code, baseRR.Body.String())
	}
	raw := bytes.TrimRight(baseRR.Body.Bytes(), "\n")
	limit := int64(len(raw)) + 64
	atLimit := append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), int(limit)-len(raw))...)
	overLimit := append(append([]byte(nil), atLimit...), ' ')

	srv.SetExportMetadataMaxBytes(limit)

	// JSON door: L+1 -> 413, L -> 201.
	{
		req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=JSONOver", bytes.NewReader(overLimit))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:1234"
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "bundle_too_large") {
			t.Fatalf("json L+1: got %d %s, want 413 bundle_too_large", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "PAD_EXPORT_METADATA_MAX_BYTES") {
			t.Errorf("json 413 should name PAD_EXPORT_METADATA_MAX_BYTES: %s", rr.Body.String())
		}

		req = httptest.NewRequest("POST", "/api/v1/workspaces/import?name=JSONAt", bytes.NewReader(atLimit))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "127.0.0.1:1234"
		rr = httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated {
			t.Fatalf("json L: got %d %s, want 201", rr.Code, rr.Body.String())
		}
	}

	// Bundle door: L+1 -> 413, L -> 201.
	{
		rr := postBundle(srv, "TarOver", makeTarGzWithExport(t, overLimit))
		if rr.Code != http.StatusRequestEntityTooLarge || !strings.Contains(rr.Body.String(), "bundle_too_large") {
			t.Fatalf("tar L+1: got %d %s, want 413 bundle_too_large", rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "PAD_EXPORT_METADATA_MAX_BYTES") {
			t.Errorf("tar 413 should name PAD_EXPORT_METADATA_MAX_BYTES: %s", rr.Body.String())
		}

		rr = postBundle(srv, "TarAt", makeTarGzWithExport(t, atLimit))
		if rr.Code != http.StatusCreated {
			t.Fatalf("tar L: got %d %s, want 201", rr.Code, rr.Body.String())
		}
	}
}

func TestIndentedLegacyExportImports(t *testing.T) {
	t.Parallel()
	src, srcSlug := testServerWithAttachments(t)
	seedExportLimitWorkspace(t, src, srcSlug)

	exp, err := src.store.ExportWorkspace(srcSlug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	indented, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent: %v", err)
	}

	dest, _ := testServerWithAttachments(t)

	// JSON door.
	req := httptest.NewRequest("POST", "/api/v1/workspaces/import?name=LegacyIndentedJSON", bytes.NewReader(indented))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:1234"
	jsonRR := httptest.NewRecorder()
	dest.ServeHTTP(jsonRR, req)
	if jsonRR.Code != http.StatusCreated {
		t.Fatalf("indented JSON import: %d %s", jsonRR.Code, jsonRR.Body.String())
	}
	var jsonWS models.Workspace
	if err := json.Unmarshal(jsonRR.Body.Bytes(), &jsonWS); err != nil {
		t.Fatalf("decode json ws: %v", err)
	}

	// Bundle door.
	tarRR := postBundle(dest, "LegacyIndentedTar", makeTarGzWithExport(t, indented))
	if tarRR.Code != http.StatusCreated {
		t.Fatalf("indented tar import: %d %s", tarRR.Code, tarRR.Body.String())
	}
	var tarWS models.Workspace
	if err := json.Unmarshal(tarRR.Body.Bytes(), &tarWS); err != nil {
		t.Fatalf("decode tar ws: %v", err)
	}

	jsonGot, err := dest.store.ExportWorkspace(jsonWS.Slug)
	if err != nil {
		t.Fatalf("re-export json ws: %v", err)
	}
	tarGot, err := dest.store.ExportWorkspace(tarWS.Slug)
	if err != nil {
		t.Fatalf("re-export tar ws: %v", err)
	}

	for name, got := range map[string]*models.WorkspaceExport{"json": jsonGot, "tar": tarGot} {
		if len(got.Collections) != len(exp.Collections) {
			t.Errorf("%s: collections=%d, want %d", name, len(got.Collections), len(exp.Collections))
		}
		if len(got.Items) != len(exp.Items) {
			t.Errorf("%s: items=%d, want %d", name, len(got.Items), len(exp.Items))
		}
		if len(got.Comments) != len(exp.Comments) {
			t.Errorf("%s: comments=%d, want %d", name, len(got.Comments), len(exp.Comments))
		}
		if len(got.ItemVersions) != len(exp.ItemVersions) {
			t.Errorf("%s: versions=%d, want %d", name, len(got.ItemVersions), len(exp.ItemVersions))
		}
	}
}

func TestExportWithoutVersionsDropsOnlyVersions(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	seedExportLimitWorkspace(t, srv, slug)

	fullRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if fullRR.Code != http.StatusOK {
		t.Fatalf("full export: %d %s", fullRR.Code, fullRR.Body.String())
	}
	var full models.WorkspaceExport
	if err := json.Unmarshal(fullRR.Body.Bytes(), &full); err != nil {
		t.Fatalf("unmarshal full: %v", err)
	}
	if len(full.ItemVersions) == 0 {
		t.Fatal("precondition: default export must contain item versions")
	}

	for _, tc := range []struct {
		name string
		path string
		tar  bool
	}{
		{name: "json", path: "/api/v1/workspaces/" + slug + "/export?include_versions=false", tar: false},
		{name: "tar", path: "/api/v1/workspaces/" + slug + "/export?format=tar&include_versions=false", tar: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := doRequest(srv, "GET", tc.path, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("export without versions: %d %s", rr.Code, rr.Body.String())
			}
			raw := rr.Body.Bytes()
			if tc.tar {
				raw = readBundle(t, raw)["pad-export.json"]
			}
			if bytes.Contains(raw, []byte(`"item_versions"`)) {
				t.Fatalf("expected no item_versions key when include_versions=false: %s", raw)
			}
			var trimmed models.WorkspaceExport
			if err := json.Unmarshal(raw, &trimmed); err != nil {
				t.Fatalf("unmarshal trimmed: %v", err)
			}
			if len(trimmed.ItemVersions) != 0 {
				t.Fatalf("expected 0 versions, got %d", len(trimmed.ItemVersions))
			}

			want := full
			want.ExportedAt = ""
			want.ItemVersions = nil
			got := trimmed
			got.ExportedAt = ""
			got.ItemVersions = nil
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("export without versions differs outside ItemVersions:\n got: %+v\nwant: %+v", got, want)
			}
		})
	}
}

func TestExportIncludeVersionsInvalid(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)

	for _, path := range []string{
		"/api/v1/workspaces/" + slug + "/export?include_versions=maybe",
		"/api/v1/workspaces/" + slug + "/export?format=tar&include_versions=maybe",
	} {
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%s", path, rr.Code, rr.Body.String())
		}
		if !strings.Contains(rr.Body.String(), "validation_error") {
			t.Errorf("%s: want validation_error, got %s", path, rr.Body.String())
		}
	}

	// Access gates run before include_versions parsing: a restricted owner sees
	// only 403 even with an invalid include_versions value.
	f := newRestrictedOwnerVisibilityFixture(t)
	for _, path := range []string{
		"/api/v1/workspaces/" + f.ws.Slug + "/export?include_versions=maybe",
		"/api/v1/workspaces/" + f.ws.Slug + "/export?format=tar&include_versions=maybe",
	} {
		rr := doRequestWithCookie(f.srv, "GET", path, nil, f.sessionToken)
		if rr.Code != http.StatusForbidden {
			t.Errorf("restricted owner %s: status = %d, want 403; body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestExportRefusalNamesRemedy(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	seedExportLimitWorkspace(t, srv, slug)
	srv.SetExportMetadataMaxBytes(16)

	// Default (include_versions=true): names both --no-versions and env var.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("default over limit: got %d %s, want 422", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, "--no-versions") || !strings.Contains(body, "PAD_EXPORT_METADATA_MAX_BYTES") {
		t.Errorf("default 422 must name --no-versions and PAD_EXPORT_METADATA_MAX_BYTES: %s", body)
	}

	// With include_versions=false: names only the env var, not --no-versions.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?include_versions=false", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("no-versions over limit: got %d %s, want 422", rr.Code, rr.Body.String())
	}
	body = rr.Body.String()
	if !strings.Contains(body, "PAD_EXPORT_METADATA_MAX_BYTES") {
		t.Errorf("no-versions 422 must name PAD_EXPORT_METADATA_MAX_BYTES: %s", body)
	}
	if strings.Contains(body, "--no-versions") || strings.Contains(body, "include_versions=false") {
		t.Errorf("no-versions 422 must omit the --no-versions remedy: %s", body)
	}
}
