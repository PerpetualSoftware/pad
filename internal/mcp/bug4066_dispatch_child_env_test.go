package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/config"
)

// TestBUG4066MCPChild is the child side: it loads config with a relative
// data dir, as any `pad` command does, then fails, so its stderr is what the
// dispatcher folds into the agent-facing error.
func TestBUG4066MCPChild(t *testing.T) {
	if os.Getenv("BUG4066_MCP_CHILD") != "1" {
		t.Skip("child-process entry point")
	}
	if _, err := config.Load(); err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
	}
	fmt.Fprintln(os.Stderr, "Error: the child failed on purpose")
	os.Exit(1)
}

// BUG-4066 r1 (lead ruling on Q1): a failed MCP stdio tool call carries the
// child's stderr to the agent, so the dispatcher's children are silenced; the
// relative data-dir warning must not appear in the error result.
func TestBUG4066_DispatchedChildPutsNoWarningInTheError(t *testing.T) {
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "pad")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run '^TestBUG4066MCPChild$' -test.count=1\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUG4066_MCP_CHILD", "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", "data")
	t.Setenv("PAD_DB_PATH", "")
	t.Chdir(t.TempDir())
	d := &ExecDispatcher{Binary: wrapper}
	res, err := d.Dispatch(context.Background(), []string{"item", "list"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	txt := textOf(res)
	if res == nil || !res.IsError || !strings.Contains(txt, "the child failed on purpose") {
		t.Fatalf("the child's failure did not reach the result (the test is not exercising the stderr path): %s", txt)
	}
	if strings.Contains(txt, "is relative") {
		t.Fatalf("the relative data-dir warning reached the agent-facing error: %s", txt)
	}
}
