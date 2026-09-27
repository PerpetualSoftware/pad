package collab

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// frozenSchemaVersion is the schema version BUG-3244 freezes. A mismatched
// join prunes the item's whole op-log (maybeRebuildOnSchemaMismatch), and
// every content-bearing row above the flush watermark goes with it: the
// first join after a bump deletes that item's unflushed edits and clears
// its content_state, so the stale body then reads as current. Until the
// rebuild preserves those edits, a bump is a data-loss release. Moving this
// value is the act the guard exists to make visible; do it only in the change
// that closes BUG-3244.
const frozenSchemaVersion = "1"

func TestSchemaVersionFrozenUntilRebuildPreservesUnflushedEdits(t *testing.T) {
	if DefaultSchemaVersion != frozenSchemaVersion {
		t.Fatalf("DefaultSchemaVersion is %q, frozen at %q: a schema bump prunes every "+
			"item's unflushed collaborative edits on its next open and then reports the "+
			"stale body as current (BUG-3244). A bump requires BUG-3244 closed (the "+
			"mismatch rebuild sets unflushed rows aside instead of deleting them) and, "+
			"for a bump that removes or renames a node or mark, TASK-3246 (a decoder "+
			"that recovers the outgoing era's edits). Refuse it until then.",
			DefaultSchemaVersion, frozenSchemaVersion)
	}
}

// The server refuses a WS upgrade whose announced version differs from its
// own, so a web bundle and a binary that disagree cannot open any editor.
// The two constants are documented as moving in lockstep; this makes it so.
var tsSchemaVersion = regexp.MustCompile(`export\s+const\s+SCHEMA_VERSION\s*=\s*['"]([^'"]*)['"]`)

func TestSchemaVersionMatchesWebClient(t *testing.T) {
	path := filepath.Join("..", "..", "web", "src", "lib", "collab", "schemaVersion.ts")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := tsSchemaVersion.FindAllSubmatch(src, -1)
	if len(m) != 1 {
		t.Fatalf("%s: want exactly one SCHEMA_VERSION declaration, found %d", path, len(m))
	}
	if got := string(m[0][1]); got != DefaultSchemaVersion {
		t.Fatalf("web SCHEMA_VERSION is %q, server DefaultSchemaVersion is %q: every "+
			"collab upgrade would be refused 400", got, DefaultSchemaVersion)
	}
}
