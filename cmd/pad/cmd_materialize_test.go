package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
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
