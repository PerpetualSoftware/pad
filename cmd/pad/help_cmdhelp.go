package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// CmdhelpVersion is the cmdhelp wire-format version this binary advertises.
// MAJOR.MINOR only — never includes a PATCH component (cmdhelp v0.1 §9).
//
// Re-exported from internal/cmdhelp so the CLI layer can reference the
// constant without importing the emitter package in places that only
// need the version string.
const CmdhelpVersion = cmdhelp.Version

// padHomepage is the canonical project URL emitted in the cmdhelp
// document's `homepage` field.
const padHomepage = "https://getpad.dev"

// helpCmd returns a custom `pad help` subcommand that replaces cobra's
// built-in help. It implements the cmdhelp v0.1 mandatory surface
// (https://getpad.dev/cmdhelp; IDEA-927):
//
//	pad help [subcommand…] [--format <fmt>] [--depth <n>] [--all]
//
// Default --format=text delegates to the existing cobra help renderer so
// behavior matches the previous built-in. The structured emitters
// (--format json|md|llm) land in TASK-934/935; they are stubbed here as
// "not yet implemented" errors so the routing layer is testable
// independently of the emitter work.
func helpCmd() *cobra.Command {
	var format string
	var depth int
	var all bool

	cmd := &cobra.Command{
		Use:   "help [command path…]",
		Short: "Help about any command",
		Long: `Show help for the pad CLI tree.

Default output is the human-readable text format (matching cobra's built-in
help). Pass --format json|md|llm to emit machine-readable output per the
cmdhelp v0.1 spec (https://getpad.dev/cmdhelp).

Scope:
  pad help                       full tree summary
  pad help <group>               one subtree (e.g. ` + "`pad help item`" + `)
  pad help <group> <command>     single command, deep`,
		DisableFlagsInUseLine: true,
		// Errors from this command (unknown topic, unknown --format, stub
		// emitter not-yet-implemented) are self-describing — suppress the
		// auto-printed usage block so terminal output stays focused on the
		// error message.
		SilenceUsage: true,
		ValidArgsFunction: func(c *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
			parent, _, _ := c.Root().Find(args)
			if parent == nil {
				parent = c.Root()
			}
			var names []string
			for _, sub := range parent.Commands() {
				if sub.Hidden || sub.Name() == "help" {
					continue
				}
				if strings.HasPrefix(sub.Name(), toComplete) {
					names = append(names, sub.Name())
				}
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(c *cobra.Command, args []string) error {
			target, _, err := c.Root().Find(args)
			if err != nil || target == nil {
				return fmt.Errorf("unknown help topic %q", strings.Join(args, " "))
			}

			switch strings.ToLower(format) {
			case "", "text":
				target.InitDefaultHelpFlag()
				target.InitDefaultVersionFlag()
				return target.Help()
			case "json":
				return emitCmdhelpJSON(target, depth, all, c.OutOrStdout())
			case "md", "llm":
				return emitCmdhelpMarkdown(target, depth, all, c.OutOrStdout())
			default:
				return fmt.Errorf("unknown --format %q (want one of: text, md, json, llm)", format)
			}
		},
	}

	cmd.Flags().StringVar(&format, "format", "text", "output format: text, md, json, llm (alias for md) — per cmdhelp v0.1")
	cmd.Flags().IntVar(&depth, "depth", -1, "truncate the command tree to N levels (-1 = unlimited; --depth=0 is summary)")
	cmd.Flags().BoolVar(&all, "all", false, "include every leaf with full detail (convenience alias for unlimited depth)")

	return cmd
}

// emitCmdhelpJSON walks the cobra command tree below `target` and emits
// a cmdhelp v0.1 JSON document matching schema/cmdhelp.schema.json.
//
// `--all` is a convenience alias for unlimited depth (spec §4); when
// passed it overrides any explicit `--depth=N`.
func emitCmdhelpJSON(target *cobra.Command, depth int, all bool, w io.Writer) error {
	maxDepth := depth
	if all {
		maxDepth = -1
	}
	return cmdhelp.EmitJSON(target, target.Root(), cmdhelp.Options{
		Binary:   target.Root().Name(),
		Version:  fullVersion(),
		Homepage: padHomepage,
		MaxDepth: maxDepth,
	}, w)
}

// emitCmdhelpMarkdown is a stub for the cmdhelp v0.1 markdown emitter
// (also serves --format llm as an alias). The real implementation walks
// the cobra command tree and writes a document matching the predictable
// section order in cmdhelp v0.1 §6. Lands in TASK-935.
func emitCmdhelpMarkdown(target *cobra.Command, depth int, all bool, w io.Writer) error {
	_ = target
	_ = depth
	_ = all
	_ = w
	return fmt.Errorf("--format md/llm not yet implemented (cmdhelp v%s markdown emitter — TASK-935)", CmdhelpVersion)
}
