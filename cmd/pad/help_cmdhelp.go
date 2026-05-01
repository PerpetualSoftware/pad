package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/config"
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

// cmdhelpOptions builds the Options struct shared by JSON and markdown
// emitters. Centralized so both formats see the same dynamic resolver,
// version metadata, and homepage.
//
// Binary is derived from the root command's name rather than hardcoded
// so tests constructing a synthetic root (e.g. "padtest") get a
// document keyed to that name.
func cmdhelpOptions(target *cobra.Command, depth int, all bool) cmdhelp.Options {
	maxDepth := depth
	if all {
		maxDepth = -1
	}
	return cmdhelp.Options{
		Binary:   target.Root().Name(),
		Version:  fullVersion(),
		Homepage: padHomepage,
		MaxDepth: maxDepth,
		Resolver: newDynamicResolver(),
	}
}

// emitCmdhelpJSON walks the cobra command tree below `target` and emits
// a cmdhelp v0.1 JSON document matching schema/cmdhelp.schema.json.
//
// `--all` is a convenience alias for unlimited depth (spec §4); when
// passed it overrides any explicit `--depth=N`.
func emitCmdhelpJSON(target *cobra.Command, depth int, all bool, w io.Writer) error {
	return cmdhelp.EmitJSON(target, target.Root(), cmdhelpOptions(target, depth, all), w)
}

// emitCmdhelpMarkdown walks the cobra command tree below `target` and
// emits a cmdhelp v0.1 markdown document with the predictable section
// order from spec §6. Also serves `--format llm` as an alias today
// (reserved for future "best for LLMs" default).
//
// `--all` is a convenience alias for unlimited depth (spec §4); when
// passed it overrides any explicit `--depth=N`.
func emitCmdhelpMarkdown(target *cobra.Command, depth int, all bool, w io.Writer) error {
	return cmdhelp.EmitMarkdown(target, target.Root(), cmdhelpOptions(target, depth, all), w)
}

// newDynamicResolver constructs a cmdhelp.Resolver bound to the current
// pad runtime: workspace, configured server URL, and credentials.
//
// Returns nil whenever the runtime is unavailable — no config, no
// workspace detected, or any error during detection. nil disables
// dynamic resolution entirely; the help command emits a purely static
// document. This is the right behavior because `pad help` MUST work
// even when pad isn't authenticated, the server isn't running, or the
// invocation happens outside any workspace.
//
// Per-source failures (server unreachable, auth missing) are handled
// inside the resolver itself: enum_source is announced on the binding
// arg/flag but Enum is left empty.
func newDynamicResolver() *cmdhelp.Resolver {
	cfg, err := config.Load()
	if err != nil || !cfg.IsConfigured() {
		return nil
	}
	if urlFlag != "" {
		cfg.URL = urlFlag
		cfg.LoadedFromFlags = true
	}

	ws, err := cli.DetectWorkspace(workspaceFlag)
	if err != nil || ws == "" {
		return nil
	}

	client := cli.NewClientFromURL(cfg.BaseURL())

	// Per-command bindings: --role and --assign only resolve to agent
	// roles / workspace members on commands where that's their actual
	// semantic. They MUST NOT bind globally because:
	//
	//   pad workspace invite --role         # workspace role: owner|editor|viewer
	//   pad item create     --role <slug>   # agent role slug
	//   pad item update     --role <slug>   # agent role slug
	//   pad item create     --assign <user> # workspace member
	//   pad item update     --assign <user> # workspace member
	//   pad item list       --assign <user> # workspace member (filter)
	//
	// `--role` on `workspace invite` is intentionally left without a
	// dynamic binding so help correctly reflects the static workspace-
	// role enum (owner/editor/viewer) rather than agent role slugs.
	itemRoleAssign := map[string]string{
		"role":   cmdhelp.EnumSourceRoles,
		"assign": cmdhelp.EnumSourceMembers,
	}
	return &cmdhelp.Resolver{
		Workspace: ws,
		// `<collection>` is unambiguous across the entire CLI — every
		// instance refers to a pad collection. Safe to bind globally.
		ArgEnumSources: map[string]string{
			"collection": cmdhelp.EnumSourceCollections,
		},
		CommandFlagBindings: map[string]map[string]string{
			"item create": itemRoleAssign,
			"item update": itemRoleAssign,
			"item list":   {"assign": cmdhelp.EnumSourceMembers},
		},
		Sources: map[string]cmdhelp.DynamicEnum{
			cmdhelp.EnumSourceCollections: func() ([]interface{}, error) {
				cols, err := client.ListCollections(ws)
				if err != nil {
					return nil, err
				}
				out := make([]interface{}, 0, len(cols))
				for _, c := range cols {
					out = append(out, c.Slug)
				}
				return out, nil
			},
			cmdhelp.EnumSourceRoles: func() ([]interface{}, error) {
				roles, err := client.ListAgentRoles(ws)
				if err != nil {
					return nil, err
				}
				out := make([]interface{}, 0, len(roles))
				for _, r := range roles {
					out = append(out, r.Slug)
				}
				return out, nil
			},
			cmdhelp.EnumSourceMembers: func() ([]interface{}, error) {
				members, err := client.ListWorkspaceMembers(ws)
				if err != nil {
					return nil, err
				}
				out := make([]interface{}, 0, len(members))
				for _, m := range members {
					if m.UserUsername != "" {
						out = append(out, m.UserUsername)
					} else if m.UserName != "" {
						out = append(out, m.UserName)
					} else if m.UserEmail != "" {
						out = append(out, m.UserEmail)
					}
				}
				return out, nil
			},
		},
	}
}
