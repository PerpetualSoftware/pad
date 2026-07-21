package storetest

import (
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// NewPostgres returns a *store.Store backed by an ISOLATED PostgreSQL database
// when PAD_TEST_POSTGRES_URL is set, and t.Skip()s the test otherwise. It lets
// tests OUTSIDE the store package (e.g. internal/server) exercise Postgres-gated
// code paths under `make test-pg`, where the store-package white-box helper
// (internal/store/store_test.go::testStorePostgres) isn't importable.
//
// It mirrors that helper: a uniquely-named database is CREATEd off the base URL,
// opened via store.NewPostgres (which runs the full migration chain), and DROPped
// on cleanup. The pgx driver is already registered transitively via the store
// import. KEEP IN SYNC with store_test.go's testStorePostgres (duplicated for the
// same import-cycle reason as NewSQLite — see the package doc).
func NewPostgres(t *testing.T) *store.Store {
	t.Helper()

	baseURL := os.Getenv("PAD_TEST_POSTGRES_URL")
	if baseURL == "" {
		t.Skip("PAD_TEST_POSTGRES_URL not set; Postgres-backed test skipped")
	}

	dbName := "pad_test_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")

	admin, err := sql.Open("pgx", baseURL)
	if err != nil {
		t.Fatalf("storetest: open pg admin conn: %v", err)
	}
	// CREATE DATABASE cannot run inside a transaction.
	if _, err := admin.Exec("CREATE DATABASE " + dbName); err != nil {
		_ = admin.Close()
		t.Fatalf("storetest: create test database %s: %v", dbName, err)
	}
	_ = admin.Close()

	s, err := store.NewPostgres(replaceDBName(baseURL, dbName))
	if err != nil {
		dropPostgresDB(baseURL, dbName)
		t.Fatalf("storetest: open test postgres store: %v", err)
	}
	t.Cleanup(func() {
		_ = s.Close()
		dropPostgresDB(baseURL, dbName)
	})
	return s
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
