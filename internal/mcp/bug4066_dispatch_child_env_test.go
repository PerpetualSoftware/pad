package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	wrapper := bug4066ChildWrapper(t)
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

// bug4066ChildWrapper writes a `pad` stand-in that re-runs this test binary
// as the failing child, with a relative data dir in the parent's environment.
func bug4066ChildWrapper(t *testing.T) string {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "pad")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run '^TestBUG4066MCPChild$' -test.count=1\n", os.Args[0])
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BUG4066_MCP_CHILD", "1")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PAD_DATA_DIR", "data")
	t.Setenv("PAD_DB_PATH", "")
	t.Chdir(t.TempDir())
	return wrapper
}

// The resource and bootstrap fetchers fold a failed child's stderr into their
// errors too (BUG-4066 r2), so they are silenced the same way.
func TestBUG4066_FetchersPutNoWarningInTheError(t *testing.T) {
	wrapper := bug4066ChildWrapper(t)
	for name, call := range map[string]func() error{
		"resource text": func() error {
			_, err := (&ExecResourceFetcher{Binary: wrapper}).run(context.Background(), []string{"item", "list"}, 0)
			return err
		},
		"resource bytes": func() error {
			_, err := (&ExecResourceFetcher{Binary: wrapper}).FetchBytes(context.Background(), []string{"item", "list"})
			return err
		},
		"bootstrap": func() error {
			_, err := (&ExecBootstrapFetcher{Binary: wrapper}).Bootstrap(context.Background(), "ws")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil || !strings.Contains(err.Error(), "the child failed on purpose") {
				t.Fatalf("the child's failure did not reach the error (the test is not exercising the stderr path): %v", err)
			}
			if strings.Contains(err.Error(), "is relative") {
				t.Fatalf("the relative data-dir warning reached the error: %v", err)
			}
		})
	}
}

// Population: every exec of a pad binary in this package's non-test source
// sets cmd.Env = dispatchChildEnv(), so a new fetcher cannot reopen the leak.
func TestBUG4066_EveryPadExecSetsTheChildEnv(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	execRe := regexp.MustCompile(`(\w+)\s*:?=\s*exec\.Command(?:Context)?\(`)
	total := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			m := execRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			total++
			want := m[1] + ".Env = dispatchChildEnv()"
			ok := false
			for _, next := range lines[i+1 : min(i+8, len(lines))] {
				if strings.Contains(next, want) {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("%s:%d execs a binary without %s", f, i+1, want)
			}
		}
	}
	if total < 4 {
		t.Fatalf("found only %d execs; the scan is not seeing them", total)
	}
}
