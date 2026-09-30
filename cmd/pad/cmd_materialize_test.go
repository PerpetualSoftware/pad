package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os/exec"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/PerpetualSoftware/pad/internal/server"
)

// The materializer worker (TASK-2198) is an internal process entry point: it
// must be registered, hidden, and absent from the cmdhelp document every MCP
// door is derived from.
func TestMaterializeWorkerCmdHiddenAndOffCmdhelp(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"__materialize-worker"})
	if err != nil || cmd == nil || cmd.Name() != "__materialize-worker" {
		t.Fatalf("__materialize-worker not registered: %v", err)
	}
	if !cmd.Hidden {
		t.Fatal("__materialize-worker must be Hidden")
	}
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "materialize") {
		t.Fatal("the cmdhelp document mentions the materializer worker")
	}
}

// PAD_MATERIALIZE=off (TASK-2198 U4) must leave the server with no
// Materializer at all: the Supervisor is never built, so no worker process can
// be spawned and StartMaterializeRecovery installs no trigger.
func TestMaterializerDisabledBuildsNothing(t *testing.T) {
	for _, v := range []string{"off", "OFF", " off ", "0", "false", "no", "disabled"} {
		built := 0
		m := newMaterializerFromEnv(func(k string) string {
			if k == materialize.EnvSwitch {
				return v
			}
			return ""
		}, slog.Default(), func(materialize.SupervisorConfig) server.Materializer {
			built++
			return materialize.NewSupervisor(materialize.SupervisorConfig{})
		})
		if m != nil || built != 0 {
			t.Errorf("%s=%q: materializer %v, builds %d; want nil, 0", materialize.EnvSwitch, v, m, built)
		}
	}
	for _, v := range []string{"", "on", "1", "true", "yes", "something-else"} {
		built, spawned := 0, 0
		m := newMaterializerFromEnv(func(k string) string {
			if k == materialize.EnvSwitch {
				return v
			}
			return ""
		}, slog.Default(), func(cfg materialize.SupervisorConfig) server.Materializer {
			built++
			cfg.Command = func() (*exec.Cmd, error) {
				spawned++
				return nil, errors.New("test: no spawn")
			}
			return materialize.NewSupervisor(cfg)
		})
		if m == nil || built != 1 {
			t.Errorf("%s=%q: materializer %v, builds %d; want one", materialize.EnvSwitch, v, m, built)
		}
		if spawned != 0 {
			t.Errorf("%s=%q: building the supervisor spawned %d workers; it must start none until a job", materialize.EnvSwitch, v, spawned)
		}
		if c, ok := m.(interface{ Close() error }); ok {
			_ = c.Close()
		}
	}
}
