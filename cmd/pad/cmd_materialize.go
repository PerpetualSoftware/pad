package main

import (
	"os"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/spf13/cobra"
)

// materializeWorkerCmd is the op-log materializer worker process (TASK-2198):
// the server spawns it and speaks internal/materialize's length-prefixed JSON
// protocol over its stdin/stdout. Hidden, and not a user command: it is off
// the cmdhelp surface (cmdhelp skips Hidden commands) and so off every MCP
// door.
//
// Its stdout IS the protocol channel. Nothing else may write there, which is
// why it takes no flags that print, and why errors go to stderr (cobra's
// default for errors) with usage suppressed.
func materializeWorkerCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "__materialize-worker",
		Short:         "Internal: op-log materializer worker (spawned by the server)",
		Hidden:        true,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Before the bundle loads: a worker whose server died must not
			// run on without its deadline kill and memory watchdog.
			materialize.ExitWhenOrphaned()
			return materialize.RunWorker(os.Stdin, os.Stdout, pad.MaterializerJS)
		},
	}
}
