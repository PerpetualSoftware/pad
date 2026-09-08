package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2951, the fourth door. The workspace-import path answered 500
// import_failed for EVERY failure, including the prefix-grammar refusal —
// whose message names the offending collection and tells the caller to edit
// the prefix in the export and import again. An actionable instruction
// delivered under a status that says "the server broke, try later" is a
// refusal a caller cannot act on and a retry loop for anything automated.
//
// The sibling bundle-import door already answered 400 for this class, so the
// two doors onto one refusal disagreed; this pins the alignment.
func TestImportWorkspace_UnimportablePrefixIs400WithTheReason(t *testing.T) {
	srv, user := importLimitFixture(t, 0)

	// A collection whose prefix cannot be derived OR validated: "!!" is not a
	// legal prefix, and a name of "!!" leaves DerivePrefix nothing to work
	// with either, so the import must refuse rather than seed a workspace
	// whose ids could never be formed.
	body, err := json.Marshal(models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Imported", Slug: "imported"},
		Collections: []models.CollectionExport{{
			Name:   "!!",
			Slug:   "bad-prefix",
			Prefix: "!!",
			Schema: `{"fields":[]}`,
		}},
	})
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}

	rr := importRequest(t, srv, user, "application/json", body)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rr.Body.String())
	}
	if env.Error.Code != "import_failed" {
		t.Errorf("code = %q, want import_failed (unchanged — only the status moved)", env.Error.Code)
	}
	// The instruction is the point: a caller must learn WHICH collection to
	// edit and what to edit it to. Status alone would pass with an empty body.
	if strings.Contains(env.Error.Message, "store: validation") {
		t.Errorf("message leaks the internal sentinel prefix: %q (render ValidationError.Reason, not Error())", env.Error.Message)
	}
	for _, want := range []string{"import collection", "edit the prefix"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message = %q, want it to contain %q", env.Error.Message, want)
		}
	}
}

// TestImportWorkspace_RealFailuresStayA500 is the counterfactual. Mapping the
// caller-input refusal to 400 must not turn every import failure into one — a
// server-side fault under a 4xx tells the caller to fix input that was fine.
// Version 0 is refused inside ImportWorkspace as an unsupported export, which
// is not a ValidationError, so it must still surface as a 500.
func TestImportWorkspace_RealFailuresStayA500(t *testing.T) {
	srv, user := importLimitFixture(t, 0)

	body, err := json.Marshal(models.WorkspaceExport{
		Workspace: models.WorkspaceExportMeta{Name: "Imported", Slug: "imported"},
	})
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}

	rr := importRequest(t, srv, user, "application/json", body)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 for a non-validation import failure; body = %s", rr.Code, rr.Body.String())
	}
}

// TestImportBundle_UnimportablePrefixKeepsTheReasonClean is the consumer
// codex round 1 caught: the tar.gz door renders a bundle failure through its
// own envelope, and its fallback path wraps err.Error(). Making the JSON door
// actionable moved the sentinel prefix INTO that text, so the change that
// improved one door would have made its sibling's message worse — the producer
// change was not finished until the consumers had been read.
func TestImportBundle_UnimportablePrefixKeepsTheReasonClean(t *testing.T) {
	// The bundle door needs attachment storage configured — without it the
	// request is refused 503 before it reaches the import at all, which is how
	// the first version of this test "failed" for a reason unrelated to what
	// it asserts.
	srv, _ := testServerWithAttachments(t)
	user, uerr := srv.store.CreateUser(models.UserCreate{
		Email: "bundle-importer@example.com", Name: "Importer",
		Password: "correct-horse-battery-staple", Role: "member",
	})
	if uerr != nil {
		t.Fatalf("create user: %v", uerr)
	}

	export := models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Bundled", Slug: "bundled"},
		Collections: []models.CollectionExport{{
			Name:   "!!",
			Slug:   "bad-prefix",
			Prefix: "!!",
			Schema: `{"fields":[]}`,
		}},
	}
	exportJSON, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal export: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "pad-export.json", Mode: 0o600, Size: int64(len(exportJSON))}); err != nil {
		t.Fatalf("tar header: %v", err)
	}
	if _, err := tw.Write(exportJSON); err != nil {
		t.Fatalf("tar write: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	rr := importRequest(t, srv, user, "application/gzip", buf.Bytes())

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rr.Code, rr.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body=%s)", err, rr.Body.String())
	}
	if strings.Contains(env.Error.Message, "store: validation") {
		t.Errorf("message leaks the internal sentinel prefix: %q", env.Error.Message)
	}
	if !strings.Contains(env.Error.Message, "edit the prefix") {
		t.Errorf("message = %q, want the actionable instruction", env.Error.Message)
	}
}
