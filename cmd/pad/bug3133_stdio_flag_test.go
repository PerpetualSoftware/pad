package main

import (
	"slices"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
)

// BUG-3133: over local stdio, MCP's overwrite_pending_edits reaches the server
// only if BuildCLIArgs turns it into a real `pad item update` flag. Driven
// against the REAL cobra tree, because a hand-built cmdhelp.Command would vouch
// for the mapper and not for the binding (CONVE-19).
func TestStdioMapsOverwritePendingEditsToTheRealFlag(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
	cmdInfo, ok := doc.Commands["item update"]
	if !ok {
		t.Fatal(`cmdhelp has no "item update" command`)
	}
	args, err := mcp.BuildCLIArgs(cmdInfo, map[string]any{
		"ref":                     "TASK-5",
		"content":                 "x",
		"expected_seq":            float64(4),
		"overwrite_pending_edits": true,
	}, "ws", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	if !slices.Contains(args, "--overwrite-pending-edits") {
		t.Fatalf("the param was dropped on the stdio transport; args = %v", args)
	}

	// Control: absent or false emits nothing.
	args, err = mcp.BuildCLIArgs(cmdInfo, map[string]any{"ref": "TASK-5", "overwrite_pending_edits": false}, "ws", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	if slices.Contains(args, "--overwrite-pending-edits") {
		t.Fatalf("false must not emit the flag; args = %v", args)
	}
}

// The flag must reach the wire, and its absence must send nothing.
func TestItemUpdate_OverwritePendingEditsFlagIsSent(t *testing.T) {
	body := captureUpdateBody(t, "TASK-9", "--content", "x", "--expected-seq", "4", "--overwrite-pending-edits")
	if body["overwrite_pending_edits"] != true {
		t.Fatalf("overwrite_pending_edits = %v, want true: %v", body["overwrite_pending_edits"], body)
	}
	body = captureUpdateBody(t, "TASK-9", "--content", "x", "--expected-seq", "4")
	if _, ok := body["overwrite_pending_edits"]; ok {
		t.Fatalf("overwrite_pending_edits invented for a caller who sent no flag: %v", body)
	}
}
