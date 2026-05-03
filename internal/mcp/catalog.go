package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// ─────────────────────────────────────────────────────────────────────
// MCP tool catalog (v0.2) — the hand-curated resource × action surface
// that replaces PLAN-942's cmdhelp-derived 1:1 verb tools.
//
// Architecture (DOC-978 / TASK-970):
//
//   - Catalog declares ~7 ToolDefs (pad_item, pad_workspace, ...).
//   - Each ToolDef has a map of action names → ActionFn.
//   - The registered handler for a ToolDef is a thin fan-out shim: it
//     reads `action` from the input, looks up the handler, and invokes
//     it. Most ActionFns delegate to env.Dispatch(cmdPath, input) which
//     forwards through the existing Dispatcher (ExecDispatcher /
//     HTTPHandlerDispatcher) — so the dispatch contract below the
//     registry boundary is unchanged.
//
//   - cmdhelp remains the source of truth for individual CLI command
//     schemas (args, flags, types). env.Dispatch uses opts.Doc to look
//     up the cmdInfo for a cmdPath and then calls BuildCLIArgs. The
//     catalog merely chooses which cmdPath to invoke for each action.
//
//   - A few ToolDefs (notably pad_meta) handle their actions inline
//     without dispatching to a CLI cmdPath. Those just return a
//     CallToolResult directly from the ActionFn.
//
// Why a fan-out layer instead of replacing the dispatcher: TASK-965
// + TASK-967 + TASK-968 stabilized the dispatcher and route table for
// HTTP transport. Reshaping the surface in the registry without
// touching dispatch keeps that investment intact and gives ExecDispatcher
// + HTTPHandlerDispatcher the v0.2 surface for free.
// ─────────────────────────────────────────────────────────────────────

// ToolDef is one v0.2 tool: name, description, schema, and the actions
// it supports. Hand-curated; one ToolDef per catalog_<resource>.go file.
type ToolDef struct {
	// Name is the MCP tool name as advertised in tools/list. Stable
	// across versions — a rename is a ToolSurfaceVersion bump.
	Name string

	// Description shows up in tools/list and in Claude Desktop's tool
	// picker. Should enumerate each action's purpose + required params
	// inline, since the schema below uses a flat union of all
	// parameters across actions and relies on the description for
	// per-action discoverability.
	Description string

	// Schema is the union of parameters across all actions. Only
	// `action` is required at the schema level; per-action constraints
	// (e.g. "create requires title") are enforced inside the action
	// handler and surface as structured validation errors.
	Schema ToolSchema

	// Actions maps the `action` enum value to its handler. Snake_case
	// for verb-only ("list", "create"); kebab-case where the verb is
	// compound ("list-comments", "tool-surface", "blocked-by"). The
	// distinction matches CLI subcommand naming so agents can mentally
	// map `pad_item.action: create` ↔ `pad item create`.
	Actions map[string]ActionFn
}

// ToolSchema is a structured description of the tool's input shape.
// Compiled to mcp.ToolOption slices when registering with mcp-go.
//
// Workspace, when true, adds an optional `workspace` string to the
// schema. Most tools need it; pad_meta does not.
type ToolSchema struct {
	Workspace bool
	Params    []ParamDef
}

// ParamDef declares one parameter on a tool's input schema. Type maps
// to mcp-go's helper functions (WithString, WithNumber, WithBoolean,
// WithArray). Enum, when non-empty, constrains string parameters.
type ParamDef struct {
	Name        string
	Type        string // "string" | "number" | "bool" | "array<string>"
	Description string
	Enum        []string
}

// ActionFn handles one tool action. Receives the call context, the raw
// MCP input map, and a per-tool execution environment. Returns the
// CallToolResult that gets surfaced to the agent.
//
// Two patterns:
//
//  1. Fan-out (most actions): call env.Dispatch(ctx, cmdPath, input).
//     For the common case of "this action is just a CLI subcommand,
//     forward the input verbatim", use the passThrough helper.
//
//  2. Inline (pad_meta + similar): build the CallToolResult directly
//     from in-memory state (ToolSurfaceVersion, the catalog itself,
//     etc.) without dispatching anywhere.
type ActionFn func(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error)

