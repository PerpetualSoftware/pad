package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestExportItemArtifact_HappyPath confirms the client GETs the per-item
// export endpoint, returns the artifact bytes verbatim, and lifts the
// download filename from the Content-Disposition header.
func TestExportItemArtifact_HappyPath(t *testing.T) {
	const wantBody = "---\nkind: playbook\n---\n# Ship\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/workspaces/acme/items/PLAYB-3/export" {
			t.Errorf("path = %s, want export path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="ship.pad.md"`)
		_, _ = io.WriteString(w, wantBody)
	}))
	defer srv.Close()

	client := NewClientFromURL(srv.URL)
	res, err := client.ExportItemArtifact("acme", "PLAYB-3")
	if err != nil {
		t.Fatalf("ExportItemArtifact: %v", err)
	}
	if string(res.Body) != wantBody {
		t.Errorf("body = %q, want %q", res.Body, wantBody)
	}
	if res.Filename != "ship.pad.md" {
		t.Errorf("filename = %q, want ship.pad.md", res.Filename)
	}
}

// TestExportItemArtifact_NonExportable confirms a 4xx server message (e.g. a
// non-playbook/convention ref) is surfaced as the API error message.
func TestExportItemArtifact_NonExportable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "unsupported_collection",
				"message": "Only playbooks and conventions can be exported as artifacts",
			},
		})
	}))
	defer srv.Close()

	client := NewClientFromURL(srv.URL)
	_, err := client.ExportItemArtifact("acme", "TASK-5")
	if err == nil {
		t.Fatal("ExportItemArtifact returned nil error for a non-exportable ref")
	}
	if !strings.Contains(err.Error(), "Only playbooks and conventions") {
		t.Errorf("err = %q, want the server message", err.Error())
	}
}

// TestImportArtifact_HappyPath confirms the client POSTs the raw artifact
// bytes (not a JSON wrapper) and decodes the {ref, slug, warnings} response.
func TestImportArtifact_HappyPath(t *testing.T) {
	const body = "---\nkind: playbook\ntitle: Ship\n---\n# Ship\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/workspaces/acme/import-artifact" {
			t.Errorf("path = %s, want import path", r.URL.Path)
		}
		got, _ := io.ReadAll(r.Body)
		if string(got) != body {
			t.Errorf("request body = %q, want the raw artifact %q", got, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref":      "PLAYB-9",
			"slug":     "ship",
			"warnings": []string{"status \"active\" reset to \"draft\" on import"},
		})
	}))
	defer srv.Close()

	client := NewClientFromURL(srv.URL)
	res, err := client.ImportArtifact("acme", []byte(body))
	if err != nil {
		t.Fatalf("ImportArtifact: %v", err)
	}
	if res.Ref != "PLAYB-9" || res.Slug != "ship" {
		t.Errorf("got ref=%q slug=%q, want PLAYB-9/ship", res.Ref, res.Slug)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "draft") {
		t.Errorf("warnings = %v, want one draft-reset warning", res.Warnings)
	}
}

// TestImportArtifact_ServerError confirms a 4xx (e.g. malformed/oversized) is
// surfaced cleanly as the server's error message.
func TestImportArtifact_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"code":    "too_large",
				"message": "Artifact body exceeds the size limit",
			},
		})
	}))
	defer srv.Close()

	client := NewClientFromURL(srv.URL)
	_, err := client.ImportArtifact("acme", []byte("whatever"))
	if err == nil {
		t.Fatal("ImportArtifact returned nil error for an oversized body")
	}
	if !strings.Contains(err.Error(), "exceeds the size limit") {
		t.Errorf("err = %q, want the server message", err.Error())
	}
}

// TestFilenameFromContentDisposition covers the header-parsing helper,
// including the empty-header fallback path and path-traversal defense
// (a hostile server filename must reduce to a safe base name).
func TestFilenameFromContentDisposition(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{`attachment; filename="ship.pad.md"`, "ship.pad.md"},
		{`attachment; filename=plain.pad.md`, "plain.pad.md"},
		{"", ""},
		{"attachment", ""},
		// Path traversal / absolute path: must collapse to the base name.
		{`attachment; filename="../../etc/x"`, "x"},
		{`attachment; filename="/etc/passwd"`, "passwd"},
		{`attachment; filename="../../../tmp/evil.pad.md"`, "evil.pad.md"},
		// A filename that is nothing but traversal/separators is unusable;
		// caller falls back to a synthetic name.
		{`attachment; filename=".."`, ""},
		{`attachment; filename="."`, ""},
		{`attachment; filename="/"`, ""},
	}
	for _, tc := range cases {
		if got := filenameFromContentDisposition(tc.header); got != tc.want {
			t.Errorf("filenameFromContentDisposition(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}
