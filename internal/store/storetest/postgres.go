package storetest

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/store"
)

var (
	// pgTemplateMu guards pgTemplateName against Cleanup racing an in-flight
	// build or clone — the same race as templateMu, and the same remedy:
	// NewPostgres holds it for read across build + clone, Cleanup takes the
	// write lock before dropping.
	pgTemplateMu   sync.RWMutex
	pgTemplateOnce sync.Once
	pgTemplateName string
	pgTemplateErr  error

	// pgCloneMu serialises CREATE DATABASE ... TEMPLATE within this binary.
	//
	// Postgres refuses to clone a database that any other session is connected
	// to ("source database is being accessed by other users"), and a clone
	// briefly connects to the source. Serial tests would not need this; the
	// lock is here because TASK-2900 may add t.Parallel() to tests this helper
	// backs, and a lock that is uncontended today costs less than a flake that
	// only appears once tests run concurrently. The serialised section is the
	// clone itself, measured at ~36ms.
	pgCloneMu sync.Mutex

	// pgBuildCount is test-only instrumentation, mirroring buildCount for the
	// SQLite template. It exists because a template that silently stopped
	// being used would still pass every test in the suite — the clone and the
	// per-test migration produce identical databases, so correctness cannot
	// distinguish them and only a count can. See postgres_test.go.
	pgBuildCount int32
)

// NewPostgres returns a *store.Store backed by an ISOLATED PostgreSQL database
// when PAD_TEST_POSTGRES_URL is set, and t.Skip()s the test otherwise. It lets
// tests OUTSIDE the store package (e.g. internal/server) exercise Postgres-gated
// code paths under `make test-pg`, where the store-package white-box helper
// (internal/store/store_test.go::testStorePostgres) isn't importable.
//
// The database is CLONED from a per-binary template that has already been
// migrated, not migrated from scratch (TASK-2900). The pgx driver is already
// registered transitively via the store import. KEEP IN SYNC with
// store_test.go's testStorePostgres (duplicated for the same import-cycle
// reason as NewSQLite — see the package doc), which carries the same template
// machinery.
//
// Why the template: the migration chain used to run once per TEST. Measured at
// 385ms of a 420ms per-construction toll, against ~700 constructions in
// internal/store alone — which is why that leg spent four fifths of its wall
// clock waiting on postgres rather than working. In the container's own CPU
// accounting the toll was 386ms per construction against 407s for the whole
// leg. Cloning an already-migrated template is a file-level copy: 104ms of
// container CPU per clone plus 346ms once per binary.
//
// This is the Postgres analogue of the SQLite template-and-copy this package
// has had since IDEA-1914, and it is here for the same reason: the schema is
// identical for every test, so building it per test buys nothing.
func NewPostgres(t *testing.T) *store.Store {
	t.Helper()

	baseURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if baseURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres-backed test skipped")
	}

	dbName := "pad_test_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	pgTemplateMu.RLock()
	defer pgTemplateMu.RUnlock()

	pgTemplateOnce.Do(func() {
		pgTemplateName, pgTemplateErr = buildPGTemplate(baseURL)
	})
	if pgTemplateErr != nil {
		t.Fatalf("storetest: build pg template: %v", pgTemplateErr)
	}

	admin, err := sql.Open("pgx", baseURL)
	if err != nil {
		t.Fatalf("storetest: open pg admin conn: %v", err)
	}
	// CREATE DATABASE cannot run inside a transaction.
	pgCloneMu.Lock()
	_, err = admin.Exec("CREATE DATABASE " + dbName + " TEMPLATE " + pgTemplateName)
	pgCloneMu.Unlock()
	_ = admin.Close()
	if err != nil {
		t.Fatalf("storetest: clone test database %s from template %s: %v", dbName, pgTemplateName, err)
	}

	// The clone arrives already migrated, so store.NewPostgres's own migrate()
	// is a fast no-op — schema_migrations came with the copy. Deliberately NOT
	// skipped: going through the same constructor production uses is what keeps
	// the fixture honest, and it is the same choice NewSQLite makes.
	s, err := store.NewPostgres(replaceDBName(baseURL, dbName))
	if err != nil {
		dropPostgresDB(baseURL, dbName)
		t.Fatalf("storetest: open cloned test postgres store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		dropPostgresDB(baseURL, dbName)
	})
	return s
}

