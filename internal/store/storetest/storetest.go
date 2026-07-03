// Package storetest provides a fast, fully-migrated SQLite *store.Store
// fixture for tests outside internal/store.
//
// It exists to fix BUG-1913 / IDEA-1914: every store-backed test used to
// pay the full migration chain (69 SQLite migrations + 3 backfills, ~2.7s
// under -race) via store.New, and internal/server's suite alone summed
// that cost across ~600 tests to roughly 30 minutes wall clock.
//
// NewSQLite runs that migration chain at most ONCE per test binary (a
// sync.Once-guarded template database), then hands every caller a plain
// file copy of the template, opened via store.New like any other test
// store. The copy's own migrate() call is a fast no-op — schema_migrations
// is already populated (store.go:272) — rather than skipped outright, so
// no production code changes were needed to support this.
//
// internal/store's OWN tests cannot import this package: storetest imports
// store, so store's white-box tests (package store) importing storetest
// would form an import cycle that Go rejects ("import cycle not allowed
// in test" — verified empirically, not assumed). internal/store/store_test.go
// therefore carries a deliberately duplicated, minimal copy of this same
// template+copy logic (testStoreSQLite/buildSQLiteTemplate/copyTemplateFile).
// KEEP THE TWO IN SYNC — this file and internal/store/store_test.go both
// carry a cross-reference comment to that effect.
package storetest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/store"
)

var (
	templateOnce sync.Once
	templatePath string
	templateErr  error
	buildCount   int32 // atomic; test-only instrumentation, see storetest_test.go
)

// NewSQLite returns a *store.Store backed by a fresh SQLite file in
// t.TempDir(), fully migrated. See the package doc for the template+copy
// mechanism. Store.Close is registered via t.Cleanup.
func NewSQLite(t *testing.T) *store.Store {
	t.Helper()
	s := newFromTemplate(t)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// NewSQLiteUnmanaged behaves like NewSQLite but does NOT register a
// t.Cleanup to close the store — the caller owns Store.Close() and must
// call it explicitly. Use this only when a test needs precise manual
// control over close/cleanup ordering that NewSQLite's automatic
// t.Cleanup would get in the way of (e.g. a goroutine-leak assertion
// around repeated construct/Stop/Close cycles within a single test).
func NewSQLiteUnmanaged(t *testing.T) *store.Store {
	t.Helper()
	return newFromTemplate(t)
}

// newFromTemplate builds (once) and copies the template DB, then opens
// the copy via store.New. Shared by NewSQLite and NewSQLiteUnmanaged;
// the only difference between the two is who owns Store.Close().
func newFromTemplate(t *testing.T) *store.Store {
	t.Helper()

	templateOnce.Do(func() {
		templatePath, templateErr = buildTemplate()
	})
	if templateErr != nil {
		t.Fatalf("storetest: build template db: %v", templateErr)
	}

	dst := filepath.Join(t.TempDir(), "test.db")
	if err := copyFile(templatePath, dst); err != nil {
		t.Fatalf("storetest: copy template db: %v", err)
	}

	s, err := store.New(dst)
	if err != nil {
		t.Fatalf("storetest: open copied db: %v", err)
	}
	return s
}

// Cleanup removes the process-wide template database, if one was built.
// Call it from TestMain after m.Run() so a lazily-built template file
// doesn't linger past the test binary's lifetime.
func Cleanup() {
	if templatePath != "" {
		_ = os.RemoveAll(filepath.Dir(templatePath))
	}
}

// buildTemplate runs the full migration chain once into a process-wide
// temp file and returns its path. The template is checkpointed and left
// in DELETE journal mode (no -wal/-shm sidecars), so NewSQLite's copy is
// a single-file operation with no sidecar bookkeeping to get wrong.
func buildTemplate() (string, error) {
	atomic.AddInt32(&buildCount, 1)

	dir, err := os.MkdirTemp("", "pad-storetest-template-*")
	if err != nil {
		return "", fmt.Errorf("mkdir template dir: %w", err)
	}
	path := filepath.Join(dir, "template.db")

	s, err := store.New(path)
	if err != nil {
		return "", fmt.Errorf("build template store: %w", err)
	}
	defer s.Close()

	if _, err := s.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return "", fmt.Errorf("checkpoint template: %w", err)
	}
	if _, err := s.DB().Exec("PRAGMA journal_mode=DELETE"); err != nil {
		return "", fmt.Errorf("set template journal_mode=DELETE: %w", err)
	}
	return path, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, err)
	}
	return out.Close()
}
