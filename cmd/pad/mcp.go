package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

// mcpCmd is the parent of `pad mcp ...` subcommands.
//
// v1 ships `pad mcp serve` (stdio transport). Future tasks add:
//   - TASK-948 — `pad mcp install <agent>` to write client configs
//     for Claude Desktop / Cursor / Windsurf in one shot.
func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run pad as a Model Context Protocol server",
		Long: `Run pad as an MCP server so AI agents (Claude Desktop, Cursor, Windsurf, etc.)
can call pad commands as tools and read pad data as resources.

Use ` + "`pad mcp serve`" + ` to start the server over stdio. See
https://getpad.dev/mcp/local for client configuration.`,
	}
	cmd.AddCommand(mcpServeCmd())
	return cmd
}

// mcpServeCmd implements the stdio MCP server. The cobra command is a
// thin wrapper around internal/mcp.Server — all transport / handshake
// behaviour and tool registration live in the package so tests can
// drive them directly.
func mcpServeCmd() *cobra.Command {
	var debug bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the MCP server over stdio",
		Long: `Starts the MCP server speaking JSON-RPC on stdin/stdout.
Logs go to stderr — stdout is reserved for protocol traffic.

Intended to be spawned by an MCP client (Claude Desktop, Cursor, Windsurf,
etc.) per its mcp.json configuration. Direct human invocation is rare;
when running interactively you'll see an idle process waiting for the
client's initialize message.

The tool surface is generated automatically from this binary's
command tree (cmdhelp v0.1) — every leaf command becomes an MCP
tool, except the curated allow-list exclusions (db ops, auth setup,
init, etc.). Use ` + "`pad_set_workspace`" + ` to set the default
workspace for the session.

Shuts down cleanly on EOF, SIGINT, or SIGTERM.`,
		// Errors here are connection-level rather than arg-validation,
		// so suppress cobra's auto-printed usage block — it would
		// corrupt the JSON-RPC stream if a client misread the state.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := mcpserver.NewServer(mcpserver.Options{
				Version: fullVersion(),
				Debug:   debug,
			})

			// Build cmdhelp Document from the live cobra tree. Same
			// emitter that powers `pad help --format json`, so the MCP
			// surface and the agent-facing docs stay in lockstep.
			root := cmd.Root()
			doc := cmdhelp.Build(root, root, cmdhelp.Options{
				Binary:   "pad",
				Version:  fullVersion(),
				Homepage: padHomepage,
				MaxDepth: -1,
			})

			// Resolve the running pad binary so the dispatcher can
			// re-invoke us as a subprocess. Falls back to argv[0] if
			// os.Executable fails (rare; mostly happens on exotic
			// /proc-less platforms).
			bin, err := os.Executable()
			if err != nil || bin == "" {
				bin = os.Args[0]
			}

			// Seed the session workspace from --workspace if the user
			// passed it. After that, agents can update the default via
			// the pad_set_workspace tool.
			state := mcpserver.NewWorkspaceState(workspaceFlag)

			// Forward root persistent flags captured at startup (e.g.
			// --url) to every dispatched subprocess. Empty values are
			// skipped by BuildCLIArgs so this is safe when the user
			// didn't pass them. --workspace is excluded — it lives in
			// WorkspaceState and is mutable via pad_set_workspace.
			rootFlags := map[string]string{
				"url": urlFlag,
			}

			if _, err := mcpserver.Register(srv.MCP(), mcpserver.RegistryOptions{
				Doc:        doc,
				Workspace:  state,
				Dispatcher: &mcpserver.ExecDispatcher{Binary: bin},
				RootFlags:  rootFlags,
			}); err != nil {
				return fmt.Errorf("pad mcp serve: register tools: %w", err)
			}

			// Resource templates (TASK-946): four read-only views over
			// pad://workspace/{ws}/* URIs. Independent of the tool
			// dispatcher because resource handlers return raw bytes
			// rather than CallToolResult.
			mcpserver.RegisterResources(
				srv.MCP(),
				&mcpserver.ExecResourceFetcher{Binary: bin},
				rootFlags,
			)

			// Static prompts (TASK-947): four multi-step workflows
			// lifted from skills/pad/SKILL.md (plan, ideate, retro,
			// onboard). No subprocess / runtime dependencies — just
			// embedded text returned as user-role messages.
			mcpserver.RegisterPrompts(srv.MCP())

			if err := srv.Run(cmd.Context()); err != nil {
				return fmt.Errorf("pad mcp serve: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&debug, "debug", false, "verbose logging on stderr (development)")
	return cmd
}
