package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3447 (codex r5): collections.source is the only agent-activity signal
// an onboarding workspace has before its first item, and `pad db migrate` is
// this same export piped into import. It must survive the bundle; a bundle
// value the server would never write imports as ” (unknown, never agent).
func TestCollectionSourceSurvivesExportImport(t *testing.T) {
	s := testStore(t)
	src := createTestWorkspace(t, s, "Source Src")
	if _, err := s.CreateCollection(src.ID, models.CollectionCreate{Name: "Agent Made", Source: "cli"}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	export, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	found := false
	for _, c := range export.Collections {
		if c.Name == "Agent Made" {
			found = true
			if c.Source != "cli" {
				t.Errorf("exported source = %q, want cli", c.Source)
			}
		}
	}
	if !found {
		t.Fatal("the agent collection is missing from the export")
	}

	dest, err := s.ImportWorkspace(export, "Imported Source", "", "test")
	if err != nil {
		t.Fatalf("ImportWorkspace: %v", err)
	}
	has, err := s.WorkspaceHasAgentActivity(dest.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Error("the imported workspace lost its agent-created collection's provenance")
	}

	// A value the server never writes imports as unknown, never as agent.
	for i := range export.Collections {
		export.Collections[i].Source = "spoofed"
	}
	dest2, err := s.ImportWorkspace(export, "Imported Spoof", "", "test")
	if err != nil {
		t.Fatalf("ImportWorkspace (spoof): %v", err)
	}
	if has, _ := s.WorkspaceHasAgentActivity(dest2.ID, nil, nil); has {
		t.Error("an unrecognised bundle source counted as agent activity")
	}
}
