package main

import (
	"reflect"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	mcpserver "github.com/PerpetualSoftware/pad/internal/mcp"
)

func TestBUG4056BuildCLIArgs(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
	for _, tc := range []struct {
		name, command string
		input         map[string]any
		want          []string
	}{
		{"flag", "chat read", map[string]any{"target": "C12", "to": float64(1000000)}, []string{"--to", "1000000", "--format", "json", "--", "C12"}},
		{"positional", "chat delete", map[string]any{"target": "C12", "seq": float64(1000000)}, []string{"--format", "json", "--", "C12", "1000000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok := doc.Commands[tc.command]
			if !ok {
				t.Fatalf("catalog command %q missing from cmdhelp", tc.command)
			}
			got, err := mcpserver.BuildCLIArgs(cmd, tc.input, "", nil)
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("BuildCLIArgs = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
