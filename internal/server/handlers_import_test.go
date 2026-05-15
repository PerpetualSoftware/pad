package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/urlimport"
)

// withLocalFetcher replaces the package-level fetcher with one that
// allows loopback dials so httptest.NewServer addresses (127.0.0.1) can
// be reached. Restored on test cleanup. Tests use this helper instead
// of mutating pkgFetcher directly so the restore happens unconditionally.
func withLocalFetcher(t *testing.T) {
	t.Helper()
	orig := pkgFetcher
	f := urlimport.NewFetcher()
	f.AllowLocal = true
	pkgFetcher = f
	t.Cleanup(func() {
		pkgFetcher = orig
	})
}

func TestImportURL_HTMLHappyPath(t *testing.T) {
	withLocalFetcher(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, `<!doctype html><html><head><title>Hello</title></head><body><article><h1>Hello world</h1><p>This is body text.</p></article></body></html>`)
	}))
	defer upstream.Close()

	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": upstream.URL + "/page",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Markdown     string `json:"markdown"`
		DetectedType string `json:"detected_type"`
		Title        string `json:"title"`
		SourceURL    string `json:"source_url"`
		FetchedAt    string `json:"fetched_at"`
		ContentType  string `json:"content_type"`
	}
	parseJSON(t, rr, &resp)
	if resp.DetectedType != "generic" {
		t.Errorf("detected_type = %q, want 'generic'", resp.DetectedType)
	}
	if !strings.Contains(resp.Markdown, "Hello world") {
		t.Errorf("markdown missing 'Hello world':\n%s", resp.Markdown)
	}
	if !strings.HasPrefix(resp.SourceURL, upstream.URL) {
		t.Errorf("source_url = %q, want to start with %q", resp.SourceURL, upstream.URL)
	}
	if resp.FetchedAt == "" {
		t.Error("fetched_at is empty")
	}
	if resp.ContentType == "" {
		t.Error("content_type is empty")
	}
}

func TestImportURL_OpenAPIHappyPath(t *testing.T) {
	withLocalFetcher(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = io.WriteString(w, `openapi: 3.0.0
info:
  title: Inline API
  version: "1.0"
paths:
  /widgets:
    get:
      summary: List widgets
      responses:
        '200':
          description: ok
`)
	}))
	defer upstream.Close()

	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": upstream.URL + "/openapi.yaml",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Markdown     string `json:"markdown"`
		DetectedType string `json:"detected_type"`
		Title        string `json:"title"`
	}
	parseJSON(t, rr, &resp)
	if resp.DetectedType != "openapi" {
		t.Errorf("detected_type = %q, want 'openapi'", resp.DetectedType)
	}
	if resp.Title != "Inline API" {
		t.Errorf("title = %q, want 'Inline API'", resp.Title)
	}
	wantSubstrings := []string{
		"# Inline API",
		"## Endpoints",
		"`GET /widgets`",
		"List widgets",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(resp.Markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, resp.Markdown)
		}
	}
}

func TestImportURL_Swagger2FallsBackToGeneric(t *testing.T) {
	withLocalFetcher(t)
	// Swagger 2.0 is detected as "openapi" by the sniffer but
	// ConvertOpenAPI rejects it. The endpoint must fall back to generic
	// so the user still gets usable output (the raw text).
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = io.WriteString(w, `swagger: "2.0"
info:
  title: Legacy
  version: "1.0"
paths: {}
`)
	}))
	defer upstream.Close()

	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": upstream.URL,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		DetectedType string `json:"detected_type"`
		Markdown     string `json:"markdown"`
	}
	parseJSON(t, rr, &resp)
	if resp.DetectedType != "generic" {
		t.Errorf("detected_type = %q, want 'generic' after Swagger 2 fallback", resp.DetectedType)
	}
	if resp.Markdown == "" {
		t.Error("markdown is empty")
	}
}

func TestImportURL_RejectsSSRF(t *testing.T) {
	// pkgFetcher NOT replaced with AllowLocal — the default safe fetcher
	// rejects loopback at validate time. The handler's pre-flight
	// urlimport.ValidateURL catches this before any I/O.
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": "http://127.0.0.1:1/secret",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s, want 400", rr.Code, rr.Body.String())
	}
	body := strings.ToLower(rr.Body.String())
	if !strings.Contains(body, "private") && !strings.Contains(body, "reserved") {
		t.Errorf("response body = %s, want to mention private/reserved", rr.Body.String())
	}
}

func TestImportURL_RejectsNonHTTPScheme(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": "file:///etc/passwd",
	})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rr.Code, rr.Body.String())
	}
}

func TestImportURL_RejectsMissingURL(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{})
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rr.Code, rr.Body.String())
	}
}

func TestImportURL_RejectsMalformedBody(t *testing.T) {
	srv := testServer(t)
	req := httptest.NewRequest("POST", "/api/v1/import/url", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "192.0.2.1:1234"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rr.Code, rr.Body.String())
	}
}

func TestImportURL_UpstreamErrorIs502(t *testing.T) {
	withLocalFetcher(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": upstream.URL,
	})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rr.Code, rr.Body.String())
	}
}

func TestImportURL_SizeCapReturns502(t *testing.T) {
	// Replace the fetcher with one that has a tiny size cap; the
	// upstream sends more, so the read-time check returns an error
	// which we surface as 502 (the upstream did something we can't
	// recover from).
	orig := pkgFetcher
	f := urlimport.NewFetcher()
	f.AllowLocal = true
	f.MaxBytes = 256
	pkgFetcher = f
	t.Cleanup(func() { pkgFetcher = orig })

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, strings.Repeat("A", 4096))
	}))
	defer upstream.Close()

	srv := testServer(t)
	rr := doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": upstream.URL,
	})
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 for size-cap, body = %s", rr.Code, rr.Body.String())
	}
}

// TestImportURL_NoDatabaseSideEffects asserts the endpoint doesn't
// mutate any items. We import a URL and verify the item count stays
// at zero (no item creation, no metadata writes).
func TestImportURL_NoDatabaseSideEffects(t *testing.T) {
	withLocalFetcher(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, `<p>side-effect probe</p>`)
	}))
	defer upstream.Close()

	srv := testServer(t)
	slug := createWSForTest(t, srv)

	// Count items before.
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items before: status = %d", rr.Code)
	}
	var beforeItems []map[string]any
	parseJSON(t, rr, &beforeItems)
	before := len(beforeItems)

	// Import.
	rr = doRequest(srv, "POST", "/api/v1/import/url", map[string]string{
		"url": upstream.URL,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("import status = %d, body = %s", rr.Code, rr.Body.String())
	}

	// Count items after.
	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list items after: status = %d", rr.Code)
	}
	var afterItems []map[string]any
	parseJSON(t, rr, &afterItems)
	if len(afterItems) != before {
		t.Errorf("item count changed: before=%d after=%d — endpoint must be side-effect-free", before, len(afterItems))
	}
}
