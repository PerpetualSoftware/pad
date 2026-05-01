package main

import (
	"fmt"

	"github.com/spf13/cobra"

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
// behaviour lives in the package so tests can drive it directly.
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
			if err := srv.Run(cmd.Context()); err != nil {
				return fmt.Errorf("pad mcp serve: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&debug, "debug", false, "verbose logging on stderr (development)")
	return cmd
}
