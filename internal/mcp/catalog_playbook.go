package mcp

import (
	"context"
	"fmt"
	"sort"

	"github.com/mark3labs/mcp-go/mcp"
)

// padPlaybookTool exposes the first-class playbook surface from
// PLAN-1377 / TASK-1381. Three actions mirror the CLI (TASK-1382):
//
//   - list — workspace playbook metadata (slug, invocation_slug,
//            trigger, status, has_arguments, summary). Same shape
//            returned by bootstrap.playbooks and `pad playbook list`.
//   - get  — full body + fields for one playbook, addressable by
//            invocation_slug, item slug, or ref. Use this before
//            invoking a playbook to read its `## Arguments` section
//            and decide what to bind.
//   - run  — parse args against the playbook's declared spec and
//            return the body + bound args + any unbound required
//            args. SIDE-EFFECT-FREE — playbooks are agent
//            instructions; the agent executes the steps, not the
//            server.
//
// All three are passThrough to the `pad playbook` CLI; the CLI is the
// canonical entry point and the MCP tool just makes it discoverable
// in tools/list. Same shape via the HTTP MCP dispatcher's routeTable
// entries for clouds that don't have a local pad binary.

func init() {
	appendToCatalog(padPlaybookTool)
}

var padPlaybookTool = ToolDef{
	Name:        "pad_playbook",
	Description: padPlaybookToolDescription,
	Schema: ToolSchema{
		Workspace: true,
		Params: []ParamDef{
			{
				Name:        "ref",
				Type:        "string",
				Description: "Playbook identifier — accepts the invocation_slug (e.g. \"ship\"), the item slug (e.g. \"cut-a-pad-release\"), or the issue ref (e.g. PLAYB-1160). Required for action=get and action=run.",
			},
			{
				Name:        "args",
				Type:        "object",
				Description: "Pre-parsed argument map keyed by the playbook's declared argument names. Use when you've already parsed user intent into discrete values (e.g. an MCP-driven agent). Mutually compatible with raw_args — explicit args take precedence. Optional for action=run.",
			},
			{
				// Note: the catalog builder's param-type recognizer
				// understands "array<string>" specifically (not the
				// generic "array"); see catalog.go::paramDefToToolOption.
				Name:        "raw_args",
				Type:        "array<string>",
				Description: "Raw CLI-style argument tokens (positional values, bareword flag names, key=value pairs). The server applies the strict parsing rules from `pad playbook run`. Use when the agent is forwarding user input verbatim. Optional for action=run.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list": passThrough([]string{"playbook", "list"}),
		"get":  passThrough([]string{"playbook", "show"}),
		// run is a custom action — the CLI's `playbook run` takes a
		// positional ref plus arbitrary trailing tokens (positional /
		// bareword-flag / key=value), and the MCP tool needs to
		// translate the structured `args` map + `raw_args` slice into
		// that token sequence before dispatching. The CLI args are
		// flatten-and-forwarded so the server-side ParsePlaybookCLIArgs
		// sees the same shape it would from a real shell.
		"run": actionPlaybookRun,
	},
}

// actionPlaybookRun is the custom dispatch for pad_playbook.action=run.
// The MCP-side input carries args as a map AND/OR raw_args as a slice;
// the two dispatcher backends each need a different shape:
//
//   - HTTPHandlerDispatcher: forwards `input` to mapPlaybookRun, which
//     accepts the original args:map shape directly and produces the
//     POST /playbooks/{ref}/run JSON body. This path preserves
//     explicit `false` values on flag-typed args, so an MCP caller
//     CAN override a flag with default=true by sending args:{flag:false}.
//
//   - ExecDispatcher: shells out to `pad playbook run <ref> <tokens...>`.
//     The CLI takes the variadic args slot (cmdhelp positional name
//     `args`, repeatable=true) so we flatten args+raw_args into a CLI
//     token sequence. Limitation: the CLI's strict parser only
//     supports bareword flag PRESENCE (no flag=false form), so a
//     local-stdio caller cannot override a flag-default-true value;
//     route through HTTP/in-process MCP for that rare case.
func actionPlaybookRun(ctx context.Context, input map[string]any, env ActionEnv) (*mcp.CallToolResult, error) {
	ref, _ := input["ref"].(string)
	if ref == "" {
		// Use the structured validation envelope (matches what
		// env.Dispatch / BuildCLIArgs produces for missing-required-arg
		// errors) so agents can branch on error.code rather than
		// regex-matching the message text.
		return NewErrorResult(ErrorPayload{
			Code:    ErrValidationFailed,
			Message: "pad_playbook.run: missing required field 'ref'",
			Hint:    "Pass `ref=<invocation_slug|item slug|issue ref>` (e.g. ship, cut-a-pad-release, PLAYB-1160).",
		}), nil
	}

	if _, isHTTP := env.Dispatcher.(*HTTPHandlerDispatcher); isHTTP {
		// Bypass env.Dispatch — its BuildCLIArgs step would choke on
		// args:map (the cmdhelp arg `args` is a repeatable positional
		// expecting strings). HTTPHandlerDispatcher reads the original
		// input from context via DispatchInputFromContext, so we
		// attach it manually and call the dispatcher directly. The
		// resulting POST body preserves structured args including
		// explicit false values on flag-typed entries.
		ctx = WithDispatchInput(ctx, mergeDispatchInput(input, env.Workspace.ResolveDefault(), env.RootFlags))
		return env.Dispatcher.Dispatch(ctx, []string{"playbook", "run"}, nil)
	}

	// Exec path — flatten into CLI tokens. Sort map keys for
	// deterministic ordering so tests + CLI replay stay stable.
	var tokens []string
	if raw, ok := input["raw_args"]; ok {
		switch v := raw.(type) {
		case []any:
			for _, t := range v {
				if s, ok := t.(string); ok && s != "" {
					tokens = append(tokens, s)
				}
			}
		case []string:
			for _, s := range v {
				if s != "" {
					tokens = append(tokens, s)
				}
			}
		}
	}
	if argsMap, ok := input["args"].(map[string]any); ok {
		keys := make([]string, 0, len(argsMap))
		for k := range argsMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := argsMap[k]
			switch tv := v.(type) {
			case bool:
				if tv {
					tokens = append(tokens, k)
				}
				// false: skipped because the CLI parser only accepts
				// bareword flag PRESENCE. Document this limitation in
				// the function docstring; HTTP path covers the rare
				// flag-default-true-override case.
			case nil:
				// Skip — caller signaled "leave unbound"; let the
				// server's bindPlaybookArgs surface it as unbound.
			default:
				tokens = append(tokens, fmt.Sprintf("%s=%v", k, tv))
			}
		}
	}

	// Re-shape the input map so BuildCLIArgs picks up ref as the first
	// positional and the trailing tokens as variadic-args. cmdhelp
	// records `args[]` as the variadic slot for `playbook run`.
	rewired := map[string]any{
		"ref":  ref,
		"args": tokens,
	}
	if ws, ok := input["workspace"].(string); ok && ws != "" {
		rewired["workspace"] = ws
	}
	return env.Dispatch(ctx, []string{"playbook", "run"}, rewired)
}

const padPlaybookToolDescription = `Playbooks — first-class invokable procedures (PLAN-1377).

Playbooks are agent-executed multi-step workflows that ship in a workspace's
playbooks collection. Each can declare a kebab-case ` + "`invocation_slug`" + ` (e.g.
"ship", "release"), and an ` + "`## Arguments`" + ` section that names + types the
inputs it expects. Natural language is the canonical way to invoke one
("ship these tasks"); the slug is a per-surface shortcut — this ` + "`pad_playbook`" + `
tool with ` + "`action: run, ref: <slug>`" + ` here, ` + "`/pad <slug>`" + ` in Claude Code,
` + "`$pad <slug>`" + ` in Codex — that resolves to the same playbook.

Actions:
  list  — Workspace playbook catalog. Returns metadata only (no bodies):
          ref, title, slug, invocation_slug, trigger, scope, status,
          has_arguments, summary. Sorted with invocation_slug-bearing
          playbooks first so user-invokable candidates surface
          at the top.
          Required: workspace.
  get   — Full item for one playbook, including body content + structured
          fields (which carry the canonical ` + "`arguments`" + ` JSON spec).
          Required: workspace, ref.
  run   — Parse args against the playbook's declared spec and return the
          body + bound_args + any required-but-unbound entries (so the
          agent can prompt the user instead of failing the call). Side-
          effect-free: the playbook body is markdown instructions for the
          agent, not a shell script — the server primes the call; the
          agent executes the steps.
          Required: workspace, ref.
          Optional: args (pre-parsed map), raw_args (CLI-style tokens).
                    Pass exactly one or merge — see the per-param docs.

Use pad_playbook when an agent needs to dispatch a named procedure or read
a playbook's declared argument contract before invoking it. For browsing
the underlying items (search, comments, links), use pad_item.`
