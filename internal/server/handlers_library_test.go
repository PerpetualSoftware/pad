package server

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

// TestConventionLibrary_NoParams verifies the default endpoint shape stays
// stable — the web UI library page and the MCP dispatcher both consume this
// without query params and expect full bodies.
func TestConventionLibrary_NoParams(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/convention-library", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Categories []collections.LibraryCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) == 0 {
		t.Fatal("expected at least one category in the library")
	}
	// Spot-check: a known category and a known full-content body.
	foundGit := false
	for _, cat := range resp.Categories {
		if cat.Name == "git" {
			foundGit = true
			if len(cat.Conventions) == 0 {
				t.Errorf("git category has no conventions")
			}
			for _, conv := range cat.Conventions {
				if conv.Content == "" {
					t.Errorf("expected full content for convention %q, got empty", conv.Title)
				}
			}
		}
	}
	if !foundGit {
		t.Errorf("expected git category in convention library")
	}
}

// TestConventionLibrary_CategoryFilter — ?category=git returns only git.
func TestConventionLibrary_CategoryFilter(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/convention-library?category=git", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	var resp struct {
		Categories []collections.LibraryCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) != 1 {
		t.Fatalf("expected exactly 1 category, got %d", len(resp.Categories))
	}
	if resp.Categories[0].Name != "git" {
		t.Errorf("expected category name 'git', got %q", resp.Categories[0].Name)
	}
}

// TestConventionLibrary_UnknownCategory — empty categories slice, NOT 404.
// The library itself exists; the filter just produced no rows.
func TestConventionLibrary_UnknownCategory(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/convention-library?category=nonexistent-zzz", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Categories []collections.LibraryCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) != 0 {
		t.Errorf("expected 0 categories for unknown filter, got %d", len(resp.Categories))
	}
}

// TestPlaybookLibrary_NoParams — legacy shape: full Content, no Summary.
func TestPlaybookLibrary_NoParams(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/playbook-library", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) == 0 {
		t.Fatal("expected at least one playbook category")
	}
	for _, cat := range resp.Categories {
		for _, pb := range cat.Playbooks {
			if pb.Content == "" {
				t.Errorf("expected full content for playbook %q (no summary mode), got empty", pb.Title)
			}
			if pb.Summary != "" {
				t.Errorf("expected empty summary for playbook %q without ?summary=true, got %q", pb.Title, pb.Summary)
			}
		}
	}
}

// TestPlaybookLibrary_CategoryFilter — ?category=agent-workflows.
func TestPlaybookLibrary_CategoryFilter(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/playbook-library?category=agent-workflows", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) != 1 {
		t.Fatalf("expected exactly 1 category, got %d", len(resp.Categories))
	}
	if resp.Categories[0].Name != "agent-workflows" {
		t.Errorf("expected agent-workflows, got %q", resp.Categories[0].Name)
	}
}

