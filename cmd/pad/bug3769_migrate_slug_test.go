package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3769: every import door canonicalizes a workspace slug, but
// `pad db migrate-to-pg` copies EXISTING workspaces, whose slugs are already
// in URLs, bookmarks and .pad.toml links. Its import keeps the slug verbatim,
// legacy ones (an underscore, capitals) included.
func TestBUG3769_MigrateKeepsALegacyWorkspaceSlug(t *testing.T) {
	src := storetest.NewSQLite(t)
	ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: "Legacy 3769"})
	if err != nil {
		t.Fatal(err)
	}
	data, err := src.ExportWorkspace(ws.Slug)
	if err != nil {
		t.Fatal(err)
	}
	// A slug minted before BUG-3769, by `pad workspace create --slug` or an
	// import's ?name=.
	data.Workspace.Slug = "My_Legacy_WS"

	dst := storetest.NewSQLite(t)
	got, err := migrateImportWorkspace(dst, data)
	if err != nil {
		t.Fatalf("migrate import: %v", err)
	}
	if got.Slug != "My_Legacy_WS" {
		t.Errorf("migrated slug = %q, want the source's My_Legacy_WS kept verbatim", got.Slug)
	}
	// The ordinary import door still canonicalizes the same bundle.
	if imp, err := dst.ImportWorkspace(data, "", "", ""); err != nil || imp.Slug != "my-legacy-ws" {
		t.Errorf("ordinary import = %+v, %v; want slug my-legacy-ws", imp, err)
	}
}
