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
//   - `pad mcp serve` — run the stdio MCP server.
//   - `pad mcp install [agent]` — write a "pad" entry into a client's
//     MCP config (Claude Desktop / Cursor / Windsurf).
//   - `pad mcp uninstall <agent>` — remove the entry.
//   - `pad mcp status` — show install state across all supported clients.
func mcpCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Run pad as a Model Context Protocol server",
		Long: `Run pad as an MCP server so AI agents (Claude Desktop, Cursor, Windsurf, etc.)
can call pad commands as tools and read pad data as resources.

Use ` + "`pad mcp serve`" + ` to start the server over stdio.
Use ` + "`pad mcp install`" + ` to register pad with a client app.
See https://getpad.dev/mcp/local for client configuration.`,
	}
	cmd.AddCommand(
		mcpServeCmd(),
		mcpInstallCmd(),
		mcpUninstallCmd(),
		mcpStatusCmd(),
	)
	return cmd
}

// mcpInstallCmd implements `pad mcp install [agent]`.
//
// With no arguments, prints the install status across supported
// agents (so the user picks one). With an agent name, writes a
// `pad` entry into that agent's MCP config. With --all, installs
// for every supported agent (creating config dirs as needed).
func mcpInstallCmd() *cobra.Command {
	var allFlag bool
	cmd := &cobra.Command{
		Use:   "install [agent]",
		Short: "Install pad as an MCP server for a client app",
		Long: `Write a "pad" entry into the named agent's MCP server config.

Supported agents:
  claude-desktop   Claude Desktop
  cursor           Cursor
  windsurf         Windsurf

With no arguments, lists current install status across supported
agents (run ` + "`pad mcp status`" + ` for the same view). With
--all, installs for every supported agent, creating config files
on demand.

Existing MCP server entries (other servers configured by the user)
are preserved — only the "pad" entry is touched.`,
		ValidArgs: agentValidArgs(),
		// Cobra defaults to ArbitraryArgs for cmds without Args set —
		// silently accepts extras, which Codex caught as a UX bug
		// (`pad mcp install cursor windsurf` would only install Cursor).
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// --all is mutually exclusive with an agent name. Without
			// this guard `pad mcp install --all cursor` would install
			// every agent and silently drop the cursor argument,
			// which Codex round 1 flagged as confusing.
			if allFlag && len(args) > 0 {
				return fmt.Errorf("--all cannot be combined with an agent name")
			}
			binary, err := os.Executable()
			if err != nil || binary == "" {
				binary = os.Args[0]
			}
			inst := &mcpserver.Installer{Binary: binary}
			switch {
			case allFlag:
				return runMCPInstallAll(cmd, inst)
			case len(args) == 0:
				return runMCPStatus(cmd, inst)
			default:
				return runMCPInstallOne(cmd, inst, args[0])
			}
		},
	}
	cmd.Flags().BoolVar(&allFlag, "all", false, "install for every supported agent")
	return cmd
}

// mcpUninstallCmd implements `pad mcp uninstall <agent>`.
func mcpUninstallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "uninstall <agent>",
		Short: "Remove the pad MCP entry from a client's config",
		Long: `Remove the "pad" entry from the named agent's MCP server
config. Other server entries are preserved. Idempotent: removing a
non-existent entry is not an error.`,
		ValidArgs:    agentValidArgs(),
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := &mcpserver.Installer{}
			path, removed, err := inst.Uninstall(args[0])
			if err != nil {
				return err
			}
			if removed {
				fmt.Fprintf(cmd.OutOrStdout(), "Removed pad MCP entry from %s\n  config: %s\n", args[0], path)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: pad entry not present (nothing to remove)\n  config: %s\n", args[0], path)
			}
			return nil
		},
	}
	return cmd
}

// mcpStatusCmd implements `pad mcp status` — same as
// `pad mcp install` with no args, but exposed as a dedicated command
// for shell scripts.
func mcpStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show pad MCP install status across supported clients",
		RunE: func(cmd *cobra.Command, args []string) error {
			binary, err := os.Executable()
			if err != nil || binary == "" {
				binary = os.Args[0]
			}
			inst := &mcpserver.Installer{Binary: binary}
			return runMCPStatus(cmd, inst)
		},
	}
}

