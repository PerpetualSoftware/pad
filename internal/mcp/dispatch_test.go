package mcp

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
)

func TestBuildCLIArgs_PositionalsAndScalarFlags(t *testing.T) {
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{
			{Name: "collection", Type: "string", Required: true},
			{Name: "title", Type: "string", Required: true},
		},
		Flags: map[string]cmdhelp.Flag{
			"priority": {Type: "string"},
			"stdin":    {Type: "bool"},
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{
		"collection": "tasks",
		"title":      "Fix OAuth",
		"priority":   "high",
		"stdin":      false, // false bool should be omitted
	}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{
		"tasks", "Fix OAuth",
		"--priority", "high",
		"--format", "json",
	}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_BoolPresenceForm(t *testing.T) {
	// Spec §5.3: bool flags MUST be presence-only (no =true).
	// Here `stdin: true` should emit just `--stdin`, no value.
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{"stdin": {Type: "bool"}},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"stdin": true}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--stdin", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_RequiredArgMissingErrors(t *testing.T) {
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "collection", Type: "string", Required: true}},
	}
	_, err := BuildCLIArgs(cmd, map[string]any{}, "")
	if err == nil {
		t.Errorf("expected error when required arg missing")
	}
	if err != nil && !strings.Contains(err.Error(), "collection") {
		t.Errorf("error should mention the missing arg name; got: %v", err)
	}
}

func TestBuildCLIArgs_OptionalArgMissingOK(t *testing.T) {
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "filter", Type: "string"}}, // not required
	}
	got, err := BuildCLIArgs(cmd, map[string]any{}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	// No positional, no flags set → just the format injection.
	want := []string{"--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_InjectsSessionWorkspace(t *testing.T) {
	got, err := BuildCLIArgs(cmdhelp.Command{}, map[string]any{}, "docapp")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--workspace", "docapp", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_ExplicitWorkspaceOverridesSession(t *testing.T) {
	// When the client passes workspace explicitly, the session
	// default must NOT be appended — otherwise we'd send two
	// --workspace flags and pflag would take whichever cobra picked
	// last. The contract: client wins.
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{"workspace": {Type: "string"}},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"workspace": "client-ws"}, "session-ws")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	wsCount := 0
	wsValue := ""
	for i, a := range got {
		if a == "--workspace" && i+1 < len(got) {
			wsCount++
			wsValue = got[i+1]
		}
	}
	if wsCount != 1 {
		t.Errorf("expected exactly 1 --workspace flag, got %d in %v", wsCount, got)
	}
	if wsValue != "client-ws" {
		t.Errorf("client-supplied value should win; got %q", wsValue)
	}
}

func TestBuildCLIArgs_RepeatableArgExpands(t *testing.T) {
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "ref", Type: "string", Repeatable: true, Required: true}},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"ref": []any{"TASK-5", "TASK-8"}}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"TASK-5", "TASK-8", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_RepeatableFlag(t *testing.T) {
	// `--field key=value` repeated for each entry per pflag's
	// stringSlice convention (also covers cmdhelp's `repeatable: true`
	// annotation on a scalar string flag).
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{
			"field": {Type: "string", Repeatable: true},
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"field": []any{"a=1", "b=2"}}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--field", "a=1", "--field", "b=2", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_StringSliceTypeAlsoRepeats(t *testing.T) {
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{
			"tag": {Type: "[]string"}, // pflag-style slice type
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"tag": []any{"x", "y"}}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--tag", "x", "--tag", "y", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_ExplicitFormatOverridesDefault(t *testing.T) {
	// Agents that want non-JSON output (e.g. markdown for human
	// rendering) must be able to override.
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{"format": {Type: "string"}},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"format": "markdown"}, "")
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	formatCount := 0
	for _, a := range got {
		if a == "--format" {
			formatCount++
		}
	}
	if formatCount != 1 {
		t.Errorf("expected single --format, got %d in %v", formatCount, got)
	}
	// Last value wins for pflag — confirm "markdown" survives.
	for i, a := range got {
		if a == "--format" && i+1 < len(got) {
			if got[i+1] != "markdown" {
				t.Errorf("--format should be 'markdown', got %q", got[i+1])
			}
		}
	}
}

func TestExecDispatcher_NoBinary_ReturnsErrorResult(t *testing.T) {
	// Fail-fast: misconfigured dispatcher must return an IsError
	// result, not panic or return a transport-level error.
	d := &ExecDispatcher{}
	res, err := d.Dispatch(t.Context(), []string{"item", "list"}, []string{"--format", "json"})
	if err != nil {
		t.Fatalf("Dispatch returned err: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result")
	}
}

func TestExecDispatcher_RunsBinaryAndReturnsStdout(t *testing.T) {
	// Use /bin/sh as a stand-in pad binary — it accepts arbitrary
	// args and prints something predictable, so we can verify
	// stdout-routing without involving a real pad subprocess.
	d := &ExecDispatcher{Binary: "/bin/sh"}
	res, err := d.Dispatch(t.Context(),
		[]string{"-c"},
		[]string{`echo '{"items": []}'`},
	)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.IsError {
		t.Errorf("unexpected IsError; result: %+v", res)
	}
	// JSON stdout should populate StructuredContent (string-fallback
	// path is fine too — we just want to assert it's not error and
	// stdout was captured somewhere).
	if res.StructuredContent == nil && len(res.Content) == 0 {
		t.Errorf("neither StructuredContent nor Content was populated")
	}
}

func TestExecDispatcher_NonzeroExitReturnsErrorResult(t *testing.T) {
	d := &ExecDispatcher{Binary: "/bin/sh"}
	res, err := d.Dispatch(t.Context(),
		[]string{"-c"},
		[]string{`echo "boom" >&2 && exit 7`},
	)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !res.IsError {
		t.Errorf("expected IsError result for non-zero exit")
	}
}
