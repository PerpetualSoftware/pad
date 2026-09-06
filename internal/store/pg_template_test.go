package store

import (
	"os"
	"sync/atomic"
	"testing"
)

// TASK-2900. The in-package mirror of storetest/postgres_test.go's mechanism
// tests, and the reason there are two is the reason there are two helpers:
// internal/store's white-box tests cannot import storetest without an import
// cycle, so the template machinery is duplicated, and a mechanism test that
// covered only one copy would leave the copy 700 tests actually use unguarded.
//
// This file is the one that matters most by volume. `testStore(t)` routes here
// under PAD_TEST_POSTGRES_URL, and it is called from 699 sites in this package.

// TestTestStorePostgres_DatabasesAreClonedNotMigrated pins the mechanism.
//
// Nothing else can. A clone and a per-test migration produce byte-identical
// databases, so no assertion about schema or behaviour distinguishes them:
// revert `CREATE DATABASE ... TEMPLATE` to a plain `CREATE DATABASE` and all
// 902 other tests in this package still pass while the whole per-test
// migration cost silently returns. Verified by mutation against the storetest
// twin, where dropping the TEMPLATE clause failed exactly one test — this one's
// counterpart — and left the other five green.
//
// The discriminator is a marker stamped on the template after migrating, which
// a file-level clone carries and a fresh migration cannot produce.
func TestTestStorePostgres_DatabasesAreClonedNotMigrated(t *testing.T) {
	baseURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if baseURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; this test is about the Postgres fixture")
	}
	s := testStore(t)

	pgTemplateMu.RLock()
	want := pgTemplateMarker(pgTemplateName)
	pgTemplateMu.RUnlock()

	var got *string
	if err := s.db.QueryRow(`SELECT obj_description('schema_migrations'::regclass, 'pg_class')`).Scan(&got); err != nil {
		t.Fatalf("read template marker from the handed-out database: %v", err)
	}
	if got == nil {
		t.Fatal("the database handed out carries NO template marker — it was migrated from scratch " +
			"rather than cloned; the per-test migration cost this package's 699 testStore(t) call sites " +
			"used to pay is back")
	}
	if *got != want {
		t.Fatalf("template marker mismatch: got %q, want %q", *got, want)
	}
}

// TestTestStorePostgres_TemplateBuildsOnce asserts the migration chain runs at
// most once per test binary. It is NOT a substitute for the test above —
// deliberately noted, because the counter reads 1 whether or not anything was
// cloned from the template, which is exactly the trap the mutation exposed.
func TestTestStorePostgres_TemplateBuildsOnce(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	_ = testStore(t)
	_ = testStore(t)

	if got := atomic.LoadInt32(&pgTemplateBuilds); got != 1 {
		t.Fatalf("buildPostgresTemplate ran %d times, want exactly 1 (sync.Once)", got)
	}
}

// TestTestStorePostgres_ClonesAreIsolated proves each caller gets its own
// writable database rather than a shared one — the failure a template
// optimisation invites is handing out the template itself.
func TestTestStorePostgres_ClonesAreIsolated(t *testing.T) {
	if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	sA := testStore(t)
	ws := createTestWorkspace(t, sA, "Clone Isolation A")

	sB := testStore(t)
	leaked, err := sB.GetWorkspaceBySlug(ws.Slug)
	if err != nil {
		t.Fatalf("query store B: %v", err)
	}
	if leaked != nil {
		t.Fatalf("workspace %q created in store A is visible in store B; clones are not isolated", ws.Slug)
	}
}