// ActionEnv carries everything an action handler needs to either
// dispatch to a CLI cmdPath or build a result inline. Wired up once
// per RegisterCatalog call; passed by value into every action handler
// (small struct; copy is cheap; immutable in practice).
type ActionEnv struct {
	// Doc is the cmdhelp document — source of truth for CLI command
	// schemas consumed by BuildCLIArgs.
	Doc *cmdhelp.Document

	// Workspace is the session workspace state. Read by env.Dispatch
	// (and by inline handlers that care about default workspace).
	Workspace *WorkspaceState

	// Dispatcher executes CLI cmdPaths. Same instance used by the
	// v0.1 cmdhelp-walk path; both surfaces share dispatch.
	Dispatcher Dispatcher

	// RootFlags are startup-captured persistent flags (e.g. --url)
	// forwarded to every dispatched call.
	RootFlags map[string]string

	// PadVersion is the runtime pad binary version. Threaded through
	// for pad_meta.action: server-info / version.
	PadVersion string

	// Catalog is the live catalog slice — supplied so pad_meta.action:
	// tool-surface can introspect the catalog without an import cycle.
	Catalog []ToolDef
}

// Dispatch is the helper most fan-out actions use. Looks up cmdInfo
// in env.Doc, builds CLI args, attaches the dispatch input to ctx,
// forwards to env.Dispatcher.
//
// Unknown cmdPath produces a "registry bug" error rather than panicking
// — catches catalog/cmdhelp drift early in tests. A clean run on
// startup ensures every action's cmdPath maps to a real CLI command.
func (env ActionEnv) Dispatch(ctx context.Context, cmdPath []string, input map[string]any) (*mcp.CallToolResult, error) {
	if input == nil {
		input = map[string]any{}
	}
	pathStr := strings.Join(cmdPath, " ")
	cmdInfo, ok := env.Doc.Commands[pathStr]
	if !ok {
		return NewErrorResult(ErrorPayload{
			Code:    ErrServerError,
			Message: fmt.Sprintf("action mapped to unknown cmdPath %q (catalog/cmdhelp drift)", pathStr),
			Hint:    "Catalog action's cmdPath doesn't appear in the cmdhelp Doc — programmer error in the catalog. File a bug.",
		}), nil
	}
	cliArgs, err := BuildCLIArgs(cmdInfo, input, env.Workspace.Get(), env.RootFlags)
	if err != nil {
		// BUG-987 bug 12: BuildCLIArgs returns plain Go errors for
		// missing required args / bad types. Previously those came
		// out as bare-text MCP results, breaking the structured
		// envelope contract that every other error path follows.
		// Wrap as validation_failed so agents see a consistent
		// shape across the surface and can branch on error.code.
		return validationFailedFromBuildErr(pathStr, err), nil
	}
	ctx = WithDispatchInput(ctx, mergeDispatchInput(input, env.Workspace.Get(), env.RootFlags))
	return env.Dispatcher.Dispatch(ctx, cmdPath, cliArgs)
}

// passThrough returns an ActionFn that forwards the input verbatim to
// the given cmdPath via env.Dispatch. The vast majority of catalog
// actions use this — a custom handler is only needed when the action
// reshapes the input or dispatches on a parameter (see actionItemLink
// for the link_type → cmdPath dispatch pattern).
func passThrough(cmdPath []string) ActionFn {
	return func(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
		return env.Dispatch(ctx, cmdPath, input)
	}
}

// Catalog is the live v0.2 tool catalog. Populated by per-tool init()
// functions in catalog_<resource>.go via appendToCatalog so each tool
// definition lives in its own file without a central registration list
// drifting out of sync.
var Catalog []ToolDef

