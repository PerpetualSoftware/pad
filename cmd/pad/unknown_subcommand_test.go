package main

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestUnknownSubcommandReturnsError(t *testing.T) {
	// Build a minimal tree that mirrors pad's command-group pattern.
	parent := &cobra.Command{
		Use:   "item",
		Short: "item group",
		RunE:  unknownSubcommandRun,
	}
	parent.AddCommand(
		&cobra.Command{Use: "list", Short: "list items"},
		&cobra.Command{Use: "show", Short: "show item"},
	)
	root := &cobra.Command{Use: "pad"}
	root.AddCommand(parent)

	t.Run("unknown subcommand returns error", func(t *testing.T) {
		root.SetArgs([]string{"item", "bogus"})
		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for unknown subcommand, got nil")
		}
		if !strings.Contains(err.Error(), "bogus") {
			t.Errorf("expected error to mention 'bogus', got %q", err.Error())
		}
	})

	t.Run("bare parent shows help without error", func(t *testing.T) {
		var buf bytes.Buffer
		root.SetOut(&buf)
		root.SetArgs([]string{"item"})
		err := root.Execute()
		if err != nil {
			t.Fatalf("unexpected error for bare parent: %v", err)
		}
		got := buf.String()
		if !strings.Contains(got, "Usage:") {
			t.Errorf("expected help output for bare parent, got %q", got)
		}
	})

	t.Run("valid subcommand runs without error", func(t *testing.T) {
		root.SetArgs([]string{"item", "list"})
		err := root.Execute()
		if err != nil {
			t.Fatalf("unexpected error for valid subcommand: %v", err)
		}
	})
}

// TestRealCommandTreeRejectsUnknownSubcommands walks the actual pad command
// tree and asserts every *pure container* command (a command that groups
// subcommands but does no work of its own) returns a non-nil error (exit
// non-zero, per main.go's os.Exit(1) on error) when handed an unknown
// subcommand. This guards against a newly added command group forgetting the
// unknownSubcommandRun guard — the whole point of issue #850.
//
// Commands that have subcommands but are *also independently runnable* (e.g.
// `pad workspace context`, which prints the context when run and carries a
// `set` subcommand) are out of scope: they legitimately execute with args, so
// they're skipped. Unknown-subcommand resolution on a pure container fails
// before any leaf command runs, so no network calls or side effects fire.
func TestRealCommandTreeRejectsUnknownSubcommands(t *testing.T) {
	guardPtr := reflect.ValueOf(unknownSubcommandRun).Pointer()

	var groups []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if !c.HasAvailableSubCommands() {
			return
		}
		// A pure container has no runner of its own, or uses the
		// unknownSubcommandRun guard. A command with a *different* real
		// Run/RunE is an independently runnable command — out of scope.
		guarded := c.RunE != nil && reflect.ValueOf(c.RunE).Pointer() == guardPtr
		pureContainer := c.Run == nil && c.RunE == nil
		if guarded || pureContainer {
			groups = append(groups, c.CommandPath())
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCmd())

	if len(groups) == 0 {
		t.Fatal("expected to find parent command groups, found none")
	}

	for _, path := range groups {
		t.Run(path, func(t *testing.T) {
			// Drop the leading "pad" program name, append a token that
			// cannot match any real subcommand.
			args := append(strings.Fields(path)[1:], "zzz-not-a-real-subcommand")

			root := newRootCmd()
			root.SetArgs(args)
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)

			if err := root.Execute(); err == nil {
				t.Errorf("%q: unknown subcommand returned nil error (exits 0); missing unknownSubcommandRun guard?", path)
			}
		})
	}
}
