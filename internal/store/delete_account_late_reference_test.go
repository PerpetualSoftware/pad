package store

import (
	"sort"
	"strings"
	"testing"
)

// lateUserReferenceConstraints must name exactly the foreign keys to users
// that DELETE FROM users can fail on: the ones with no ON DELETE action
// (BUG-3289). A new such key that is not listed makes the retry refuse to
// cover it, and a listed name that no longer exists is dead text, so both
// directions fail here. Read from the migrated schema, never from the
// migration files, so a constraint spelled some other way is still seen.
func TestLateUserReferenceConstraints_MatchSchema(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	if s.dialect.Driver() != DriverPostgres {
		t.Skip("Postgres only: the retry never fires on SQLite")
	}
	rows, err := s.db.Query(`
		SELECT conname FROM pg_constraint
		WHERE contype = 'f' AND confrelid = 'users'::regclass AND confdeltype = 'a'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var schema []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		schema = append(schema, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for name := range lateUserReferenceConstraints {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	if strings.Join(schema, "\n") != strings.Join(listed, "\n") {
		t.Fatalf("lateUserReferenceConstraints disagrees with the schema.\nschema:\n  %s\nlisted:\n  %s",
			strings.Join(schema, "\n  "), strings.Join(listed, "\n  "))
	}
}