// appendToCatalog inserts def into Catalog. Called from
// catalog_<resource>.go init() functions. Keep this small + simple;
// validation happens at RegisterCatalog time (where errors surface
// once at startup rather than per-init).
func appendToCatalog(def ToolDef) {
	Catalog = append(Catalog, def)
}

// CatalogOptions configures RegisterCatalog. Mirrors RegistryOptions
// for the cmdhelp-walk path but adds PadVersion (needed by pad_meta).
type CatalogOptions struct {
	Doc        *cmdhelp.Document
	Workspace  *WorkspaceState
	Dispatcher Dispatcher
	RootFlags  map[string]string
	PadVersion string
}

// RegisterCatalog installs the v0.2 catalog tools on srv. Returns the
// count of tools registered (excluding pad_set_workspace, which the
// cmdhelp-walk path in Register() owns).
//
// Called alongside Register() from cmd/pad/mcp.go during the parallel-
// rollout phase of TASK-970. Once TASK-981 lands, Register() collapses
// into RegisterCatalog and the cmdhelp leaf walker goes away.
func RegisterCatalog(srv *server.MCPServer, opts CatalogOptions) (int, error) {
	if err := validateCatalogOptions(opts); err != nil {
		return 0, err
	}
	env := ActionEnv{
		Doc:        opts.Doc,
		Workspace:  opts.Workspace,
		Dispatcher: opts.Dispatcher,
		RootFlags:  opts.RootFlags,
		PadVersion: opts.PadVersion,
		Catalog:    Catalog,
	}
	count := 0
	for _, def := range Catalog {
		tool := buildToolFromDef(def)
		handler := makeFanOutHandler(def, env)
		srv.AddTool(tool, handler)
		count++
	}
	return count, nil
}

func validateCatalogOptions(opts CatalogOptions) error {
	if opts.Doc == nil {
		return errCatalogMissing("Doc")
	}
	if opts.Workspace == nil {
		return errCatalogMissing("Workspace")
	}
	if opts.Dispatcher == nil {
		return errCatalogMissing("Dispatcher")
	}
	return nil
}

// errCatalogMissing keeps the validation error messages in one place
// so the surface is consistent across the three required fields.
func errCatalogMissing(field string) error {
	return &catalogError{msg: "CatalogOptions." + field + " is required"}
}

type catalogError struct{ msg string }

func (e *catalogError) Error() string { return e.msg }

// buildToolFromDef compiles a ToolDef into the mcp.Tool shape mcp-go
// expects. The schema is flat: `action` is required, every other
// parameter is optional at the schema level (per-action validation
// runs in the action handler). This matches DOC-978's design — JSON
// Schema discriminated unions are awkward in mcp-go's helpers, and
// the description carries the action × params matrix anyway.
func buildToolFromDef(def ToolDef) mcp.Tool {
	opts := []mcp.ToolOption{mcp.WithDescription(def.Description)}

	// `action` is always required and always enumerated. Sorted for
	// deterministic schema output across builds.
	actions := sortedActionNames(def)
	opts = append(opts,
		mcp.WithString("action",
			mcp.Description("The action to perform. See tool description for required parameters per action."),
			mcp.Required(),
			mcp.Enum(actions...),
		),
	)

	if def.Schema.Workspace {
		opts = append(opts,
			mcp.WithString("workspace",
				mcp.Description(
					"Workspace slug to target for this call. Resolution order: "+
						"(1) explicit value here wins, (2) else session default set via "+
						"pad_set_workspace, (3) else the CWD-linked workspace from .pad.toml. "+
						"Pass explicitly to switch workspaces mid-session without affecting "+
						"the session default.",
				),
			),
		)
	}

	for _, p := range def.Schema.Params {
		opts = append(opts, paramDefToToolOption(p))
	}

	return mcp.NewTool(def.Name, opts...)
}

