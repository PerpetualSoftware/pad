package storetest

import (
	"os"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-2900. These mirror the SQLite template's tests, and they exist for a
// reason worth stating plainly: **a Postgres template that silently stopped
// being used would still pass every other test in the repository.** The clone
// and the per-test migration produce byte-identical databases, so no assertion
// about behaviour can tell them apart. Only the build COUNT can, which makes
// these the wiring tests for the mechanism rather than tests of the fixture.

// TestNewPostgres_TemplateBuildsOnce asserts the migration chain runs at most
// once per test binary no matter how many databases are handed out — the whole
// point of the change. pgBuildCount is monotonic and capped at 1 by
// pgTemplateOnce, so asserting it is exactly 1 after at least one NewPostgres
// call is valid whatever ran before this test in the binary.
func TestNewPostgres_TemplateBuildsOnce(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	_ = NewPostgres(t)
	_ = NewPostgres(t)
	_ = NewPostgres(t)

	if got := atomic.LoadInt32(&pgBuildCount); got != 1 {
		t.Fatalf("buildPGTemplate ran %d times total, want exactly 1 (sync.Once)", got)
	}
}

// TestNewPostgres_ClonesAreIsolatedAndFunctional proves each call returns its
// own writable, working database rather than a shared one: a real end-to-end
// write through one store must not be visible from another.
//
// This is the assertion that would fail if CREATE DATABASE ... TEMPLATE were
// ever replaced by something that handed out the template itself, which is the
// failure mode a speed optimisation invites.
func TestNewPostgres_ClonesAreIsolatedAndFunctional(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	sA := NewPostgres(t)

	u, err := sA.CreateUser(models.UserCreate{
		Email:    "pgtemplate-probe@example.com",
		Username: "pgtemplate-probe",
		Name:     "PG Template Probe",
		Password: "correcthorsebattery",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := sA.CreateWorkspace(models.WorkspaceCreate{Name: "Probe", Slug: "pgtemplate-probe", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if got, err := sA.GetWorkspaceBySlug(ws.Slug); err != nil || got == nil {
		t.Fatalf("workspace not readable back from its own clone: got=%v err=%v", got, err)
	}

	sB := NewPostgres(t)
	if leaked, err := sB.GetWorkspaceBySlug(ws.Slug); err != nil {
		t.Fatalf("query store B: %v", err)
	} else if leaked != nil {
		t.Fatal("workspace created in store A is visible in store B; clones are not isolated")
	}
}

// TestNewPostgres_DatabasesAreClonedNotMigrated is the test that actually
// pins the mechanism, and it exists because the build-once counter above does
// NOT.
//
// pgBuildCount proves the template was BUILT once. It says nothing about
// whether anything was cloned from it: revert `CREATE DATABASE ... TEMPLATE`
// to a plain `CREATE DATABASE` and the counter still reads 1, every other test
// in the repository still passes, and the entire cost this change removes
// comes back silently. That is the regression this test is for, and it was
// only findable by asking what a mutation would leave green.
//
// The discriminator is a marker stamped on the template after migrating,
// carried into a clone by the file-level copy and absent from anything
// migrated from scratch. See buildPGTemplate for why it is a table comment and
// why schema_migrations.applied_at is not good enough.
func TestNewPostgres_DatabasesAreClonedNotMigrated(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := NewPostgres(t)

	pgTemplateMu.RLock()
	want := templateMarker(pgTemplateName)
	pgTemplateMu.RUnlock()

	var got *string
	err := s.DB().QueryRow(`SELECT obj_description('schema_migrations'::regclass, 'pg_class')`).Scan(&got)
	if err != nil {
		t.Fatalf("read template marker from the handed-out database: %v", err)
	}
	if got == nil {
		t.Fatal("the database handed out carries NO template marker — it was migrated from scratch, " +
			"not cloned from the template; the per-test migration cost is back")
	}
	if *got != want {
		t.Fatalf("template marker mismatch: got %q, want %q — the database was cloned from something "+
			"other than this binary's template", *got, want)
	}
}

// TestNewPostgres_CloneCarriesTheMigratedSchema is the counterpart to the
// isolation test above, and it fails in the opposite direction: a clone that
// arrived EMPTY would also be "isolated".
//
// It asserts the schema came across with the file copy by writing through a
// table that only the late migrations create, rather than by reading
// schema_migrations — a row count there would be satisfied by a template whose
// migration bookkeeping was copied without the objects it describes.
func TestNewPostgres_CloneCarriesTheMigratedSchema(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := NewPostgres(t)

	u, err := s.CreateUser(models.UserCreate{
		Email:    "pgtemplate-schema@example.com",
		Username: "pgtemplate-schema",
		Name:     "PG Template Schema",
		Password: "correcthorsebattery",
		Role:     "admin",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Schema", Slug: "pgtemplate-schema", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	// Collections carry `traits`, added late in the chain (TASK-2657) and
	// tightened by TASK-2710's partial unique indexes. A clone missing the tail
	// of the migrations fails here rather than passing quietly.
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Probe", Prefix: "PRB"})
	if err != nil {
		t.Fatalf("create collection in clone: %v", err)
	}
	if _, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "probe item"}); err != nil {
		t.Fatalf("create item in clone: %v", err)
	}
}
