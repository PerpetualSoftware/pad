package store

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// TestGenerateNULTriggerMigration regenerates the Layer B migration from the
// shared column list.
//
// It is a TEST rather than a deleted scratch script (codex round 1): the
// migration's own header tells a reader to run it, and an artifact that
// instructs you to run something that does not exist is worse than one with no
// instructions. It is skipped unless GEN_NUL_TRIGGERS is set, so it costs
// nothing on an ordinary run.
//
// Regenerating is not a routine step. The pin test fails when the file and the
// list disagree, and the right response is usually to fix whichever is wrong —
// regenerate only after deciding the LIST is right.
func TestGenerateNULTriggerMigration(t *testing.T) {
	if os.Getenv("GEN_NUL_TRIGGERS") == "" {
		t.Skip("generator; run with GEN_NUL_TRIGGERS=1 after changing nulcolumns.go")
	}
	if err := os.WriteFile("migrations/"+nulTriggerMigration, []byte(renderNULTriggerMigration()), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote %d triggers for %d columns", len(NULProtectedColumns())*2, len(NULProtectedColumns()))
}

// renderNULTriggerMigration is the single definition of the migration's text.
//
// The generator writes it and the pin test compares against it, so "the file
// matches the list" is checked byte for byte rather than by counting trigger
// names — which is all the first version did, and would have passed a wrong
// BEFORE UPDATE OF clause, a wrong predicate, or a changed marker.
func renderNULTriggerMigration() string {
	cols := NULProtectedColumns()
	sort.Slice(cols, func(i, j int) bool {
		if cols[i].Table != cols[j].Table {
			return cols[i].Table < cols[j].Table
		}
		return cols[i].Column < cols[j].Column
	})

	var b strings.Builder
	b.WriteString(nulTriggerMigrationHeader)
	for _, c := range cols {
		for _, ev := range []struct{ suffix, on string }{
			{"ins", "INSERT"},
			{"upd", "UPDATE OF " + c.Column},
		} {
			name := fmt.Sprintf("pad_nul_%s_%s_%s", c.Table, c.Column, ev.suffix)
			cond := fmt.Sprintf("instr(NEW.%s, char(0)) > 0", c.Column)
			if c.Class == classJSON {
				cond += fmt.Sprintf(`
			OR (json_valid(NEW.%s) AND EXISTS (
				SELECT 1 FROM json_tree(NEW.%s)
				WHERE instr(value, char(0)) > 0 OR instr(key, char(0)) > 0
			))`, c.Column, c.Column)
			}
			fmt.Fprintf(&b, `CREATE TRIGGER IF NOT EXISTS %s
BEFORE %s ON %s
FOR EACH ROW WHEN NEW.%s IS NOT NULL AND (
			%s
)
BEGIN
	SELECT RAISE(ABORT, '%s: %s.%s must not contain a NUL');
END;

`, name, ev.on, c.Table, c.Column, cond, nulTriggerMarker, c.Table, c.Column)
		}
	}
	return b.String()
}
