package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2892. ImportWorkspace minted the workspace through CreateWorkspace —
// its own committed write — BEFORE opening the transaction that carries every
// other row. Eight error returns sit between that INSERT and the commit, and
// each one left the workspace row behind: named, slugged, owned by the caller,
// carrying no collections and no items.
//
// The second-order consequence is the user-visible one. uniqueWorkspaceSlug
// probes `WHERE slug = ? AND deleted_at IS NULL`, and a husk is not
// soft-deleted, so it HOLDS the slug: the retry that finally succeeds lands on
// `name-2`, and that slug is in every URL for the workspace from then on.

// failingImportBundle builds a bundle that imports successfully up to its
// second collection and then fails there, at
// `INSERT INTO collections ... UNIQUE(workspace_id, slug)`.
//
// The two collections share a SLUG and differ in every other respect, so the
// bundle has exactly one thing wrong with it. That matters: a fixture
// rejectable for two reasons cannot tell you which one the run hit, and this
// one is asserted on by message below.
func failingImportBundle(name, slug string) *models.WorkspaceExport {
	return &models.WorkspaceExport{
		Version:    1,
		ExportedAt: "2026-09-06T00:00:00Z",
		Workspace: models.WorkspaceExportMeta{
			Name:     name,
			Slug:     slug,
			Settings: "{}",
		},
		Collections: []models.CollectionExport{
			{
				ID:        "coll-first",
				Name:      "First",
				Slug:      "duplicated",
				Prefix:    "FIR",
				Schema:    `{"fields":[]}`,
				Settings:  "{}",
				CreatedAt: "2026-09-06T00:00:00Z",
				UpdatedAt: "2026-09-06T00:00:00Z",
			},
			{
				ID:        "coll-second",
				Name:      "Second",
				Slug:      "duplicated",
				Prefix:    "SEC",
				Schema:    `{"fields":[]}`,
				Settings:  "{}",
				CreatedAt: "2026-09-06T00:00:00Z",
				UpdatedAt: "2026-09-06T00:00:00Z",
			},
		},
	}
}

// validImportBundle is failingImportBundle with the slug collision removed and
// nothing else changed, so a retry differs from the failed attempt in exactly
// the way the operator's fix would.
func validImportBundle(name, slug string) *models.WorkspaceExport {
	b := failingImportBundle(name, slug)
	b.Collections[1].Slug = "distinct"
	return b
}

// TestImportWorkspaceFailureLeavesNoWorkspace is the first leg: a failed
// import leaves no workspace row at all.
//
// The counterfactual matters here — `GetWorkspaceBySlug` returning nil would
// also be satisfied by an import that never got as far as creating anything,
// so the test first pins that the failure is the one it engineered (the
// collection INSERT, not a decode or a version check) and that the import did
// fail rather than quietly succeeding.
func TestImportWorkspaceFailureLeavesNoWorkspace(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	ws, err := s.ImportWorkspace(failingImportBundle("Husk Probe", "husk-probe"), "", "")
	if err == nil {
		t.Fatalf("ImportWorkspace succeeded on a bundle with two collections sharing a slug; got workspace %+v", ws)
	}
	if !strings.Contains(err.Error(), "import collection") {
		t.Fatalf("bundle failed for the wrong reason — want an `import collection` failure, got: %v", err)
	}

	got, gerr := s.GetWorkspaceBySlug("husk-probe")
	if gerr != nil {
		t.Fatalf("GetWorkspaceBySlug: %v", gerr)
	}
	if got != nil {
		t.Fatalf("failed import left a workspace behind: slug=%q id=%s", got.Slug, got.ID)
	}
}

// TestImportWorkspaceRetryKeepsOriginalSlug is the second leg, and the
// user-visible one: after a failed import, fixing the bundle and retrying
// lands on the ORIGINAL slug.
//
// On the unfixed build the husk from the first attempt still holds
// `retry-slug`, so uniqueWorkspaceSlug hands the successful import
// `retry-slug-2` — a degraded slug in every URL, caused by an attempt that
// stored nothing.
func TestImportWorkspaceRetryKeepsOriginalSlug(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	if _, err := s.ImportWorkspace(failingImportBundle("Retry Probe", "retry-slug"), "", ""); err == nil {
		t.Fatal("ImportWorkspace succeeded on the deliberately-broken bundle; the retry leg proves nothing")
	}

	ws, err := s.ImportWorkspace(validImportBundle("Retry Probe", "retry-slug"), "", "")
	if err != nil {
		t.Fatalf("retry with the corrected bundle failed: %v", err)
	}
	if ws.Slug != "retry-slug" {
		t.Fatalf("retry landed on a degraded slug: got %q, want %q — the failed attempt is still holding the original", ws.Slug, "retry-slug")
	}
}
