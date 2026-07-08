package main

import (
	"bytes"
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
