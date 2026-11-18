package store

import (
	"errors"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestWorkspaceSlugClaims(t *testing.T) {
	s := testStore(t)
	w, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Claims Original"})
	if err != nil {
		t.Fatal(err)
	}
	renamed, changed, err := s.RenameWorkspace(w.ID, w.Slug, "claims-new", nil)
	if err != nil || !changed || renamed.Slug != "claims-new" {
		t.Fatalf("rename: %+v %v %v", renamed, changed, err)
	}
	for _, slug := range []string{w.Slug, renamed.Slug} {
		found, err := s.GetWorkspaceBySlug(slug)
		if err != nil || found == nil || found.ID != w.ID {
			t.Fatalf("lookup %s: %+v %v", slug, found, err)
		}
		ok, err := s.WorkspaceHasSlug(w.ID, slug)
		if err != nil || !ok {
			t.Fatalf("claim %s: %v %v", slug, ok, err)
		}
	}
	_, changed, err = s.RenameWorkspace(w.ID, w.Slug, "claims-new", nil)
	if err != nil || changed {
		t.Fatalf("retry: %v %v", changed, err)
	}
	_, _, err = s.RenameWorkspace(w.ID, w.Slug, "elsewhere", nil)
	if !errors.Is(err, ErrWorkspaceRenameStale) {
		t.Fatalf("stale: %v", err)
	}
	other, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Claims Original"})
	if err != nil || other.Slug != "claims-original-2" {
		t.Fatalf("create: %+v %v", other, err)
	}
	_, _, err = s.RenameWorkspace(other.ID, other.Slug, w.Slug, nil)
	if !errors.Is(err, ErrWorkspaceSlugConflict) {
		t.Fatalf("conflict: %v", err)
	}
	if _, err := s.db.Exec(s.q(`UPDATE workspaces SET slug = ? WHERE id = ?`), w.Slug, other.ID); err == nil {
		t.Fatal("direct update stole alias")
	}
	if _, _, err := s.RenameWorkspace(w.ID, "claims-new", w.Slug, nil); err != nil {
		t.Fatalf("restore old slug: %v", err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM workspaces WHERE id = ?`), w.ID); err != nil {
		t.Fatal(err)
	}
	ok, err := s.WorkspaceHasSlug(w.ID, w.Slug)
	if err != nil || ok {
		t.Fatalf("purged lookup: %v %v", ok, err)
	}
	after, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Claims Original"})
	if err != nil || after.Slug != "claims-original-3" {
		t.Fatalf("purged claim: %+v %v", after, err)
	}
	if _, err := s.db.Exec(s.q(`DELETE FROM workspaces WHERE id = ?`), other.ID); err != nil {
		t.Fatal(err)
	}
	reused, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Claims Original"})
	if err != nil || reused.Slug != "claims-original-2" {
		t.Fatalf("never renamed reuse: %+v %v", reused, err)
	}
}

func TestWorkspaceRenameConcurrentClaims(t *testing.T) {
	s := testStore(t)
	a, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Rename Race A"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Rename Race B"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, w := range []*models.Workspace{a, b} {
		wg.Add(1)
		go func(w *models.Workspace) {
			defer wg.Done()
			_, _, err := s.RenameWorkspace(w.ID, w.Slug, "rename-race-target", nil)
			errs <- err
		}(w)
	}
	wg.Wait()
	close(errs)
	wins, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			wins++
		} else if errors.Is(err, ErrWorkspaceSlugConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestWorkspaceSlugClaimImportAtomic(t *testing.T) {
	s := testStore(t)
	w, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Claims Import"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ImportWorkspaceSlugClaims(map[string]string{"import-old": w.ID, w.Slug: w.ID}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWorkspaceBySlug("import-old")
	if err != nil || got == nil || got.ID != w.ID {
		t.Fatalf("imported alias: %+v %v", got, err)
	}
	claims, err := s.WorkspaceSlugClaims()
	if err != nil || claims["import-old"] != w.ID {
		t.Fatalf("claims: %v %v", claims, err)
	}
	if err := s.ImportWorkspaceSlugClaims(map[string]string{w.Slug: "other", "must-rollback": "other"}); !errors.Is(err, ErrWorkspaceSlugConflict) {
		t.Fatalf("import conflict: %v", err)
	}
	claims, err = s.WorkspaceSlugClaims()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := claims["must-rollback"]; ok {
		t.Fatal("partial import committed")
	}
}
