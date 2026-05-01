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

// RegistryOptions configures Register.
type RegistryOptions struct {
	// Doc is the cmdhelp Document describing the command tree. Required.
	Doc *cmdhelp.Document
	// Workspace holds the session workspace mutated by pad_set_workspace.
	// Required.
	Workspace *WorkspaceState
	// Dispatcher executes tool calls. Required (use ExecDispatcher in
	// production; fakes in tests).
	Dispatcher Dispatcher
	// ExcludeCommands lists command paths to skip (space-separated, e.g.
	// "db backup"). A path that matches a registered group prefix
	// suppresses every leaf under it — excluding "completion" suppresses
	// "completion bash", "completion zsh", etc.
	//
	// nil = use DefaultExcludes. An empty slice = exclude nothing.
	ExcludeCommands []string
}

// DefaultExcludes is the curated allow-list scope: read + item-CRUD +
// project intelligence. Excluded commands are unsafe (mutate auth or
// filesystem state outside the workspace), interactive (prompt the
// user), recursive (start another MCP server), or long-running
// (streaming watchers).
//
// Operators who want a tighter or looser surface override via
// RegistryOptions.ExcludeCommands.
var DefaultExcludes = []string{
	// Recursive — would spawn another MCP server inside this one.
	"mcp serve",
	"mcp install",
	// Lifecycle — server start/stop and DB ops are operator-only.
	"server start",
	"server stop",
	"db backup",
	"db restore",
	"db migrate-to-pg",
	// Auth — interactive password prompts, mutate credentials file.
	"auth setup",
	"auth login",
	"auth logout",
	"auth configure",
	"auth reset-password",
	// Workspace lifecycle — interactive or filesystem-mutating.
	"init",
	"workspace init",
	"workspace import",
	"workspace export",
	"workspace onboard",
	"workspace join",
	// Editor — opens $EDITOR (subprocess never returns).
	"item edit",
	// Streaming — long-running watchers tie up an MCP slot.
	"project watch",
	// Filesystem mutations outside the workspace.
	"agent install",
	"agent update",
	// Shell completion — not useful as an MCP tool surface.
	"completion",
}

// Register walks opts.Doc, registers every leaf command (no
// children in the document) as an MCP tool on srv, and installs the
// built-in pad_set_workspace tool. Returns the count of tools
// registered (including pad_set_workspace).
//
// Tool names use snake_case: "item create" → "item_create". Inputs
// follow the cmdhelp Arg/Flag types. Dispatch goes through
// opts.Dispatcher with the workspace flag injected from
// opts.Workspace if not explicitly provided.
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

	excludes := opts.ExcludeCommands
	if excludes == nil {
		excludes = DefaultExcludes
	}
	excludeSet := make(map[string]struct{}, len(excludes))
	for _, e := range excludes {
		excludeSet[e] = struct{}{}
	}

	registerSetWorkspaceTool(srv, opts.Workspace)
	count := 1 // pad_set_workspace

	paths := make([]string, 0, len(opts.Doc.Commands))
	for p := range opts.Doc.Commands {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	leaves := identifyLeaves(paths)

	for _, path := range paths {
		if !leaves[path] {
			continue
		}
		if _, blocked := excludeSet[path]; blocked {
			continue
		}
		if hasExcludedAncestor(path, excludeSet) {
			continue
		}
		cmdInfo := opts.Doc.Commands[path]
		tool := buildTool(path, cmdInfo)
		handler := makeDispatchHandler(path, cmdInfo, opts.Dispatcher, opts.Workspace)
		srv.AddTool(tool, handler)
		count++
	}
	return count, nil
}

// ToolNameFromPath converts a cmdhelp command path ("item create") to
// the canonical MCP tool-name form ("item_create"). Exported so the
// dispatch helpers and tests can compute names without re-implementing
// the rule.
func ToolNameFromPath(path string) string {
	return strings.ReplaceAll(path, " ", "_")
}

