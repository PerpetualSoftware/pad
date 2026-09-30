package store

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3307. createWorkspaceQ probes for a free slug and then INSERTs; two
// creates of one name that both probed before either inserted raced, and the
// loser's unique violation surfaced as a 500. The legs that lose the race on
// purpose do it through workspaceSlugProbedHook, which commits a competing
// workspace with the probed slug between the probe and the INSERT. They are
// NOT parallel: the hook is a package variable, and Go runs parallel tests
// only after every sequential one has finished.

// competeOnce arms the hook to create one competing workspace with the probed
// slug, the first time a create probes wantSlug.
func competeOnce(t *testing.T, s *Store, wantSlug string) *bool {
	t.Helper()
	fired := false
	workspaceSlugProbedHook = func(slug string) {
		if fired || slug != wantSlug {
			return
		}
		fired = true
		if _, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Rival", Slug: slug}); err != nil {
			t.Errorf("competing create: %v", err)
		}
	}
	t.Cleanup(func() { workspaceSlugProbedHook = nil })
	return &fired
}

func TestCreateWorkspace_LosingTheSlugRacePicksTheNextSlug(t *testing.T) {
	s := testStore(t)
	fired := competeOnce(t, s, "race-x")

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Race X"})
	if !*fired {
		t.Fatal("precondition: the competing create never ran, so the race was not lost")
	}
	if err != nil {
		t.Fatalf("the create that lost the race failed: %v", err)
	}
	if ws.Slug != "race-x-2" {
		t.Fatalf("slug = %q, want race-x-2", ws.Slug)
	}
}

// The plan-limit door mints inside its own transaction. On SQLite that
// transaction is BEGIN IMMEDIATE, so writers serialize and no race can open
// there (the hook's competing write would wait on the lock); the leg runs
// where it can race, Postgres.
func TestCreateWorkspace_PlanLimitTx_LosingTheSlugRacePicksTheNextSlug(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("SQLite transactions are BEGIN IMMEDIATE: the race cannot open inside one")
	}
	owner, err := s.CreateUser(models.UserCreate{Email: "bug3307-plan@test.com", Name: "Owner", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	fired := competeOnce(t, s, "race-plan")

	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Race Plan", OwnerID: owner.ID}, WithPlanLimit())
	if !*fired {
		t.Fatal("precondition: the competing create never ran")
	}
	if err != nil {
		t.Fatalf("the create that lost the race failed: %v", err)
	}
	if ws.Slug != "race-plan-2" {
		t.Fatalf("slug = %q, want race-plan-2", ws.Slug)
	}
}

// Import mints inside the transaction that carries the collections and items
// (BUG-2892). On Postgres a failed statement aborts that transaction, which
// is why the losing INSERT must not be a failed statement.
func TestImportWorkspace_LosingTheSlugRacePicksTheNextSlug(t *testing.T) {
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("SQLite transactions are BEGIN IMMEDIATE: the race cannot open inside one")
	}
	fired := competeOnce(t, s, "race-import")

	ws, err := s.ImportWorkspace(&models.WorkspaceExport{
		Version:   1,
		Workspace: models.WorkspaceExportMeta{Name: "Race Import", Slug: "race-import"},
	}, "", "", "")
	if !*fired {
		t.Fatal("precondition: the competing create never ran")
	}
	if err != nil {
		t.Fatalf("the import that lost the race failed: %v", err)
	}
	if ws.Slug != "race-import-2" {
		t.Fatalf("slug = %q, want race-import-2", ws.Slug)
	}
}

// Deterministic, no race: a soft-deleted workspace keeps its slug and the
// slug column is globally UNIQUE, so a new workspace of the same name must
// not be handed that slug.
func TestCreateWorkspace_ReusingASoftDeletedName(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	old, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Gone Soon"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.DeleteWorkspace(old.Slug); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	again, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Gone Soon"})
	if err != nil {
		t.Fatalf("create with a soft-deleted workspace's name: %v", err)
	}
	if again.Slug == old.Slug {
		t.Fatalf("new workspace took the soft-deleted one's slug %q", old.Slug)
	}
	// The soft-deleted one must still be restorable under its own slug.
	if err := s.RestoreWorkspace(old.Slug); err != nil {
		t.Fatalf("restore the soft-deleted workspace: %v", err)
	}
}

// Secondary instrument: real concurrency, no seam. It can pass by luck on a
// tree with the race, so the seam legs above are the evidence; this one says
// the fix holds when the interleaving is the scheduler's.
func TestCreateWorkspace_ConcurrentSameNameAllSucceed(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const n = 8
	var wg sync.WaitGroup
	slugs := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Crowd"})
			errs[i] = err
			if ws != nil {
				slugs[i] = ws.Slug
			}
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("create %d: %v", i, errs[i])
			continue
		}
		if seen[slugs[i]] {
			t.Errorf("slug %q handed out twice", slugs[i])
		}
		seen[slugs[i]] = true
	}
	if len(seen) != n && t.Failed() == false {
		t.Errorf("got %d distinct slugs, want %d: %v", len(seen), n, fmt.Sprint(slugs))
	}
}
