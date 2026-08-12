package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

// sessionCmd groups session-registry commands (TASK-2533).
func sessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Manage this machine's on-disk pad session registry",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(sessionRegisterCmd())
	return cmd
}

// sessionRegisterCmd registers the CURRENT process in the on-disk
// session registry (~/.pad/sessions/<pid>.json). See
// internal/cli/session_registry.go's package doc comment for what this
// is for and — as important — what it is NOT for yet: Phase 1's nudge
// delivery (the plugin monitor) needs no session registry at all; this
// is forward-looking infra for Phase 3's live-sessions/presence surface
// (IDEA-2464).
func sessionRegisterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "register",
		Short: "Register this session in the local pad session registry",
		Long: `Records this process's pid, working directory, and (when running inside a
Claude Code session) the CLAUDE_CODE_MESSAGING_SOCKET path to
~/.pad/sessions/<pid>.json.

Safe to call from any directory — it does not require a linked
workspace (.pad.toml) — and safe to call more than once per session
(each call overwrites this pid's record with a fresh timestamp).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("get working directory: %w", err)
			}
			socketPath := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")

			path, err := cli.RegisterSession(cwd, socketPath)
			if err != nil {
				return fmt.Errorf("register session: %w", err)
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]string{
					"path": path,
					"cwd":  cwd,
				})
			}
			fmt.Printf("Registered session (pid %d) at %s\n", os.Getpid(), path)
			return nil
		},
	}
}
