package mcp

import (
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// RegistryOptions configures Register.
//
// As of TASK-981 the v0.1 cmdhelp leaf walker is retired — Register
// registers pad_set_workspace plus the v0.2 catalog (catalog.go).
// cmdhelp is still required (the source of truth for individual CLI
// command schemas consumed by BuildCLIArgs at dispatch time), but no
// longer drives tool naming or count.
type RegistryOptions struct {
	// Doc is the cmdhelp Document describing the command tree.
	// Required — used by env.Dispatch to look up cmdInfo for
	// BuildCLIArgs. The tool surface itself is hand-curated in
	// catalog.go and per-resource catalog_<name>.go files.
	Doc *cmdhelp.Document

	// Workspace holds the session workspace mutated by pad_set_workspace.
	// Required.
	Workspace *WorkspaceState

	// Dispatcher executes tool calls. Required (use ExecDispatcher in
	// production; fakes in tests).
	Dispatcher Dispatcher

	// RootFlags lists root-level CLI flags to inject into every
	// dispatched call when the input doesn't already provide them.
	// Captured at server startup — runtime-mutable state belongs in
	// WorkspaceState, not here. Common case: --url for non-default
	// server endpoints (root persistent flag in cmd/pad/main.go).
	//
	// Empty values are skipped, so passing
	// `map[string]string{"url": urlFlag}` is safe even when the user
	// didn't pass --url to `pad mcp serve`.
	RootFlags map[string]string

	// PadVersion is the runtime pad binary version. Forwarded to the
	// catalog's ActionEnv so pad_meta.action: server-info / version
	// can report it. Empty falls back to FallbackVersion.
	PadVersion string
}

// Register installs pad's MCP tools on srv: the built-in
// pad_set_workspace plus every v0.2 catalog tool. Returns the count of
// registered tools (including pad_set_workspace).
//
// As of TASK-981 (PLAN-969), this is the single tool-registration
// entry point. The legacy cmdhelp leaf walker has been retired —
// Register no longer iterates opts.Doc.Commands. Doc is still required
// because the catalog's action handlers use it via ActionEnv.Dispatch
// to look up CLI flag/arg schemas at dispatch time.
func Register(srv *server.MCPServer, opts RegistryOptions) (int, error) {
	if opts.Doc == nil {
		return 0, fmt.Errorf("RegistryOptions.Doc is required")
	}
	if opts.Workspace == nil {
		return 0, fmt.Errorf("RegistryOptions.Workspace is required")
	}
	if opts.Dispatcher == nil {
		return 0, fmt.Errorf("RegistryOptions.Dispatcher is required")
	}

	registerSetWorkspaceTool(srv, opts.Workspace)
	count := 1 // pad_set_workspace

	catalogCount, err := RegisterCatalog(srv, CatalogOptions{
		Doc:        opts.Doc,
		Workspace:  opts.Workspace,
		Dispatcher: opts.Dispatcher,
		RootFlags:  opts.RootFlags,
		PadVersion: opts.PadVersion,
	})
	if err != nil {
		return 0, fmt.Errorf("register catalog: %w", err)
	}
	return count + catalogCount, nil
}

// MCPPropertyName converts a CLI flag or argument name to the
// snake_case form used inside the dispatch input map. Hyphens become
// underscores; everything else passes through.
//
// Why translate (TASK-964): Cobra's flag names are kebab-case
// (`--due-date`), and JSON Schema property names accept any string,
// but Anthropic's tool-use convention — and most LLMs' training data
// — is snake_case (`due_date`). Hyphenated property names trip up
// agents that auto-normalize to snake_case before emitting JSON, and
// silently lose every flag with a hyphen. The catalog's parameter
// names are snake_case; BuildCLIArgs translates them back to kebab-
// case at dispatch.
//
// Single-word names (workspace, format, content, stdin) round-trip
// unchanged.
func MCPPropertyName(cliName string) string {
	return strings.ReplaceAll(cliName, "-", "_")
}

// flagsHiddenFromMCP are flag names whose semantics depend on the
// subprocess's stdin (or other transport-only ergonomics) and so
// should NOT appear on the MCP tool surface. Agents wanting to set
// content should use the corresponding --content flag, which is wired
// through the JSON args path.
//
// Even though catalog.go declares parameters explicitly (so stdin is
// never advertised), BuildCLIArgs still defends against an agent
// passing `stdin: true` from a stale schema cache — the subprocess
// would then block on EOF and create an empty item.
var flagsHiddenFromMCP = map[string]struct{}{
	"stdin": {},
}
