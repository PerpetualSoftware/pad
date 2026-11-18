package store

import (
	"database/sql"
	"errors"
	"fmt"
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

func TestWorkspaceSlugClaimsMigrationGuards(t *testing.T) {
	s := testStoreSQLite(t)
	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `INSERT INTO workspace_slug_claims (slug, workspace_id) VALUES (?, ?)`, "bad\x00slug", "old-id")
		mustExec(t, raw, `INSERT INTO workspace_slug_claims (slug, workspace_id) VALUES (?, ?)`, "bad-id", "id"+invalidByte)
	})
	report, err := s.ScanNUL()
	if err != nil {
		t.Fatal(err)
	}
	got := report.ByColumn()
	if got["workspace_slug_claims.slug"] != 1 || got["workspace_slug_claims.workspace_id"] != 1 {
		t.Fatalf("claims violations: %v", got)
	}
	if !MigratedTables()["workspace_slug_claims"] {
		t.Fatal("claims missing from migration preflight")
	}
	raw, err := sql.Open("sqlite", s.dbPath+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`INSERT INTO workspace_slug_claims (slug, workspace_id) VALUES (?, ?)`, "new\x00slug", "owner"); err == nil {
		t.Fatal("raw writer inserted NUL claim")
	}
}

func TestWorkspaceSlugClaimsRepairLegacyCanonical(t *testing.T) {
	for _, bad := range []string{"legacy\x00slug", "legacy" + invalidByte} {
		t.Run(fmt.Sprintf("%x", bad), func(t *testing.T) {
			db, _ := preMigrationDB(t, "186")
			s := &Store{db: db, dialect: &sqliteDialect{}}
			if _, err := db.Exec(`DROP TRIGGER pad_nul_workspaces_slug_ins`); err != nil {
				t.Fatal(err)
			}
			id, ts := newID(), now()
			if _, err := db.Exec(`INSERT INTO workspaces (id, name, slug, settings, created_at, updated_at) VALUES (?, 'Legacy', CAST(? AS TEXT), '{}', ?, ?)`, id, []byte(bad), ts, ts); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"186_workspace_slug_claims.sql", "187_nul_invariant_triggers_post186.sql"} {
				body, err := migrationsFS.ReadFile("migrations/" + name)
				if err != nil {
					t.Fatal(err)
				}
				if err := applySQLiteMigration(db, name, string(body)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.Exec(`INSERT INTO workspace_slug_claims (slug, workspace_id) VALUES ('valid-alias', ?)`, id); err != nil {
				t.Fatal(err)
			}
			report, err := s.RepairNUL()
			if err != nil || len(report.Failed) != 0 {
				t.Fatalf("repair: %+v %v", report, err)
			}
			scan, err := s.ScanNUL()
			if err != nil || scan.Total() != 0 {
				t.Fatalf("remaining violations: %+v %v", scan, err)
			}
			again, err := s.RepairNUL()
			if err != nil || len(again.Repaired) != 0 || len(again.Failed) != 0 || len(again.Skipped) != 0 {
				t.Fatalf("second repair: %+v %v", again, err)
			}
			ws, err := s.GetWorkspaceByID(id)
			if err != nil || ws == nil {
				t.Fatalf("workspace: %+v %v", ws, err)
			}
			for _, slug := range []string{ws.Slug, "valid-alias"} {
				owner, err := s.GetWorkspaceBySlug(slug)
				if err != nil || owner == nil || owner.ID != id {
					t.Fatalf("claim %q: %+v %v", slug, owner, err)
				}
			}
			var n int
			if err := db.QueryRow(`SELECT COUNT(*) FROM workspace_slug_claims WHERE slug = CAST(? AS TEXT)`, []byte(bad)).Scan(&n); err != nil || n != 0 {
				t.Fatalf("old claim count %d: %v", n, err)
			}
		})
	}
}

func TestWorkspaceSlugRepairDoesNotDeleteOtherOwnersClaim(t *testing.T) {
	s := testStoreSQLite(t)
	ws := createTestWorkspace(t, s, "Claim owner")
	bad := "malformed\x00slug"
	plantLegacyRows(t, s, func(raw *sql.DB) {
		mustExec(t, raw, `UPDATE workspaces SET slug = ? WHERE id = ?`, bad, ws.ID)
		mustExec(t, raw, `UPDATE workspace_slug_claims SET workspace_id = 'other-owner' WHERE slug = ?`, bad)
	})
	changed, err := s.repairOneNUL(NULViolation{Table: "workspaces", Column: "slug", Key: map[string]string{"id": ws.ID}})
	if err != nil || !changed {
		t.Fatalf("repair: %v %v", changed, err)
	}
	var owner string
	if err := s.db.QueryRow(`SELECT workspace_id FROM workspace_slug_claims WHERE slug = CAST(? AS TEXT)`, []byte(bad)).Scan(&owner); err != nil || owner != "other-owner" {
		t.Fatalf("other claim: %q %v", owner, err)
	}
}
