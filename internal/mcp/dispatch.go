package mcp

import (
	"context"
	"encoding/json"
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
// natively. Non-zero exit returns an IsError-flagged result with
// stderr as the message.
type ExecDispatcher struct {
	// Binary is the path to the pad executable. Required.
	Binary string
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
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return mcp.NewToolResultErrorf(
			"pad %s failed: %s", strings.Join(cmdPath, " "), msg,
		), nil
	}
	out := stdout.String()
	// If stdout is JSON, surface it as structured content alongside
	// the text fallback. This is what `--format json` emits for most
	// pad commands; clients that parse structured content (Claude
	// Desktop, Cursor) get richer rendering.
	if trimmed := strings.TrimSpace(out); strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var parsed any
		if json.Unmarshal([]byte(trimmed), &parsed) == nil {
			return mcp.NewToolResultStructured(parsed, out), nil
		}
	}
	return mcp.NewToolResultText(out), nil
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

	if !workspaceProvided && sessionWorkspace != "" {
		out = append(out, "--workspace", sessionWorkspace)
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
