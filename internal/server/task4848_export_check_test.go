package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

type exportMetadataTooLargeEnvelope struct {
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

func TestTask4848_ExportCheckTarReturns204NoBody(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)
	seedExportLimitWorkspace(t, srv, slug)
	uploadDistinctPNGs(t, srv, slug, []string{"sample.png"})

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar&check=true", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("body length = %d, want 0", rr.Body.Len())
	}
	if got := rr.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want empty", got)
	}
	if got := rr.Header().Get("Trailer"); got != "" {
		t.Errorf("Trailer = %q, want empty", got)
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "application/gzip") {
		t.Errorf("Content-Type = %q, must not be application/gzip", ct)
	}
	assertSpoolDirEmpty(t, spoolDir)
}

func TestTask4848_ExportCheckJSONReturns204NoBody(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)
	seedExportLimitWorkspace(t, srv, slug)

	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?check=true", nil)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rr.Code, rr.Body.String())
	}
	if rr.Body.Len() != 0 {
		t.Errorf("body length = %d, want 0", rr.Body.Len())
	}
	if got := rr.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("Content-Disposition = %q, want empty", got)
	}
	assertSpoolDirEmpty(t, spoolDir)
}

func TestTask4848_ExportCheckMetadataLimitRefusal(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)
	seedExportLimitWorkspace(t, srv, slug)

	jsonMeasure := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
	if jsonMeasure.Code != http.StatusOK {
		t.Fatalf("measure json: %d %s", jsonMeasure.Code, jsonMeasure.Body.String())
	}
	jsonSize := int64(jsonMeasure.Body.Len())

	tarMeasure := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if tarMeasure.Code != http.StatusOK {
		t.Fatalf("measure tar: %d %s", tarMeasure.Code, tarMeasure.Body.String())
	}
	tarExportSize := int64(len(readBundle(t, tarMeasure.Body.Bytes())["pad-export.json"]))

	cases := []struct {
		name      string
		realPath  string
		checkPath string
		limit     int64
	}{
		{
			name:      "json",
			realPath:  "/api/v1/workspaces/" + slug + "/export",
			checkPath: "/api/v1/workspaces/" + slug + "/export?check=true",
			limit:     jsonSize - 1,
		},
		{
			name:      "tar",
			realPath:  "/api/v1/workspaces/" + slug + "/export?format=tar",
			checkPath: "/api/v1/workspaces/" + slug + "/export?format=tar&check=true",
			limit:     tarExportSize - 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.SetExportMetadataMaxBytes(tc.limit)
			defer srv.SetExportMetadataMaxBytes(0)

			realRR := doRequest(srv, "GET", tc.realPath, nil)
			checkRR := doRequest(srv, "GET", tc.checkPath, nil)

			if checkRR.Code != http.StatusUnprocessableEntity {
				t.Fatalf("check status = %d, want 422; body=%s", checkRR.Code, checkRR.Body.String())
			}
			if realRR.Code != checkRR.Code {
				t.Fatalf("status mismatch: real=%d check=%d", realRR.Code, checkRR.Code)
			}

			var realEnv, checkEnv exportMetadataTooLargeEnvelope
			if err := json.Unmarshal(realRR.Body.Bytes(), &realEnv); err != nil {
				t.Fatalf("decode real 422: %v (%s)", err, realRR.Body.String())
			}
			if err := json.Unmarshal(checkRR.Body.Bytes(), &checkEnv); err != nil {
				t.Fatalf("decode check 422: %v (%s)", err, checkRR.Body.String())
			}
			if checkEnv.Error.Code != "export_too_large" {
				t.Errorf("code = %q, want export_too_large", checkEnv.Error.Code)
			}
			if checkEnv.Error.Details.Part != "export" {
				t.Errorf("details.part = %q, want %q", checkEnv.Error.Details.Part, "export")
			}
			if !checkEnv.Error.Details.IncludeVersions {
				t.Errorf("details.include_versions = false, want true")
			}
			if checkEnv.Error.Details.LimitBytes != tc.limit {
				t.Errorf("details.limit_bytes = %d, want %d", checkEnv.Error.Details.LimitBytes, tc.limit)
			}
			if checkEnv.Error.Details.SizeReached <= tc.limit {
				t.Errorf("details.size_reached = %d, want > %d", checkEnv.Error.Details.SizeReached, tc.limit)
			}
			if checkEnv.Error.Message != realEnv.Error.Message {
				t.Errorf("message mismatch:\ncheck: %q\n real: %q", checkEnv.Error.Message, realEnv.Error.Message)
			}
			assertSpoolDirEmpty(t, spoolDir)
		})
	}
}