func agentValidArgs() []string {
	out := make([]string, 0, len(mcpserver.SupportedAgents))
	for _, a := range mcpserver.SupportedAgents {
		out = append(out, a.Name)
	}
	return out
}

func runMCPStatus(cmd *cobra.Command, inst *mcpserver.Installer) error {
	w := cmd.OutOrStdout()
	fmt.Fprintln(w, "Pad MCP install status:")
	fmt.Fprintln(w)
	for _, row := range inst.Status() {
		marker := "[ ]"
		if row.Installed {
			marker = "[x]"
		}
		fmt.Fprintf(w, "  %s %-20s %s\n", marker, row.Label, row.ConfigPath)
		if row.Error != "" {
			fmt.Fprintf(w, "      error: %s\n", row.Error)
		}
		if row.Installed && row.Command != "" {
			fmt.Fprintf(w, "      command: %s\n", row.Command)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Install: pad mcp install <agent>   (or --all for every supported)")
	return nil
}

func runMCPInstallOne(cmd *cobra.Command, inst *mcpserver.Installer, agent string) error {
	path, modified, err := inst.Install(agent)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	if modified {
		fmt.Fprintf(w, "Installed pad MCP entry for %s\n  config: %s\n  command: %s mcp serve\n", agent, path, inst.Binary)
		fmt.Fprintln(w, "  → Restart the client to pick up the new server entry.")
	} else {
		fmt.Fprintf(w, "%s already up to date\n  config: %s\n", agent, path)
	}
	return nil
}

func runMCPInstallAll(cmd *cobra.Command, inst *mcpserver.Installer) error {
	w := cmd.OutOrStdout()
	for _, a := range mcpserver.SupportedAgents {
		path, modified, err := inst.Install(a.Name)
		if err != nil {
			fmt.Fprintf(w, "  ✗ %s — skipped: %v\n", a.Label, err)
			continue
		}
		if modified {
			fmt.Fprintf(w, "  ✓ %s installed (%s)\n", a.Label, path)
		} else {
			fmt.Fprintf(w, "  ✓ %s already up to date (%s)\n", a.Label, path)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Restart each client to pick up the new server entry.")
	return nil
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

			// As of TASK-981 (PLAN-969) Register registers pad_set_workspace
			// + every v0.2 catalog tool in one call. The cmdhelp leaf
			// walker that powered v0.1 has been retired; cmdhelp is
			// still consumed at dispatch time (BuildCLIArgs reads
			// individual command schemas) but no longer drives the tool
			// surface shape.
			// Pre-flatten rootFlags into the token list ExecDispatcher
			// reuses for its WorkspaceLister side channel — when
			// classifyExecError needs to populate available_workspaces,
			// it spawns `pad workspace list` and must hit the same
			// server endpoint (e.g. --url for non-default servers).
			rootArgs := []string{}
			for k, v := range rootFlags {
				if v == "" {
					continue
				}
				rootArgs = append(rootArgs, "--"+k, v)
			}
			dispatcher := &mcpserver.ExecDispatcher{Binary: bin, RootArgs: rootArgs}
			if _, err := mcpserver.Register(srv.MCP(), mcpserver.RegistryOptions{
				Doc:        doc,
				Workspace:  state,
				Dispatcher: dispatcher,
				RootFlags:  rootFlags,
				PadVersion: fullVersion(),
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

			// Tool-surface stability metadata (TASK-963). Static
			// resource at pad://_meta/version. Complements the
			// handshake's capabilities.experimental.padCmdhelp for
			// clients that prefer reading a JSON document.
			mcpserver.RegisterMeta(srv.MCP(), fullVersion())

			if err := srv.Run(cmd.Context()); err != nil {
				return fmt.Errorf("pad mcp serve: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&debug, "debug", false, "verbose logging on stderr (development)")
	return cmd
}
