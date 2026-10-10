package main

import (
	"log/slog"
	"os"
	"strings"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/PerpetualSoftware/pad/internal/server"
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

// newMaterializerFromEnv builds the op-log recovery worker's Materializer
// (TASK-2198 U4): a materialize.Supervisor configured from
// PAD_MATERIALIZE_TIMEOUT / PAD_MATERIALIZE_MEM_LIMIT. It returns nil, and
// never calls build, when PAD_MATERIALIZE=off: then no Supervisor exists and
// no worker process is ever spawned. build is NewSupervisor in production.
func newMaterializerFromEnv(getenv func(string) string, logger *slog.Logger, build func(materialize.SupervisorConfig) server.Materializer) server.Materializer {
	if !materialize.Enabled(getenv, logger) {
		logger.Info("op-log recovery disabled (" + materialize.EnvSwitch + "=off)")
		return nil
	}
	return build(materialize.ConfigFromEnv(getenv, logger))
}

func newSupervisorMaterializer(cfg materialize.SupervisorConfig) server.Materializer {
	return materialize.NewSupervisor(cfg)
}

// opLogCompactEnv turns dormancy compaction on (TASK-3531): "on" enables it,
// anything else (the default) keeps the op-log GC deleting dormant logs.
const opLogCompactEnv = "PAD_OPLOG_COMPACT"

// opLogCompactorFromEnv returns the compactor when PAD_OPLOG_COMPACT=on and the
// materializer is enabled (the snapshot runs on its worker), else nil.
func opLogCompactorFromEnv(getenv func(string) string, logger *slog.Logger, m server.Materializer) server.OpLogCompactor {
	if strings.TrimSpace(strings.ToLower(getenv(opLogCompactEnv))) != "on" {
		return nil
	}
	c, ok := m.(server.OpLogCompactor)
	if !ok || m == nil {
		logger.Warn("op-log compaction requested but the materializer is off; dormant op-logs are deleted as before",
			"env", opLogCompactEnv)
		return nil
	}
	logger.Info("op-log compaction on: dormant op-logs are compacted into one snapshot instead of deleted", "env", opLogCompactEnv)
	return c
}