// buildPGTemplate creates ONE fully-migrated database per test binary and
// returns its name. Callers hold pgTemplateMu for read.
//
// The connection that runs the migrations is closed before returning, and that
// close is load-bearing rather than hygiene: CREATE DATABASE ... TEMPLATE fails
// outright while any session is still connected to the source.
//
// LEAK POSTURE: a test binary killed mid-run leaves its pad_tmpl_* database
// behind, because Cleanup never runs. That is bounded rather than unbounded —
// the Postgres container is per-worktree (TASK-2708) and `make test-pg` tears
// its stack down with -v, so an orphaned template dies with the container that
// holds it. Not worth a reaper.
func buildPGTemplate(baseURL string) (string, error) {
	atomic.AddInt32(&pgBuildCount, 1)
	name := "pad_tmpl_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	admin, err := sql.Open("pgx", baseURL)
	if err != nil {
		return "", fmt.Errorf("open pg admin conn: %w", err)
	}
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		_ = admin.Close()
		return "", fmt.Errorf("create template database %s: %w", name, err)
	}
	_ = admin.Close()

	// The one time this binary pays for the migration chain.
	s, err := store.NewPostgres(replaceDBName(baseURL, name))
	if err != nil {
		dropPostgresDB(baseURL, name)
		return "", fmt.Errorf("migrate template database %s: %w", name, err)
	}
	// Stamp the template with a marker a FRESHLY-MIGRATED database cannot
	// have. This is what makes the mechanism testable at all: a clone and a
	// per-test migration produce otherwise byte-identical databases, so no
	// assertion about schema or behaviour can tell them apart, and the
	// build-once counter only proves the template was BUILT once — not that
	// anything was cloned FROM it. A regression that quietly reverted
	// `CREATE DATABASE ... TEMPLATE` to a plain CREATE would pass every other
	// test in the repository while restoring the whole cost this change removes.
	//
	// A table comment is the carrier because it lives in the database's own
	// pg_description and therefore travels with a file-level clone, while
	// being invisible to every query the product makes and to the table
	// walkers in the NUL-scan tests, which enumerate DATA. A database comment
	// would not work: those live in the shared pg_shdescription keyed by
	// database oid, and a clone gets a new oid.
	//
	// applied_at was the obvious alternative and is NOT sufficient: it is
	// RFC3339, so second-resolution, and a fresh 385ms migration can land
	// inside the same second as the template's and produce an identical set.
	if _, err := s.DB().Exec(fmt.Sprintf("COMMENT ON TABLE schema_migrations IS '%s'", templateMarker(name))); err != nil {
		_ = s.Close()
		dropPostgresDB(baseURL, name)
		return "", fmt.Errorf("stamp template %s: %w", name, err)
	}
	if err := s.Close(); err != nil {
		dropPostgresDB(baseURL, name)
		return "", fmt.Errorf("close template connection %s: %w", name, err)
	}
	return name, nil
}

// templateMarker is the sentinel stamped on the template and expected on every
// clone. See buildPGTemplate for why a table comment carries it.
func templateMarker(templateName string) string {
	return "storetest-pg-template:" + templateName
}

// dropPGTemplate removes the per-binary template database, if one was built.
// Called from Cleanup under the write lock.
func dropPGTemplate() {
	if pgTemplateName == "" {
		return
	}
	if baseURL := os.Getenv("PAD_TEST_POSTGRES_URL"); baseURL != "" {
		dropPostgresDB(baseURL, pgTemplateName)
	}
	pgTemplateName = ""
}

func dropPostgresDB(baseURL, dbName string) {
	admin, err := sql.Open("pgx", baseURL)
	if err != nil {
		return
	}
	defer admin.Close()
	_, _ = admin.Exec("DROP DATABASE IF EXISTS " + dbName + " WITH (FORCE)")
}

// replaceDBName swaps the database name in a postgres connection URL, preserving
// any query string. Mirrors store_test.go::replaceDBName.
func replaceDBName(connStr, newDB string) string {
	query := ""
	base := connStr
	if qIdx := strings.IndexByte(connStr, '?'); qIdx >= 0 {
		query = connStr[qIdx:]
		base = connStr[:qIdx]
	}
	if lastSlash := strings.LastIndexByte(base, '/'); lastSlash >= 0 {
		return base[:lastSlash+1] + newDB + query
	}
	return connStr
}
