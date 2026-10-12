package main

import (
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/spf13/cobra"
)

// addWorkerIDFlag declares --worker-id on a lease-aware command (claim,
// release, project next / ready; TASK-3613). Unset, the client sends this
// session's own worker id (cli.ResolveWorkerID).
func addWorkerIDFlag(cmd *cobra.Command, dst *string) {
	cmd.Flags().StringVar(dst, "worker-id", "", `lease worker id ("wk_" + 32 hex) to act as (default: this session's own, from pad session register or $PAD_WORKER_ID)`)
}

// applyWorkerID points client at an explicit --worker-id, if one was given.
func applyWorkerID(client *cli.Client, id string) error {
	if id == "" {
		return nil
	}
	return client.SetWorkerID(id)
}