// paramDefToToolOption maps a ParamDef to the matching mcp-go helper.
// Unknown Type strings fall through to WithString — same defensive
// default the cmdhelp-walk path uses.
func paramDefToToolOption(p ParamDef) mcp.ToolOption {
	propOpts := make([]mcp.PropertyOption, 0, 2)
	if p.Description != "" {
		propOpts = append(propOpts, mcp.Description(p.Description))
	}
	if len(p.Enum) > 0 {
		propOpts = append(propOpts, mcp.Enum(p.Enum...))
	}
	switch p.Type {
	case "number":
		return mcp.WithNumber(p.Name, propOpts...)
	case "bool":
		return mcp.WithBoolean(p.Name, propOpts...)
	case "array<string>":
		return mcp.WithArray(p.Name, append(propOpts, mcp.WithStringItems())...)
	default:
		return mcp.WithString(p.Name, propOpts...)
	}
}

// makeFanOutHandler returns the ToolHandlerFunc that mcp-go invokes
// when the tool is called. Reads `action` from the input, looks up the
// handler, and forwards. Unknown / missing action returns a structured
// error result so the agent sees the valid options and can recover.
//
// The catalog's `action` key is stripped from the input before the
// action handler runs. It's the fan-out routing field — its value
// names the handler we just dispatched to, so the handler doesn't
// need it. More importantly, leaving it in would shadow any CLI flag
// named `--action` when the handler eventually dispatches: e.g.
// `pad workspace audit-log --action item.created` reuses the same
// flag name as our routing key, and BuildCLIArgs would emit
// `--action audit-log` (the catalog action name) instead of the
// user's filter. Stripping at the boundary keeps action handlers
// free to talk to the dispatcher in CLI-flag terms without watching
// for that collision.
func makeFanOutHandler(def ToolDef, env ActionEnv) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input := req.GetArguments()
		if input == nil {
			input = map[string]any{}
		}
		action, _ := input["action"].(string)
		if action == "" {
			return errMissingAction(def), nil
		}
		handler, ok := def.Actions[action]
		if !ok {
			return errUnknownAction(def, action), nil
		}
		// Clone before stripping so we don't mutate req.Params.Arguments
		// — defensive against any future mcp-go change that re-reads the
		// request map after dispatch.
		stripped := make(map[string]any, len(input))
		for k, v := range input {
			if k == "action" {
				continue
			}
			stripped[k] = v
		}
		return handler(ctx, stripped, env)
	}
}

// sortedActionNames returns def.Actions' keys in lexical order. Used
// for both the schema's action enum (so the ordering is stable across
// builds and reproducible in tests) and the error helpers below (so
// users see actions alphabetized rather than in map-iteration order).
func sortedActionNames(def ToolDef) []string {
	out := make([]string, 0, len(def.Actions))
	for name := range def.Actions {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// errMissingAction is the structured result returned when the input
// has no `action` field. Lists the valid actions inline so agents can
// retry with a correct value.
func errMissingAction(def ToolDef) *mcp.CallToolResult {
	return NewErrorResult(ErrorPayload{
		Code:    ErrValidationFailed,
		Message: fmt.Sprintf("%s: missing required field 'action'", def.Name),
		Hint:    "Pass `action=<verb>`. Valid actions: " + strings.Join(sortedActionNames(def), ", "),
	})
}

// errUnknownAction is the structured result returned when `action` is
// set but not in the tool's action table. Same listing as
// errMissingAction so agents can self-correct.
func errUnknownAction(def ToolDef, action string) *mcp.CallToolResult {
	return NewErrorResult(ErrorPayload{
		Code:    ErrValidationFailed,
		Message: fmt.Sprintf("%s: unknown action %q", def.Name, action),
		Hint:    "Valid actions: " + strings.Join(sortedActionNames(def), ", "),
	})
}
