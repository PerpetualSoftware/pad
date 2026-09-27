package collab

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// frozenSchemaVersion is the schema version BUG-3244 froze. A mismatched join
// empties the item's op-log (maybeRebuildOnSchemaMismatch). Since BUG-3244 the
// unflushed content-bearing rows are SET ASIDE rather than deleted and the item
// stays marked superseded_set_aside, so a bump no longer loses them silently.
// But nothing yet turns set-aside rows back into text: until TASK-3246 ships a
// decoder for the outgoing era, every item with unflushed edits at the moment
// of a bump is left with edits a user can read only as raw updates or discard.
// Moving this value is the act the guard exists to make visible, and it needs
// a ruling on the BUG-3244 / TASK-3246 trail first.
const frozenSchemaVersion = "1"

func TestSchemaVersionFrozenUntilSetAsideEditsAreRecoverable(t *testing.T) {
	if DefaultSchemaVersion != frozenSchemaVersion {
		t.Fatalf("DefaultSchemaVersion is %q, frozen at %q. A schema bump sets every "+
			"item's unflushed collaborative edits aside on its next open (BUG-3244): they "+
			"are kept and the item reads superseded_set_aside, but no editor can restore "+
			"them. Before a bump, TASK-3246 must recover the outgoing era's set-aside edits "+
			"(a decoder for the removed or renamed nodes and marks, or a measurement showing "+
			"that the new schema decodes the old era as it is), and the lead must rule the "+
			"freeze lifted on that trail. Update frozenSchemaVersion only in that change.",
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
