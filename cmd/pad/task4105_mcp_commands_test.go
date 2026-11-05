package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

// TASK-4105: pad_item's schedule actions dispatch to `pad item remind` and its
// subcommands on stdio, and BuildCLIArgs emits only the positionals and flags
// the command declares. A flag renamed or removed here would leave the
// matching MCP parameter silently dropped, so each one MCP sends is pinned
// against the real command tree.
func TestScheduleCommandsExposeWhatMCPSends(t *testing.T) {
	doc := cmdhelp.Build(newRootCmd(), newRootCmd(), cmdhelp.Options{MaxDepth: -1})
	schedule := []string{"every", "start", "deadline", "window", "anchor", "audience", "tz", "floating"}
	resolve := []string{"comment", "occurrence"}
	for _, tc := range []struct {
		path  string
		args  []string
		flags []string
	}{
		{"item remind", []string{"ref"}, append([]string{"remind-at"}, schedule...)},
		{"item remind show", []string{"ref"}, nil},
		{"item remind edit", []string{"ref"}, schedule},
		{"item remind rm", []string{"ref"}, []string{"yes"}},
		{"item remind done", []string{"ref"}, resolve},
		{"item remind skip", []string{"ref"}, resolve},
		{"item remind snooze", []string{"ref"}, append([]string{"until"}, resolve...)},
		{"item remind move", []string{"ref"}, append([]string{"until"}, resolve...)},
		{"item remind agenda", nil, []string{"from", "to", "tz"}},
	} {
		cmd, ok := doc.Commands[tc.path]
		if !ok {
			t.Errorf("%q is missing from cmdhelp", tc.path)
			continue
		}
		var args []string
		for _, a := range cmd.Args {
			args = append(args, a.Name)
		}
		if len(args) != len(tc.args) || (len(args) == 1 && args[0] != tc.args[0]) {
			t.Errorf("%q positionals = %v, want %v", tc.path, args, tc.args)
		}
		for _, f := range tc.flags {
			if _, ok := cmd.Flags[f]; !ok {
				t.Errorf("%q has no --%s flag; the MCP parameter maps to nothing", tc.path, f)
			}
		}
		if f, ok := cmd.Flags["floating"]; ok && f.Type != "bool" {
			t.Errorf("%q --floating is %q, want bool", tc.path, f.Type)
		}
	}
}
