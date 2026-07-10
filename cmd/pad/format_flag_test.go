package main

import (
	"io"
	"strings"
	"testing"
)

// TestFormatFlagValidation covers the root PersistentPreRunE added for
// BUG-2032: a bogus --format value must fail loudly instead of silently
// rendering a table, while the three real formats pass through.
func TestFormatFlagValidation(t *testing.T) {
	// formatFlag is a package-level var. Every newRootCmd() rebinds it to
	// its "table" default (StringVar assigns the default at registration),
	// but restore it defensively so this test can't leak a value into
	// others sharing the process.
	orig := formatFlag
	defer func() { formatFlag = orig }()

	t.Run("invalid --format is rejected before the command runs", func(t *testing.T) {
		root := newRootCmd()
		// collection list reaches PersistentPreRunE; the validation error
		// fires before RunE, so no network call is made.
		root.SetArgs([]string{"collection", "list", "--format", "bogus"})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)

		err := root.Execute()
		if err == nil {
			t.Fatal("expected error for --format bogus, got nil")
		}
		if !strings.Contains(err.Error(), "invalid --format") {
			t.Errorf("expected error to mention 'invalid --format', got %q", err.Error())
		}
	})

	t.Run("valid --format values pass validation", func(t *testing.T) {
		root := newRootCmd()
		if root.PersistentPreRunE == nil {
			t.Fatal("root has no PersistentPreRunE — --format validation not wired")
		}
		for _, f := range []string{"table", "json", "markdown"} {
			formatFlag = f
			if err := root.PersistentPreRunE(root, nil); err != nil {
				t.Errorf("--format %q should pass validation, got %v", f, err)
			}
		}
	})

	t.Run("PersistentPreRunE rejects an unknown value directly", func(t *testing.T) {
		root := newRootCmd()
		formatFlag = "yaml"
		err := root.PersistentPreRunE(root, nil)
		if err == nil {
			t.Fatal("expected error for --format yaml, got nil")
		}
		if !strings.Contains(err.Error(), "invalid --format") {
			t.Errorf("expected 'invalid --format' error, got %q", err.Error())
		}
	})
}
