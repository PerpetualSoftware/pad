package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestWorkspaceSourcePersisted covers migration 069 / BUG-1557: the
// workspaces.source column round-trips through CreateWorkspace and every
// read that scans a models.Workspace. A missed scan site would surface here
// (or as a "sql: expected N destination arguments" failure) rather than at
// runtime.
func TestWorkspaceSourcePersisted(t *testing.T) {
	s := testStore(t)

	cli, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Agent WS", Source: "cli"})
	if err != nil {
		t.Fatalf("create cli workspace: %v", err)
	}
	if cli.Source != "cli" {
		t.Fatalf("CreateWorkspace return: expected source=cli, got %q", cli.Source)
	}

	// CreateWorkspace returns via GetWorkspaceBySlug, but assert the other
	// single-row read (by ID) round-trips it too.
	byID, err := s.GetWorkspaceByID(cli.ID)
	if err != nil || byID == nil {
		t.Fatalf("get by id: %v (nil=%v)", err, byID == nil)
	}
	if byID.Source != "cli" {
		t.Fatalf("GetWorkspaceByID: expected source=cli, got %q", byID.Source)
	}

	// A workspace created without an explicit source (the common
	// direct-store path — cloud auto-create, import, test helpers) stays
	// "" so it is never treated as agent-created.
	web := createTestWorkspace(t, s, "Web WS")
	if web.Source != "" {
		t.Fatalf("default CreateWorkspace: expected empty source, got %q", web.Source)
	}

	// ListWorkspaces (cross-tenant) round-trips source for every row.
	all, err := s.ListWorkspaces()
	if err != nil {
		t.Fatalf("list workspaces: %v", err)
	}
	got := map[string]string{}
	for _, w := range all {
		got[w.ID] = w.Source
	}
	if got[cli.ID] != "cli" {
		t.Fatalf("ListWorkspaces: expected cli workspace source=cli, got %q", got[cli.ID])
	}
	if got[web.ID] != "" {
		t.Fatalf("ListWorkspaces: expected web workspace source=\"\", got %q", got[web.ID])
	}
}
