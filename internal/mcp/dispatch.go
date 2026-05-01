package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// Dispatcher executes a pad CLI command on behalf of a tool call. It
// always returns a *mcp.CallToolResult — error paths set IsError on
// the result so MCP clients see structured stderr without having to
// distinguish protocol errors from tool errors.
//
// Two implementations ship:
//
//   - ExecDispatcher (this file) — shells out to the pad binary with
//     cliArgs. Used by `pad mcp serve` for local stdio MCP, where the
//     subprocess inherits the user's credentials from `~/.pad/credentials.json`.
//   - HTTPHandlerDispatcher (dispatch_http.go) — calls pad-cloud's
//     HTTP handlers in-process with the requesting user attached to
//     the request context. Used by the future `/mcp` endpoint
//     (PLAN-943 TASK-950) where the dispatcher serves multiple OAuth
//     users from a single process and can't safely shell out.
//
// Both consume the same cliArgs (built by BuildCLIArgs) so the
// registry stays transport-agnostic. The HTTP dispatcher additionally
// reads the original JSON input map via DispatchInputFromContext so it
// doesn't have to reverse-parse cliArgs back to typed values; the
// registry attaches that input via WithDispatchInput before calling
// Dispatch.
type Dispatcher interface {
	Dispatch(ctx context.Context, cmdPath []string, cliArgs []string) (*mcp.CallToolResult, error)
}

// dispatchInputKey is the unexported context-key type used to forward
// the original MCP tool-call JSON arguments from the registry to a
// dispatcher implementation. Unexported so callers can only set/read
// it via WithDispatchInput / DispatchInputFromContext, which keeps the
// type discipline obvious.
type dispatchInputKey struct{}

// WithDispatchInput returns ctx decorated with the original MCP tool-
// call JSON input map. Called from the registry's tool handler before
// invoking Dispatch. ExecDispatcher ignores it (it dispatches via
// cliArgs); HTTPHandlerDispatcher reads it to build a structured HTTP
// body without reverse-parsing CLI flags.
func WithDispatchInput(ctx context.Context, input map[string]any) context.Context {
	if input == nil {
		return ctx
	}
	return context.WithValue(ctx, dispatchInputKey{}, input)
}

// DispatchInputFromContext returns the original MCP tool-call JSON
// input attached by WithDispatchInput, or nil if none. Safe to call
// on any context.
func DispatchInputFromContext(ctx context.Context) map[string]any {
	v, _ := ctx.Value(dispatchInputKey{}).(map[string]any)
	return v
}

// mergeDispatchInput returns a copy of the user-supplied MCP input
// augmented with the same default injections BuildCLIArgs applies —
// session workspace, root flags — so the dispatcher receives the
// effective input rather than a partial map. Pure function (no
// mutation of `input`); safe to call with a nil input map.
//
// HTTPHandlerDispatcher reads the merged map via
// DispatchInputFromContext to build a structured request body without
// reverse-parsing CLI flags. ExecDispatcher ignores it (it dispatches
// via cliArgs).
func mergeDispatchInput(input map[string]any, sessionWorkspace string, rootFlags map[string]string) map[string]any {
	out := make(map[string]any, len(input)+len(rootFlags)+1)
	for k, v := range input {
		out[k] = v
	}
	if _, ok := out["workspace"]; !ok && sessionWorkspace != "" {
		out["workspace"] = sessionWorkspace
	}
	for k, v := range rootFlags {
		if v == "" {
			continue
		}
		// MCP property names are snake_case (TASK-964) — translate
		// rootFlags' kebab-case keys before checking collision.
		propName := MCPPropertyName(k)
		if _, ok := out[propName]; ok {
			continue
		}
		out[propName] = v
	}
	return out
}

