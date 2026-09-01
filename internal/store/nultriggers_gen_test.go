package store

import (
	"os"
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
