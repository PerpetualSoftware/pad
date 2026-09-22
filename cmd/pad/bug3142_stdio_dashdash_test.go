package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
)

// BUG-3142: over local stdio, BuildCLIArgs used to emit positionals FIRST and
// no `--`, so cobra parsed any free-text value starting with `-` as a flag and
// refused the call before the command ran. Driven against the REAL cobra tree
// and each command's own flag parser, because a hand-built cmdhelp.Command
// would vouch for the mapper and not for the binding (CONVE-19).
//
// The population is every stdio-reachable FREE-TEXT positional, enumerated on
// BUG-3142's trail from the command tree (the other positionals are refs,
// slugs and ids, for which a `-`-leading value is invalid anyway).
func TestStdioFreeTextPositionalsSurviveALeadingDash(t *testing.T) {
	// ParseFlags writes the root's persistent flags into package globals
	// (formatFlag, workspaceFlag, urlFlag). Leaving `--format json` behind
	// changed what every later test in this package printed.
	savedFormat, savedWorkspace, savedURL := formatFlag, workspaceFlag, urlFlag
	t.Cleanup(func() { formatFlag, workspaceFlag, urlFlag = savedFormat, savedWorkspace, savedURL })

	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})

	type row struct {
		path  string         // cmdhelp key, e.g. "item create"
		input map[string]any // MCP input; the free-text arg carries the hostile value
		want  []string       // positionals the command must receive, in order
	}
	const dash = "-ship it"
	rows := []row{
		{"item create", map[string]any{"collection": "tasks", "title": dash}, []string{"tasks", dash}},
		{"item comment", map[string]any{"ref": "TASK-5", "message": dash}, []string{"TASK-5", dash}},
		{"item note", map[string]any{"ref": "TASK-5", "summary": dash}, []string{"TASK-5", dash}},
		{"item decide", map[string]any{"ref": "TASK-5", "decision": dash}, []string{"TASK-5", dash}},
		{"item search", map[string]any{"query": dash}, []string{dash}},
		{"playbook match", map[string]any{"text": dash}, []string{dash}},
		{"playbook run", map[string]any{"ref": "ship", "args": []any{"-x", "--stop-after-each"}}, []string{"ship", "-x", "--stop-after-each"}},
		{"library get", map[string]any{"title": dash}, []string{dash}},
		{"library activate", map[string]any{"title": dash}, []string{dash}},
		{"collection create", map[string]any{"name": dash}, []string{dash}},
		{"role create", map[string]any{"name": dash}, []string{dash}},
		{"workspace create", map[string]any{"name": dash}, []string{dash}},
		// A positional that is LITERALLY a flag this transport also emits
		// (lead ruling on BUG-3142): it must stay text, and must not steal or
		// override the real --workspace.
		{"item search", map[string]any{"query": "--workspace"}, []string{"--workspace"}},
	}

	for _, r := range rows {
		t.Run(r.path+"/"+r.want[len(r.want)-1], func(t *testing.T) {
			info, ok := doc.Commands[r.path]
			if !ok {
				t.Fatalf("cmdhelp has no %q command", r.path)
			}
			args, err := mcp.BuildCLIArgs(info, r.input, "session-ws", nil)
			if err != nil {
				t.Fatalf("BuildCLIArgs: %v", err)
			}
			cmd, rest, err := root.Find(strings.Fields(r.path))
			if err != nil || len(rest) != 0 {
				t.Fatalf("root.Find(%q): %v, rest %v", r.path, err, rest)
			}
			// The command's own parser, persistent flags merged in — exactly
			// what cobra runs before the command's RunE.
			if err := cmd.ParseFlags(args); err != nil {
				t.Fatalf("the command refused its own argv %q: %v", args, err)
			}
			if got := cmd.Flags().Args(); !slices.Equal(got, r.want) {
				t.Fatalf("positionals = %q, want %q (argv %q)", got, r.want, args)
			}
			if ws, _ := cmd.Flags().GetString("workspace"); ws != "session-ws" {
				t.Fatalf("--workspace = %q, want the session default (argv %q)", ws, args)
			}
		})
	}
}
