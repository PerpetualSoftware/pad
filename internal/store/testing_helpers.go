package store

import (
	"fmt"
	"strings"
)

// SetBcryptCostForTesting overrides the bcrypt cost used by CreateUser
// and UpdateUser for the lifetime of a test binary. It returns a
// restore function — call it from TestMain (or defer it) to leave
// the package state clean, though process exit also suffices since
// the override is local to the test binary.
//
// Why this exists: under the race detector, bcrypt.GenerateFromPassword
// at the production cost (12) takes ~3s per call. The internal/server
// and internal/store test suites bootstrap dozens of users each; the
// cumulative cost exceeded the 30m CI -race timeout (BUG-1371). Tests
// that don't care about hash strength call this once per binary in
// TestMain to drop the cost to bcrypt.MinCost (= 4), which restores
// the race step to well under the timeout.
//
// Production code MUST NOT call this. The "ForTesting" suffix is the
// grep signal — any non-test caller is a bug.
func SetBcryptCostForTesting(cost int) func() {
	prev := bcryptCost
	bcryptCost = cost
	return func() { bcryptCost = prev }
}

// SuspendTraitUniquenessForTesting drops TASK-2710's partial unique indexes
// and returns a restore function that recreates them. Defer the restore, so a
// suspended constraint cannot outlive the test that suspended it.
//
// Why this exists: those indexes make "two collections in one workspace
// declaring the same trait" unrepresentable, and several tests need exactly
// that state — not because it is supported, but because it exists in LEGACY
// databases and the code's handling of it is what they verify. The round-2
// shadowing tests in internal/server are the clearest case: they prove
// resolution returns the collection the caller can SEE rather than one hidden
// collection that happens to sort first, and that guarantee still stands
// between a legacy database and a wrong answer in the window before
// dedupeTraitDeclarations repairs it at startup — or on a deployment where an
// operator has dropped the index. Deleting those tests to satisfy the
// constraint would retire a live guarantee; building their fixture the way the
// state actually occurs keeps them testing what they always tested.
//
// THE RESTORE CAN FAIL, and that is information rather than a nuisance:
// recreating a unique index while a duplicate is still live is exactly the
// failure the migration would hit, so a test that de-duplicates first will see
// it succeed and a test that deliberately leaves the duplicate in place will
// see it fail. The restore therefore reports its error instead of swallowing
// it, and callers that intend to leave a duplicate behind say so by ignoring
// it explicitly.
//
// Production code MUST NOT call this. The "ForTesting" suffix is the grep
// signal — any non-test caller is a bug.
func (s *Store) SuspendTraitUniquenessForTesting() (restore func() error) {
	indexes := []string{
		"idx_collections_artifact_kind_per_workspace",
		"idx_collections_invocation_field_per_workspace",
	}
	for _, name := range indexes {
		if _, err := s.db.Exec(`DROP INDEX IF EXISTS ` + name); err != nil {
			return func() error { return fmt.Errorf("suspend %s: %w", name, err) }
		}
	}
	return func() error {
		body, rerr := traitUniquenessIndexDDL(s.dialect.Driver())
		if rerr != nil {
			return rerr
		}
		for _, stmt := range body {
			if _, err := s.db.Exec(stmt); err != nil {
				return fmt.Errorf("restore trait uniqueness: %w", err)
			}
		}
		return nil
	}
}

// traitUniquenessIndexDDL returns the CREATE statements for TASK-2710's
// indexes, read from the SHIPPED migration rather than retyped — a restore
// that recreated a hand-copied approximation would let the two drift and would
// then be attesting to an index the product does not have.
func traitUniquenessIndexDDL(driver DriverType) ([]string, error) {
	var path string
	if driver == DriverPostgres {
		path = "pgmigrations/064_collection_trait_uniqueness.sql"
	} else {
		path = "migrations/087_collection_trait_uniqueness.sql"
	}
	var raw []byte
	var err error
	if driver == DriverPostgres {
		raw, err = pgMigrationsFS.ReadFile(path)
	} else {
		raw, err = migrationsFS.ReadFile(path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var out []string
	for _, stmt := range strings.Split(string(raw), ";") {
		trimmed := strings.TrimSpace(stmt)
		if strings.Contains(strings.ToUpper(trimmed), "CREATE UNIQUE INDEX") {
			out = append(out, trimmed)
		}
	}
	if len(out) != 2 {
		return nil, fmt.Errorf("expected 2 CREATE UNIQUE INDEX statements in %s, found %d", path, len(out))
	}
	return out, nil
}
