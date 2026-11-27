package store

import (
	"database/sql"
	"os"
	"sync"
	"testing"
)

const (
	bug3754ProbeName = "999_bug3754_probe.sql"
	bug3754ProbeBody = `CREATE TABLE bug3754_probe (x INTEGER);`
)

// BUG-3754: two migration runners that read appliedMigrations before either
// commits must not fail when the second reaches a migration the first already
// applied. applySQLiteMigration must recheck schema_migrations inside its
// BEGIN IMMEDIATE transaction and skip an already-applied version.
func TestBUG3754_SQLiteMigrationRechecksAppliedInsideTx(t *testing.T) {
	t.Parallel()
	s := testStoreSQLite(t)

	if err := applySQLiteMigration(s.db, bug3754ProbeName, bug3754ProbeBody); err != nil {
		t.Fatalf("runner A applySQLiteMigration: %v", err)
	}
	if err := applySQLiteMigration(s.db, bug3754ProbeName, bug3754ProbeBody); err != nil {
		t.Fatalf("runner B applySQLiteMigration (stale applied set): %v", err)
	}

	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, bug3754ProbeName).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Fatalf("schema_migrations rows for %s = %d, want 1", bug3754ProbeName, rows)
	}

	var tableRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bug3754_probe`).Scan(&tableRows); err != nil {
		t.Fatalf("bug3754_probe table not present: %v", err)
	}
}

func TestBUG3754_SQLiteConcurrentRunnersBothSucceed(t *testing.T) {
	t.Parallel()
	s := testStoreSQLite(t)
	s2, err := New(s.dbPath)
	if err != nil {
		t.Fatalf("open second store on %s: %v", s.dbPath, err)
	}
	t.Cleanup(func() { s2.Close() })

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var errs [2]error
	dbs := [2]*sql.DB{s.db, s2.db}

	for i := range dbs {
		wg.Add(1)
		go func(idx int, db *sql.DB) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			errs[idx] = applySQLiteMigration(db, bug3754ProbeName, bug3754ProbeBody)
		}(i, dbs[i])
	}

	<-ready
	<-ready
	close(start)
	wg.Wait()

	for i, runErr := range errs {
		if runErr != nil {
			t.Errorf("concurrent runner %d failed: %v", i, runErr)
		}
	}

	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, bug3754ProbeName).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Fatalf("schema_migrations rows for %s = %d, want 1", bug3754ProbeName, rows)
	}

	var tableRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bug3754_probe`).Scan(&tableRows); err != nil {
		t.Fatalf("bug3754_probe table not present: %v", err)
	}
}

func TestBUG3754_PGMigrationRechecksAppliedInsideTx(t *testing.T) {
	t.Parallel()
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, pgURL)

	if err := applyPostgresMigration(s.db, bug3754ProbeName, bug3754ProbeBody); err != nil {
		t.Fatalf("runner A applyPostgresMigration: %v", err)
	}
	if err := applyPostgresMigration(s.db, bug3754ProbeName, bug3754ProbeBody); err != nil {
		t.Fatalf("runner B applyPostgresMigration (stale applied set): %v", err)
	}

	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, bug3754ProbeName).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Fatalf("schema_migrations rows for %s = %d, want 1", bug3754ProbeName, rows)
	}

	var tableRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bug3754_probe`).Scan(&tableRows); err != nil {
		t.Fatalf("bug3754_probe table not present: %v", err)
	}
}

func TestBUG3754_PGConcurrentRunnersBothSucceed(t *testing.T) {
	t.Parallel()
	pgURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set")
	}
	s := testStorePostgres(t, pgURL)

	const body = `SELECT pg_sleep(0.3); CREATE TABLE bug3754_probe (x INTEGER);`

	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	var errs [2]error

	for i := range errs {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ready <- struct{}{}
			<-start
			errs[idx] = applyPostgresMigration(s.db, bug3754ProbeName, body)
		}(i)
	}

	<-ready
	<-ready
	close(start)
	wg.Wait()

	for i, runErr := range errs {
		if runErr != nil {
			t.Errorf("concurrent runner %d failed: %v", i, runErr)
		}
	}

	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, bug3754ProbeName).Scan(&rows); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if rows != 1 {
		t.Fatalf("schema_migrations rows for %s = %d, want 1", bug3754ProbeName, rows)
	}

	var tableRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM bug3754_probe`).Scan(&tableRows); err != nil {
		t.Fatalf("bug3754_probe table not present: %v", err)
	}
}
