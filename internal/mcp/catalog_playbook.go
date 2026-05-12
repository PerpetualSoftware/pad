package mcp

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
				Name:        "raw_args",
				Type:        "array",
				Description: "Raw CLI-style argument tokens (positional values, bareword flag names, key=value pairs). The server applies the strict parsing rules from `pad playbook run`. Use when the agent is forwarding user input verbatim. Optional for action=run.",
			},
		},
	},
	Actions: map[string]ActionFn{
		"list": passThrough([]string{"playbook", "list"}),
		"get":  passThrough([]string{"playbook", "show"}),
		"run":  passThrough([]string{"playbook", "run"}),
	},
}

const padPlaybookToolDescription = `Playbooks — first-class invokable procedures (PLAN-1377).

Playbooks are agent-executed multi-step workflows that ship in a workspace's
playbooks collection. Each can declare a kebab-case ` + "`invocation_slug`" + ` (e.g.
"ship", "release") that maps directly to ` + "`/pad <slug>`" + ` in chat-driven skills,
and an ` + "`## Arguments`" + ` section that names + types the inputs it expects.

Actions:
  list  — Workspace playbook catalog. Returns metadata only (no bodies):
          ref, title, slug, invocation_slug, trigger, scope, status,
          has_arguments, summary. Sorted with invocation_slug-bearing
          playbooks first so user-facing /pad <slug> candidates surface
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