// identifyLeaves returns a map of path → true for every command path
// that is NOT a strict prefix of another path (and is therefore a leaf).
func identifyLeaves(paths []string) map[string]bool {
	leaf := make(map[string]bool, len(paths))
	for _, p := range paths {
		leaf[p] = true
	}
	for _, p := range paths {
		for _, q := range paths {
			if p != q && strings.HasPrefix(q, p+" ") {
				leaf[p] = false
				break
			}
		}
	}
	return leaf
}

// hasExcludedAncestor returns true when any space-delimited prefix of
// path appears in excludes. Used so excluding "completion" suppresses
// "completion bash", "completion zsh", etc.
func hasExcludedAncestor(path string, excludes map[string]struct{}) bool {
	parts := strings.Split(path, " ")
	for i := 1; i < len(parts); i++ {
		prefix := strings.Join(parts[:i], " ")
		if _, ok := excludes[prefix]; ok {
			return true
		}
	}
	return false
}

func buildTool(path string, cmdInfo cmdhelp.Command) mcp.Tool {
	desc := cmdInfo.Summary
	if cmdInfo.Description != "" {
		if desc != "" {
			desc += "\n\n"
		}
		desc += cmdInfo.Description
	}
	opts := []mcp.ToolOption{mcp.WithDescription(desc)}

	for _, arg := range cmdInfo.Args {
		opts = append(opts, propertyForArg(arg))
	}

	flagNames := make([]string, 0, len(cmdInfo.Flags))
	for name := range cmdInfo.Flags {
		flagNames = append(flagNames, name)
	}
	sort.Strings(flagNames)
	for _, name := range flagNames {
		opts = append(opts, propertyForFlag(name, cmdInfo.Flags[name]))
	}
	return mcp.NewTool(ToolNameFromPath(path), opts...)
}

func propertyForArg(arg cmdhelp.Arg) mcp.ToolOption {
	propOpts := propertyOptionsCommon(arg.Description, arg.Required, arg.Enum)
	if arg.Repeatable {
		return mcp.WithArray(arg.Name, append(propOpts, mcp.WithStringItems())...)
	}
	switch arg.Type {
	case "int", "number", "float", "float64", "int64":
		return mcp.WithNumber(arg.Name, propOpts...)
	case "bool":
		return mcp.WithBoolean(arg.Name, propOpts...)
	default:
		return mcp.WithString(arg.Name, propOpts...)
	}
}

func propertyForFlag(name string, flag cmdhelp.Flag) mcp.ToolOption {
	propOpts := propertyOptionsCommon(flag.Description, flag.Required, flag.Enum)
	if flag.Repeatable {
		return mcp.WithArray(name, append(propOpts, mcp.WithStringItems())...)
	}
	switch flag.Type {
	case "bool":
		return mcp.WithBoolean(name, propOpts...)
	case "int", "number", "int64", "uint", "uint64", "float", "float64":
		return mcp.WithNumber(name, propOpts...)
	case "[]string", "stringSlice", "[]int", "intSlice", "stringArray":
		return mcp.WithArray(name, append(propOpts, mcp.WithStringItems())...)
	default:
		return mcp.WithString(name, propOpts...)
	}
}

func propertyOptionsCommon(description string, required bool, enum []interface{}) []mcp.PropertyOption {
	out := make([]mcp.PropertyOption, 0, 3)
	if description != "" {
		out = append(out, mcp.Description(description))
	}
	if required {
		out = append(out, mcp.Required())
	}
	if len(enum) > 0 {
		out = append(out, mcp.Enum(stringifyEnum(enum)...))
	}
	return out
}

func stringifyEnum(values []interface{}) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprint(v)
	}
	return out
}

// makeDispatchHandler closes over the command path + dispatcher so
// each registered tool routes to the right CLI invocation.
func makeDispatchHandler(
	path string,
	cmdInfo cmdhelp.Command,
	dispatcher Dispatcher,
	state *WorkspaceState,
) server.ToolHandlerFunc {
	cmdPath := strings.Split(path, " ")
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		input := req.GetArguments()
		args, err := BuildCLIArgs(cmdInfo, input, state.Get())
		if err != nil {
			return mcp.NewToolResultErrorf("%s: %s", ToolNameFromPath(path), err.Error()), nil
		}
		return dispatcher.Dispatch(ctx, cmdPath, args)
	}
}
