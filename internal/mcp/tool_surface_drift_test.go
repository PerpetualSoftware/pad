package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TASK-2005 drift guard.
//
// The MCP tool catalog (internal/mcp/catalog_*.go, authoritative) is the
// source of truth for what tools + actions exist. Three documentation
// surfaces must stay in lockstep with it or agents under-discover tools:
//
//   - internal/mcp/instructions.md — the in-binary server instructions
//     handed to EVERY MCP session. Stale here is the worst kind of drift:
//     silent, per-session, invisible.
//   - README.md — the developer-facing tool catalog table + the
//     tool_surface_version string.
//   - internal/mcp/version.go::ToolSurfaceVersion — the version constant.
//
// These tests FAIL when a catalog action is missing from the tool's own
// documented action list, or when a documented version string drifts from
// ToolSurfaceVersion.
//
// The action checks are PER-TOOL scoped (not a whole-document substring
// search): each action must appear on the line that documents ITS tool.
// That way adding, say, `restore` to pad_role without documenting it fails
// even though `restore` is already documented under pad_item/pad_workspace.
// Both docs render one line per tool — instructions.md as a "- `tool` — …"
// bullet, README as a "| `tool` | … |" table row — so the tool's own line
// is an unambiguous scope. If either layout ever goes multi-line per tool,
// these helpers need updating (and will fail loudly rather than silently
// passing).

// TestInstructionsMDCoversEveryCatalogAction asserts every tool in the live
// Catalog has a documentation bullet in instructions.md, and every one of
// its actions appears on that bullet.
func TestInstructionsMDCoversEveryCatalogAction(t *testing.T) {
	if len(Catalog) == 0 {
		t.Fatal("Catalog is empty — init() registration broke; drift guard cannot run")
	}

	for _, def := range Catalog {
		line := findToolLine(Instructions, "- `"+def.Name+"`")
		if line == "" {
			t.Errorf("tool %q is in the catalog but has no '- `%s` — …' bullet in instructions.md — document it under the '## Tool surface' section", def.Name, def.Name)
			continue
		}
		for action := range def.Actions {
			if !containsAction(line, action) {
				t.Errorf("action %q of tool %q is in the catalog but not documented on %q's instructions.md bullet — add it to the tool's action list", action, def.Name, def.Name)
			}
		}
	}
}

// TestReadmeTableCoversEveryCatalogAction asserts the README catalog table
// has a row for every tool in the live Catalog, and every one of its actions
// appears in that row.
func TestReadmeTableCoversEveryCatalogAction(t *testing.T) {
	readme := readRepoFile(t, "README.md")

	for _, def := range Catalog {
		row := findToolLine(readme, "| `"+def.Name+"` |")
		if row == "" {
			t.Errorf("tool %q is in the catalog but has no '| `%s` | … |' row in the README catalog table", def.Name, def.Name)
			continue
		}
		for action := range def.Actions {
			if !containsAction(row, action) {
				t.Errorf("action %q of tool %q is in the catalog but not in %q's README table row", action, def.Name, def.Name)
			}
		}
	}
}

// TestInstructionsMDVersionMatchesToolSurface pins the "## Tool surface (vX)"
// heading in instructions.md to the current ToolSurfaceVersion. Bump
// ToolSurfaceVersion without re-titling the heading and this fails.
func TestInstructionsMDVersionMatchesToolSurface(t *testing.T) {
	want := "## Tool surface (v" + ToolSurfaceVersion + ")"
	if !strings.Contains(Instructions, want) {
		t.Errorf("instructions.md is missing the current tool-surface heading %q — its '## Tool surface (v…)' title drifted from ToolSurfaceVersion (%q). Re-title the heading.", want, ToolSurfaceVersion)
	}
}

// TestReadmeVersionMatchesToolSurface pins README.md's version references
// (the catalog table heading and the tool_surface_version line) to the
// current ToolSurfaceVersion. Bump the constant without touching the README
// and this fails.
func TestReadmeVersionMatchesToolSurface(t *testing.T) {
	readme := readRepoFile(t, "README.md")

	catalogHeading := "**Tool catalog (v" + ToolSurfaceVersion + ")**"
	if !strings.Contains(readme, catalogHeading) {
		t.Errorf("README.md is missing the current catalog heading %q — it drifted from ToolSurfaceVersion (%q).", catalogHeading, ToolSurfaceVersion)
	}

	versionLine := `tool_surface_version: "` + ToolSurfaceVersion + `"`
	if !strings.Contains(readme, versionLine) {
		t.Errorf("README.md is missing %q — the documented tool_surface_version drifted from ToolSurfaceVersion (%q).", versionLine, ToolSurfaceVersion)
	}
}

// findToolLine returns the first line of doc whose trimmed form starts with
// prefix (the tool's documentation-line marker), or "" if none matches.
// Scoping the action search to a single line keeps one tool's actions from
// satisfying another tool's coverage check.
func findToolLine(doc, prefix string) string {
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			return line
		}
	}
	return ""
}

// containsAction reports whether a tool's documentation line references the
// given action. Actions are rendered backtick-quoted in the README table
// (`create`) and slash- or space-separated in instructions.md (create /
// update); a plain substring check within the already-tool-scoped line
// covers both layouts without coupling to either.
func containsAction(line, action string) bool {
	return strings.Contains(line, action)
}

// readRepoFile reads a file relative to the repo root. Go runs tests with the
// package directory as the working directory, so the repo root is two levels
// up from internal/mcp.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	path := filepath.Join("..", "..", rel)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("cannot read %s (%s): %v", rel, path, err)
	}
	return string(b)
}