func TestTask4848_ExportCheckManifestLimitRefusal(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)

	// Upload attachments with long filenames so attachments/manifest.json is
	// strictly larger than pad-export.json (which carries no attachments).
	longNames := make([]string, 25)
	for i := range longNames {
		longNames[i] = fmt.Sprintf("attachment-%02d-%s.png", i, strings.Repeat("a", 180))
	}
	uploadDistinctPNGs(t, srv, slug, longNames)

	baseline := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)
	if baseline.Code != http.StatusOK {
		t.Fatalf("baseline tar export: %d %s", baseline.Code, baseline.Body.String())
	}
	files := readBundle(t, baseline.Body.Bytes())
	exportSize := int64(len(files["pad-export.json"]))
	manifestSize := int64(len(files["attachments/manifest.json"]))
	if manifestSize <= exportSize {
		t.Fatalf("precondition: manifestSize (%d) must exceed exportSize (%d)", manifestSize, exportSize)
	}

	// Place limit strictly between pad-export.json and attachments/manifest.json.
	limit := exportSize + (manifestSize-exportSize)/2
	if limit < exportSize || limit >= manifestSize {
		t.Fatalf("bad limit %d for exportSize=%d manifestSize=%d", limit, exportSize, manifestSize)
	}
	srv.SetExportMetadataMaxBytes(limit)
	defer srv.SetExportMetadataMaxBytes(0)

	checkRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar&check=true", nil)
	realRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)

	if checkRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("check status = %d, want 422; body=%s", checkRR.Code, checkRR.Body.String())
	}
	if realRR.Code != checkRR.Code {
		t.Fatalf("status mismatch: real=%d check=%d", realRR.Code, checkRR.Code)
	}
	if !bytes.Equal(realRR.Body.Bytes(), checkRR.Body.Bytes()) {
		t.Errorf("body mismatch between real and check:\ncheck: %s\n real: %s", checkRR.Body.String(), realRR.Body.String())
	}

	var env exportMetadataTooLargeEnvelope
	if err := json.Unmarshal(checkRR.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode 422: %v (%s)", err, checkRR.Body.String())
	}
	if env.Error.Code != "export_too_large" {
		t.Errorf("code = %q, want export_too_large", env.Error.Code)
	}
	if env.Error.Details.Part != "manifest" {
		t.Errorf("details.part = %q, want %q", env.Error.Details.Part, "manifest")
	}
	if env.Error.Details.LimitBytes != limit {
		t.Errorf("details.limit_bytes = %d, want %d", env.Error.Details.LimitBytes, limit)
	}
	if env.Error.Details.SizeReached != manifestSize {
		t.Errorf("details.size_reached = %d, want %d", env.Error.Details.SizeReached, manifestSize)
	}
	if !env.Error.Details.IncludeVersions {
		t.Errorf("details.include_versions = false, want true")
	}
	if !strings.Contains(env.Error.Message, "--json") {
		t.Errorf("manifest refusal message must mention --json: %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "PAD_EXPORT_METADATA_MAX_BYTES") {
		t.Errorf("manifest refusal message must mention PAD_EXPORT_METADATA_MAX_BYTES: %q", env.Error.Message)
	}
	if strings.Contains(env.Error.Message, "--no-versions") || strings.Contains(env.Error.Message, "include_versions=false") {
		t.Errorf("manifest refusal message must not mention --no-versions or include_versions=false: %q", env.Error.Message)
	}
	assertSpoolDirEmpty(t, spoolDir)
}

