package mcp

import (
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

// TestCatalogTools_AnnotationsExplicit pins the annotation block every
// catalog tool advertises (BUG-2302). Before this, no tool set
// annotations, so mcp-go's NewTool defaults rode out on the wire:
// ReadOnlyHint:false + DestructiveHint:true + OpenWorldHint:true on
// EVERY tool, including pure reads like pad_search.
//
// The expectation table is a deliberate SECOND enumeration, not a call
// to annotationForDef — re-deriving here would make the test tautologous
// (green no matter what the derivation says; CONVE-12). Instead each
// tool's fully-read-only status is written down literally, so:
//
//   - a broken derivation fails against the literal values;
//   - a new catalog tool fails loudly until someone adds a row with a
//     deliberate annotation decision;
//   - a write action added to a currently-all-read tool (e.g.
//     pad_project) flips its derived annotations and fails here, which
//     is the wanted alarm — that change IS a contract change.
func TestCatalogTools_AnnotationsExplicit(t *testing.T) {
	// Three classes, one deliberate row per tool:
	//   read      — every action read-only.
	//   additive  — has writes, but all purely additive (create/attach).
	//   destructive — at least one action can overwrite or delete.
	const (
		read        = "read"
		additive    = "additive"
		destructive = "destructive"
	)
	table := map[string]string{
		"pad_search":     read,
		"pad_project":    read,
		"pad_attachment": read,
		"pad_meta":       read,
		// pad_playbook.run is side-effect-free by design (returns the
		// body + bound args for the agent to execute) — all three
		// actions are reads.
		"pad_playbook": read,

		// Writes are invite/create/claim/restore — all additive
		// (restore is documented "mutating but non-destructive").
		"pad_workspace": additive,
		// activate copies a library entry in as a new item.
		"pad_library": additive,

		// update/delete (and friends) can overwrite or remove.
		"pad_item":       destructive,
		"pad_collection": destructive,
		"pad_role":       destructive,
	}

	seen := map[string]bool{}
	for _, def := range Catalog {
		seen[def.Name] = true
		class, ok := table[def.Name]
		if !ok {
			t.Errorf("tool %q has no row in this test's annotation table — add one with a deliberate read/additive/destructive decision", def.Name)
			continue
		}
		tool := buildToolFromDef(def)
		assertAnnotation(t, def.Name, tool.Annotations, annotationExpectation{
			readOnly:    class == read,
			destructive: class == destructive,
			idempotent:  class == read,
			openWorld:   false,
		})
	}
	for name := range table {
		if !seen[name] {
			t.Errorf("annotation table lists tool %q not present in Catalog (stale row)", name)
		}
	}
}

// TestSetWorkspaceTool_AnnotationsExplicit pins pad_set_workspace's
// hand-written annotation block (BUG-2302): a session-state write —
// not read-only — but non-destructive, idempotent (same slug twice =
// same state), and closed-world. Both deployment variants (persisting
// local and non-persisting shared) advertise the same conservative
// block.
func TestSetWorkspaceTool_AnnotationsExplicit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state *WorkspaceState
	}{
		{"local", NewWorkspaceState("")},
		{"shared", NewSharedWorkspaceState()},
	} {
		tool, _ := SetWorkspaceTool(tc.state, nil)
		assertAnnotation(t, "pad_set_workspace("+tc.name+")", tool.Annotations, annotationExpectation{
			readOnly:    false,
			destructive: false,
			idempotent:  true,
			openWorld:   false,
		})
	}
}

// TestAdditiveWriteActions_NoStaleEntries mirrors
// TestReadOnlyActions_NoStaleEntries for the additive-write allowlist:
// every entry must name a real catalog (tool, action) pair, and must
// NOT also be listed read-only — an additive WRITE that is also a READ
// is a contradiction, and the pair silently changing class is exactly
// the drift this guards.
func TestAdditiveWriteActions_NoStaleEntries(t *testing.T) {
	byName := map[string]ToolDef{}
	for _, def := range Catalog {
		byName[def.Name] = def
	}
	for tool, set := range additiveWriteActions {
		def, ok := byName[tool]
		if !ok {
			t.Errorf("additiveWriteActions has tool %q not in Catalog (stale entry)", tool)
			continue
		}
		for action, v := range set {
			if !v {
				t.Errorf("additiveWriteActions[%s][%s] is false — the allowlist should only list additive writes (omit the rest)", tool, action)
			}
			if _, ok := def.Actions[action]; !ok {
				t.Errorf("additiveWriteActions[%s][%s] has no matching Catalog action (stale entry)", tool, action)
			}
			if isReadOnlyAction(tool, action) {
				t.Errorf("additiveWriteActions[%s][%s] is also in readOnlyActions — an action can't be both a read and an additive write", tool, action)
			}
		}
	}
}

type annotationExpectation struct {
	readOnly, destructive, idempotent, openWorld bool
}

// assertAnnotation checks all four hint pointers are explicitly set
// (nil would mean "no claim", which is exactly the pre-fix defect
// shape: the absence that lets library defaults win) and carry the
// expected values.
func assertAnnotation(t *testing.T, name string, ann mcp.ToolAnnotation, want annotationExpectation) {
	t.Helper()
	checks := []struct {
		field string
		got   *bool
		want  bool
	}{
		{"ReadOnlyHint", ann.ReadOnlyHint, want.readOnly},
		{"DestructiveHint", ann.DestructiveHint, want.destructive},
		{"IdempotentHint", ann.IdempotentHint, want.idempotent},
		{"OpenWorldHint", ann.OpenWorldHint, want.openWorld},
	}
	for _, c := range checks {
		if c.got == nil {
			t.Errorf("%s: %s is nil — annotations must be explicit, not inherited", name, c.field)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s: %s = %v, want %v", name, c.field, *c.got, c.want)
		}
	}
}