// ExecDispatcher shells out to the pad binary at Binary. stdout is
// returned as Text content; if it parses as JSON the result is also
// surfaced via StructuredContent so MCP clients can consume it
// natively. Non-zero exit returns an IsError-flagged result with a
// structured ErrorEnvelope (TASK-973) — stderr is classified into a
// closed-set ErrorCode so agents can branch on the code rather than
// parsing free-form text.
type ExecDispatcher struct {
	// Binary is the path to the pad executable. Required.
	Binary string

	// RootArgs are pre-formatted root-flag tokens (e.g. ["--url", X])
	// to forward to every spawned subprocess. Same data as the
	// rootFlags map carried by RegistryOptions but pre-flattened so
	// the dispatcher doesn't re-iterate it on every Dispatch call.
	// Used by the WorkspaceLister side-channel — when classifyExecError
	// needs to populate available_workspaces, it spawns
	// `pad workspace list` with these flags so it hits the same
	// endpoint as the original failed call.
	RootArgs []string
}

// Dispatch runs `<Binary> <cmdPath...> <cliArgs...>` and packages the
// output for an MCP client.
func (d *ExecDispatcher) Dispatch(ctx context.Context, cmdPath []string, cliArgs []string) (*mcp.CallToolResult, error) {
	if d.Binary == "" {
		return mcp.NewToolResultError("dispatcher: binary path not configured"), nil
	}
	full := append(append([]string{}, cmdPath...), cliArgs...)
	cmd := exec.CommandContext(ctx, d.Binary, full...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Classify into the structured envelope. WorkspaceLister
		// uses the same dispatcher (recursive in spirit but a fresh
		// subprocess) to enrich no_workspace / unknown_workspace
		// errors with available_workspaces.
		return classifyExecError(ctx, cmdPath, err, stderr.String(), d), nil
	}
	out := stdout.String()
	// If stdout is JSON, surface it as structured content alongside
	// the text fallback. This is what `--format json` emits for most
	// pad commands; clients that parse structured content (Claude
	// Desktop, Cursor) get richer rendering.
	return packageJSONResult(out), nil
}

// packageJSONResult converts a CLI stdout string into an MCP tool
// result. JSON bodies surface as structuredContent; non-JSON falls
// back to text. Top-level arrays are wrapped in `{items: [...]}` so
// MCP host validators (Claude Desktop) accept the structured shape —
// without the wrap they reject the tool call with "expected: record"
// (BUG-985 bug 3).
//
// Shared between ExecDispatcher and HTTPHandlerDispatcher's
// packageHTTPResponse so both transports produce the same wire
// shape.
func packageJSONResult(out string) *mcp.CallToolResult {
	trimmed := strings.TrimSpace(out)
	if !strings.HasPrefix(trimmed, "{") && !strings.HasPrefix(trimmed, "[") {
		return mcp.NewToolResultText(out)
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return mcp.NewToolResultText(out)
	}
	if arr, ok := parsed.([]any); ok {
		// Wrap top-level arrays. MCP host validators (Claude Desktop)
		// require `structuredContent` to be an object/record, not an
		// array. The original JSON text is still returned as the text
		// fallback so clients that don't parse structured content keep
		// seeing the raw shape they used to.
		return mcp.NewToolResultStructured(map[string]any{"items": arr}, out)
	}
	return mcp.NewToolResultStructured(parsed, out)
}

// ListWorkspaces satisfies WorkspaceLister so error helpers can
// populate available_workspaces hints. Best-effort: if listing fails
// (no auth, network down, etc.) the caller treats an error as "no
// listing available" and surfaces the bare error envelope.
//
// Implementation note: spawns a fresh `pad workspace list --format
// json` subprocess. Adds one extra exec per error path that needs the
// hint — acceptable cost given how rare these errors are. Honors
// RootArgs so the listing hits the same server endpoint as the
// originally-failed call (e.g. --url for non-default servers).
func (d *ExecDispatcher) ListWorkspaces(ctx context.Context) ([]WorkspaceHint, error) {
	if d.Binary == "" {
		return nil, errors.New("dispatcher: binary path not configured")
	}
	args := []string{"workspace", "list", "--format", "json"}
	args = append(args, d.RootArgs...)
	cmd := exec.CommandContext(ctx, d.Binary, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("workspace list: %s", strings.TrimSpace(stderr.String()))
	}
	return parseWorkspaceListJSON(stdout.String())
}