func TestTask4848_ExportCheckEntryCapRefusal(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)

	uploadDistinctPNGs(t, srv, slug, []string{"one.png", "two.png", "three.png"})
	srv.importBundleMaxEntriesOverride = 4
	defer func() { srv.importBundleMaxEntriesOverride = 0 }()

	checkRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar&check=true", nil)
	realRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)

	if checkRR.Code != http.StatusUnprocessableEntity {
		t.Fatalf("check status = %d, want 422; body=%s", checkRR.Code, checkRR.Body.String())
	}
	if realRR.Code != checkRR.Code {
		t.Fatalf("status mismatch: real=%d check=%d", realRR.Code, checkRR.Code)
	}
	if !bytes.Equal(realRR.Body.Bytes(), checkRR.Body.Bytes()) {
		t.Errorf("body mismatch between real and check:\ncheck: %s\n real: %s", checkRR.Body.String(), realRR.Body.String())
	}

	var env exportEntryTooLargeEnvelope
	if err := json.Unmarshal(checkRR.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode 422: %v (%s)", err, checkRR.Body.String())
	}
	if env.Error.Code != "export_too_large" {
		t.Errorf("code = %q, want export_too_large", env.Error.Code)
	}
	if env.Error.Details.Entries != 5 || env.Error.Details.LimitEntries != 4 {
		t.Errorf("entries/limit = %d/%d, want 5/4", env.Error.Details.Entries, env.Error.Details.LimitEntries)
	}
	assertSpoolDirEmpty(t, spoolDir)
}

func TestTask4848_ExportCheckAttachmentsDisabled503(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	slug := createWSForTest(t, srv)
	spoolDir := t.TempDir()
	srv.SetExportSpoolDir(spoolDir)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("GetWorkspaceBySlug(%q): ws=%v err=%v", slug, ws, err)
	}
	if err := srv.store.CreateAttachment(&models.Attachment{
		WorkspaceID: ws.ID,
		UploadedBy:  "system",
		StorageKey:  "fs:disabled-check-blob",
		ContentHash: "disabled-check-hash",
		MimeType:    "image/png",
		SizeBytes:   16,
		Filename:    "a.png",
	}); err != nil {
		t.Fatalf("CreateAttachment: %v", err)
	}

	checkRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar&check=true", nil)
	realRR := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar", nil)

	if checkRR.Code != http.StatusServiceUnavailable {
		t.Fatalf("check status = %d, want 503; body=%s", checkRR.Code, checkRR.Body.String())
	}
	if realRR.Code != checkRR.Code || !bytes.Equal(realRR.Body.Bytes(), checkRR.Body.Bytes()) {
		t.Errorf("mismatch between check (%d %s) and real (%d %s)",
			checkRR.Code, checkRR.Body.String(), realRR.Code, realRR.Body.String())
	}
	assertSpoolDirEmpty(t, spoolDir)
}

