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
			"stale body as current (BUG-3244). Before a bump, maybeRebuildOnSchemaMismatch "+
			"must set the content-bearing rows above the flush watermark aside instead of "+
			"deleting them, and keep the item marked stale until they are recovered or "+
			"discarded; closing BUG-3244 without that change does not make a bump safe. "+
			"A bump that removes or renames a node or mark also needs TASK-3246 (a decoder "+
			"that recovers the outgoing era's edits). Update frozenSchemaVersion only in "+
			"that change.",
			DefaultSchemaVersion, frozenSchemaVersion)
	}
}

// The server refuses a WS upgrade whose announced version differs from its
// own, so a web bundle and a binary that disagree cannot open any editor.
// The two constants are documented as moving in lockstep; this makes it so.
//
// A declaration is a line that STARTS with the export, so a comment quoting
// it (" * export const ...", "// export const ...") never counts. Every such
// line must also be a plain string literal; any other form fails rather
// than being skipped, so an unrecognised declaration cannot leave a stale
// match standing in for it.
var (
	tsSchemaVersionDecl    = regexp.MustCompile(`(?m)^export\s+const\s+SCHEMA_VERSION\b.*$`)
	tsSchemaVersionLiteral = regexp.MustCompile("^export\\s+const\\s+SCHEMA_VERSION\\s*=\\s*(['\"`])([^'\"`]*)(['\"`])\\s*;?\\s*$")
)

func TestSchemaVersionMatchesWebClient(t *testing.T) {
	path := filepath.Join("..", "..", "web", "src", "lib", "collab", "schemaVersion.ts")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	decls := tsSchemaVersionDecl.FindAll(src, -1)
	if len(decls) != 1 {
		t.Fatalf("%s: want exactly one line declaring SCHEMA_VERSION, found %d", path, len(decls))
	}
	m := tsSchemaVersionLiteral.FindSubmatch(decls[0])
	if m == nil || string(m[1]) != string(m[3]) {
		t.Fatalf("%s: SCHEMA_VERSION is not declared as a plain string literal: %q", path, decls[0])
	}
	if got := string(m[2]); got != DefaultSchemaVersion {
		t.Fatalf("web SCHEMA_VERSION is %q, server DefaultSchemaVersion is %q: every "+
			"collab upgrade would be refused 400", got, DefaultSchemaVersion)
	}
}