// parseWorkspaceListJSON decodes the JSON shape `pad workspace list
// --format json` emits into the WorkspaceHint slice. Lenient about
// optional fields — only `slug` is required; everything else gets
// zero values when absent.
func parseWorkspaceListJSON(body string) ([]WorkspaceHint, error) {
	body = strings.TrimSpace(body)
	if body == "" || body == "null" {
		return nil, nil
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, fmt.Errorf("decode workspace list: %w", err)
	}
	out := make([]WorkspaceHint, 0, len(raw))
	for _, w := range raw {
		slug, _ := w["slug"].(string)
		if slug == "" {
			continue
		}
		name, _ := w["name"].(string)
		isDefault, _ := w["default"].(bool)
		out = append(out, WorkspaceHint{Slug: slug, Name: name, Default: isDefault})
	}
	return out, nil
}

// BuildCLIArgs translates an MCP tool call's JSON arguments into the
// CLI argument list that should be appended after the command path.
// Pure function — no side effects, no subprocess — so it tests cleanly.
//
// Behaviour:
//   - cmdInfo.Args are emitted as positional arguments in their
//     declared order. Required args missing from input return an error.
//   - Repeatable args expand each element into a separate positional.
//   - cmdInfo.Flags are emitted as `--flag value` pairs in
//     alphabetical order (deterministic for tests). Bool flags are
//     presence-only (`--flag`, never `--flag=true`).
//   - Repeatable / slice-typed flags expand to repeated `--flag value`
//     pairs.
//   - Flags listed in flagsHiddenFromMCP (e.g. `stdin`) are dropped
//     defensively — they reference subprocess stdin which the MCP
//     transport doesn't pipe through.
//   - sessionWorkspace is appended as `--workspace <ws>` if the
//     command does NOT already define a `workspace` flag in the
//     input map AND sessionWorkspace is non-empty.
//   - rootFlags (e.g. `--url` captured at server startup) are
//     appended when the input doesn't already supply them. Empty
//     values in rootFlags are skipped.
//   - `--format json` is appended unless the input explicitly sets a
//     format flag (e.g. an agent specifically requests markdown).
func BuildCLIArgs(
	cmdInfo cmdhelp.Command,
	input map[string]any,
	sessionWorkspace string,
	rootFlags map[string]string,
) ([]string, error) {
	if input == nil {
		input = map[string]any{}
	}

	var positionals []string
	for _, arg := range cmdInfo.Args {
		// MCP property names are snake_case (TASK-964) — look up the
		// translated form so kebab-case CLI args (rare for positionals
		// but possible in custom commands) round-trip correctly.
		propName := MCPPropertyName(arg.Name)
		v, ok := input[propName]
		if !ok {
			if arg.Required {
				return nil, fmt.Errorf("missing required argument %q", arg.Name)
			}
			continue
		}
		if arg.Repeatable {
			items, err := toStringSlice(v)
			if err != nil {
				return nil, fmt.Errorf("argument %q: %w", arg.Name, err)
			}
			positionals = append(positionals, items...)
		} else {
			positionals = append(positionals, fmt.Sprint(v))
		}
	}

	// Sorted flag order makes test assertions stable.
	flagNames := make([]string, 0, len(cmdInfo.Flags))
	for name := range cmdInfo.Flags {
		flagNames = append(flagNames, name)
	}
	sort.Strings(flagNames)

	formatProvided := false
	workspaceProvided := false
	var flagArgs []string
	for _, name := range flagNames {
		// Defensive: even if an agent's stale schema cache passes
		// stdin=true, drop it here — buildTool already hides it from
		// new schemas, and the running subprocess can't read agent stdin.
		if _, hidden := flagsHiddenFromMCP[name]; hidden {
			continue
		}
		flag := cmdInfo.Flags[name]
		// Input map keys are MCP property names (snake_case per TASK-964),
		// while `name` is the CLI flag name (kebab-case for compound
		// flags like --due-date). Translate when reading the input;
		// continue to emit the kebab-case form on the CLI.
		propName := MCPPropertyName(name)
		val, ok := input[propName]
		if !ok {
			continue
		}
		if name == "format" {
			formatProvided = true
		}
		if name == "workspace" {
			workspaceProvided = true
		}
		switch flag.Type {
		case "bool":
			b, err := toBool(val)
			if err != nil {
				return nil, fmt.Errorf("flag %q: %w", name, err)
			}
			if b {
				flagArgs = append(flagArgs, "--"+name)
			}
		default:
			if isSliceFlag(flag) {
				items, err := toStringSlice(val)
				if err != nil {
					return nil, fmt.Errorf("flag %q: %w", name, err)
				}
				for _, item := range items {
					flagArgs = append(flagArgs, "--"+name, item)
				}
				continue
			}
			flagArgs = append(flagArgs, "--"+name, fmt.Sprint(val))
		}
	}

	out := append(positionals, flagArgs...)

	// Inject root-level flags (e.g. --url) when the input didn't
	// already supply them. Sorted for deterministic test output.
	// rootFlags keys are CLI form (kebab-case if compound); input keys
	// are MCP form (snake_case per TASK-964) — translate before the
	// collision check so we don't double-emit a flag the agent already
	// passed.
	if len(rootFlags) > 0 {
		rootNames := make([]string, 0, len(rootFlags))
		for name := range rootFlags {
			rootNames = append(rootNames, name)
		}
		sort.Strings(rootNames)
		for _, name := range rootNames {
			val := rootFlags[name]
			if val == "" {
				continue
			}
			if _, alreadyInInput := input[MCPPropertyName(name)]; alreadyInInput {
				continue
			}
			out = append(out, "--"+name, val)
		}
	}

	// `--workspace` is a persistent root flag on cobra (registered
	// globally via rootCmd, not on individual leaf commands), so it
	// never appears in cmdInfo.Flags and the per-flag iteration above
	// can't see it. Honor it here directly: prefer explicit
	// input["workspace"], fall back to sessionWorkspace.
	//
	// Without this branch, BUG-985 reproduced: agents passing
	// `workspace=docapp` explicitly got no_workspace errors because
	// the flag was silently dropped — only the session-default
	// fallback below ever ran, which itself only fires when no
	// explicit value is set.
	if !workspaceProvided {
		if explicit, _ := input["workspace"].(string); explicit != "" {
			out = append(out, "--workspace", explicit)
		} else if sessionWorkspace != "" {
			out = append(out, "--workspace", sessionWorkspace)
		}
	}
	if !formatProvided {
		out = append(out, "--format", "json")
	}
	return out, nil
}

// isSliceFlag is true when a flag should be emitted as repeated
// `--flag value` pairs rather than a single `--flag value`. Covers
// both pflag's slice types and cmdhelp's `repeatable` annotation.
func isSliceFlag(f cmdhelp.Flag) bool {
	if f.Repeatable {
		return true
	}
	switch f.Type {
	case "[]string", "stringSlice", "[]int", "intSlice", "stringArray":
		return true
	}
	return false
}

func toStringSlice(v any) ([]string, error) {
	switch t := v.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out, nil
	case string:
		return []string{t}, nil
	}
	return nil, fmt.Errorf("expected array or string, got %T", v)
}

func toBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		switch t {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no", "":
			return false, nil
		}
	case json.Number:
		s := t.String()
		return s != "" && s != "0", nil
	}
	return false, fmt.Errorf("expected bool, got %T", v)
}