func TestTask4848_ExportCheckInvalidAndGatesFirst(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)

	for _, path := range []string{
		"/api/v1/workspaces/" + slug + "/export?check=bogus",
		"/api/v1/workspaces/" + slug + "/export?format=tar&check=bogus",
	} {
		rr := doRequest(srv, "GET", path, nil)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body=%s", path, rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "validation_error") {
			t.Errorf("%s: want validation_error, got %s", path, body)
		}
		if !strings.Contains(body, "check") {
			t.Errorf("%s: message must name check, got %s", path, body)
		}
	}

	// Non-owner (editor) and restricted owner both get their access refusal
	// before check is parsed, identical to the request without check.
	f := newRestrictedOwnerVisibilityFixture(t)
	editor, err := f.srv.store.CreateUser(models.UserCreate{
		Email: "export-check-editor@example.com", Name: "Export Check Editor", Username: "export-check-editor", Password: "pw-test-12345",
	})
	if err != nil {
		t.Fatalf("create editor: %v", err)
	}
	if err := f.srv.store.AddWorkspaceMember(f.ws.ID, editor.ID, "editor"); err != nil {
		t.Fatalf("add editor: %v", err)
	}
	editorTok, err := f.srv.store.CreateAPIToken(editor.ID, models.APITokenCreate{
		Name: "editor-pat", WorkspaceID: f.ws.ID,
	}, 0, 0)
	if err != nil {
		t.Fatalf("create editor token: %v", err)
	}
	editorHeaders := map[string]string{"Authorization": "Bearer " + editorTok.Token}

	for _, suffix := range []string{"", "?format=tar"} {
		basePath := "/api/v1/workspaces/" + f.ws.Slug + "/export" + suffix
		sep := "?"
		if suffix != "" {
			sep = "&"
		}
		bogusPath := basePath + sep + "check=bogus"

		// Non-owner caller
		wantNonOwner := doRequestWithHeaders(f.srv, "GET", basePath, nil, editorHeaders)
		gotNonOwner := doRequestWithHeaders(f.srv, "GET", bogusPath, nil, editorHeaders)
		if gotNonOwner.Code != http.StatusForbidden || gotNonOwner.Code != wantNonOwner.Code {
			t.Errorf("non-owner %s: status = %d (want %d / 403); body=%s", bogusPath, gotNonOwner.Code, wantNonOwner.Code, gotNonOwner.Body.String())
		}
		if !bytes.Equal(gotNonOwner.Body.Bytes(), wantNonOwner.Body.Bytes()) {
			t.Errorf("non-owner %s body mismatch:\n got: %s\nwant: %s", bogusPath, gotNonOwner.Body.String(), wantNonOwner.Body.String())
		}

		// Restricted owner caller
		wantRestricted := doRequestWithCookie(f.srv, "GET", basePath, nil, f.sessionToken)
		gotRestricted := doRequestWithCookie(f.srv, "GET", bogusPath, nil, f.sessionToken)
		if gotRestricted.Code != http.StatusForbidden || gotRestricted.Code != wantRestricted.Code {
			t.Errorf("restricted owner %s: status = %d (want %d / 403); body=%s", bogusPath, gotRestricted.Code, wantRestricted.Code, gotRestricted.Body.String())
		}
		if !bytes.Equal(gotRestricted.Body.Bytes(), wantRestricted.Body.Bytes()) {
			t.Errorf("restricted owner %s body mismatch:\n got: %s\nwant: %s", bogusPath, gotRestricted.Body.String(), wantRestricted.Body.String())
		}
	}
}

func TestTask4848_ExportCheckFalseAndZeroReturnFullExport(t *testing.T) {
	t.Parallel()
	srv, slug := testServerWithAttachments(t)
	seedExportLimitWorkspace(t, srv, slug)
	uploadDistinctPNGs(t, srv, slug, []string{"pic.png"})

	for _, val := range []string{"false", "0"} {
		t.Run("tar_check_"+val, func(t *testing.T) {
			rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?format=tar&check="+val, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			if ct := rr.Header().Get("Content-Type"); ct != "application/gzip" {
				t.Errorf("Content-Type = %q, want application/gzip", ct)
			}
			files := readBundle(t, rr.Body.Bytes())
			if len(files["pad-export.json"]) == 0 || len(files["attachments/manifest.json"]) == 0 {
				t.Errorf("expected non-empty tar bundle files, got keys: %v", len(files))
			}
		})

		t.Run("json_check_"+val, func(t *testing.T) {
			rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export?check="+val, nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
			}
			var exp models.WorkspaceExport
			if err := json.Unmarshal(rr.Body.Bytes(), &exp); err != nil {
				t.Fatalf("expected valid JSON export body: %v", err)
			}
			if exp.Workspace.Slug != slug {
				t.Errorf("workspace slug = %q, want %q", exp.Workspace.Slug, slug)
			}
		})
	}
}
