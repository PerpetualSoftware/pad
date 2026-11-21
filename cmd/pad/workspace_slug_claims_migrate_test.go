package main

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

func TestMigrateWorkspaceSlugClaimsExcludesDeletedWorkspaces(t *testing.T) {
	dst := storetest.NewPostgres(t)
	var dbName string
	if err := dst.DB().QueryRow(`SELECT current_database()`).Scan(&dbName); err != nil {
		t.Fatal(err)
	}
	destination, err := url.Parse(os.Getenv("PAD_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	destination.Path = "/" + dbName
	setTempHomeMain(t)
	t.Chdir(t.TempDir())
	sourcePath := filepath.Join(t.TempDir(), "source.db")
	src, err := store.New(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { src.Close() })
	for _, name := range []string{"live", "deleted", "deleted-renamed"} {
		ws, err := src.CreateWorkspace(models.WorkspaceCreate{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if name != "deleted" {
			ws, _, err = src.RenameWorkspace(ws.ID, ws.Slug, name+"-new", nil)
			if err != nil {
				t.Fatal(err)
			}
		}
		if name != "live" {
			if err := src.DeleteWorkspace(ws.Slug); err != nil {
				t.Fatal(err)
			}
		}
	}
	cmd := dbMigrateToPgCmd()
	cmd.SetArgs([]string{"--from", sourcePath, "--to", destination.String(), "--force"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	claims, err := dst.WorkspaceSlugClaims()
	if err != nil || len(claims) != 2 {
		t.Fatalf("destination claims: %v %v", claims, err)
	}
	for _, slug := range []string{"live", "live-new"} {
		ws, err := dst.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil || claims[slug] != ws.ID {
			t.Fatalf("migrated alias %q: %+v %v", slug, ws, err)
		}
	}
	for _, slug := range []string{"deleted", "deleted-renamed", "deleted-renamed-new"} {
		ws, err := dst.CreateWorkspace(models.WorkspaceCreate{Name: "Reused", Slug: slug})
		if err != nil || ws.Slug != slug {
			t.Fatalf("reuse omitted slug %q: %+v %v", slug, ws, err)
		}
	}
}