// TestPlaybookLibrary_SummaryMode — Content stripped, Summary populated.
func TestPlaybookLibrary_SummaryMode(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/playbook-library?summary=true", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) == 0 {
		t.Fatal("expected at least one playbook category")
	}
	checked := 0
	for _, cat := range resp.Categories {
		for _, pb := range cat.Playbooks {
			if pb.Content != "" {
				t.Errorf("expected empty content in summary mode for %q, got %d chars", pb.Title, len(pb.Content))
			}
			if pb.Summary == "" {
				t.Errorf("expected non-empty summary for %q", pb.Title)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no playbooks found to assert against")
	}
}

// TestPlaybookLibrary_SummaryDoesNotMutateGlobal — guards against the
// summary-mode handler clobbering the package-level library data via
// shared slice backing. A second non-summary request must still see full
// bodies after a summary request.
func TestPlaybookLibrary_SummaryDoesNotMutateGlobal(t *testing.T) {
	srv := testServer(t)

	// Request with summary=true first.
	rr := doRequest(srv, "GET", "/api/v1/playbook-library?summary=true", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("summary req: expected 200, got %d", rr.Code)
	}

	// Now request without summary — bodies must be back to full.
	rr = doRequest(srv, "GET", "/api/v1/playbook-library", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("plain req: expected 200, got %d", rr.Code)
	}
	var resp struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	for _, cat := range resp.Categories {
		for _, pb := range cat.Playbooks {
			if pb.Content == "" {
				t.Errorf("library data was mutated by a prior summary request: playbook %q has empty Content", pb.Title)
			}
		}
	}

	// And the global library accessor still has full bodies.
	for _, cat := range collections.PlaybookLibrary() {
		for _, pb := range cat.Playbooks {
			if pb.Content == "" {
				t.Errorf("global PlaybookLibrary() mutated: %q has empty Content", pb.Title)
			}
		}
	}
}

// TestPlaybookLibrary_CategoryAndSummary — both flags together.
func TestPlaybookLibrary_CategoryAndSummary(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/playbook-library?category=agent-workflows&summary=true", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	var resp struct {
		Categories []collections.PlaybookCategory `json:"categories"`
	}
	parseJSON(t, rr, &resp)
	if len(resp.Categories) != 1 {
		t.Fatalf("expected 1 category, got %d", len(resp.Categories))
	}
	for _, pb := range resp.Categories[0].Playbooks {
		if pb.Content != "" {
			t.Errorf("expected empty content in combined mode for %q", pb.Title)
		}
		if pb.Summary == "" {
			t.Errorf("expected non-empty summary for %q", pb.Title)
		}
	}
}

// TestLibraryEntry_Convention — title resolves to a convention, full body.
func TestLibraryEntry_Convention(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/library/entry?title="+url.QueryEscape("Commit after task completion"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp libraryEntryResponse
	parseJSON(t, rr, &resp)
	if resp.Type != "convention" {
		t.Errorf("expected type=convention, got %q", resp.Type)
	}
	if resp.Convention == nil {
		t.Fatal("expected Convention to be set")
	}
	if resp.Convention.Content == "" {
		t.Error("expected full content on convention entry")
	}
	if resp.Playbook != nil {
		t.Error("expected Playbook to be nil for convention entry")
	}
}

// TestLibraryEntry_Playbook — title resolves to a playbook, full body.
func TestLibraryEntry_Playbook(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/library/entry?title="+url.QueryEscape("Ship tasks"), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp libraryEntryResponse
	parseJSON(t, rr, &resp)
	if resp.Type != "playbook" {
		t.Errorf("expected type=playbook, got %q", resp.Type)
	}
	if resp.Playbook == nil {
		t.Fatal("expected Playbook to be set")
	}
	if resp.Playbook.Content == "" {
		t.Error("expected full content on playbook entry")
	}
	if resp.Playbook.InvocationSlug != "ship" {
		t.Errorf("expected invocation_slug=ship, got %q", resp.Playbook.InvocationSlug)
	}
	if resp.Convention != nil {
		t.Error("expected Convention to be nil for playbook entry")
	}
}

// TestLibraryEntry_MissingTitle — 400 in the canonical {error: {code, message}}
// envelope (matches the rest of the API). CLI parseError relies on this shape.
func TestLibraryEntry_MissingTitle(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/library/entry", nil)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing title, got %d", rr.Code)
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	parseJSON(t, rr, &resp)
	if resp.Error.Code != "bad_request" {
		t.Errorf("expected error.code=bad_request, got %q", resp.Error.Code)
	}
	if resp.Error.Message == "" {
		t.Error("expected non-empty error.message")
	}
}

// TestLibraryEntry_NotFound — 404 with the canonical envelope.
func TestLibraryEntry_NotFound(t *testing.T) {
	srv := testServer(t)
	rr := doRequest(srv, "GET", "/api/v1/library/entry?title="+url.QueryEscape("definitely not a real library entry"), nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rr.Code)
	}
	var resp struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	parseJSON(t, rr, &resp)
	if resp.Error.Code != "not_found" {
		t.Errorf("expected error.code=not_found, got %q", resp.Error.Code)
	}
}
