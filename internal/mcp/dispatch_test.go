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
	}, "", nil)
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
	// MCP delivers the snake_case `dry_run: true` (TASK-964); the
	// dispatcher translates back to the kebab-case `--dry-run` CLI flag.
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{"dry-run": {Type: "bool"}},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"dry_run": true}, "", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--dry-run", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_HyphenatedFlagAcceptedAsSnakeCase(t *testing.T) {
	// TASK-964: the round-trip contract. MCP property names are
	// snake_case; CLI flag names stay kebab-case. A scalar
	// hyphenated flag like `--due-date` must surface to agents as
	// `due_date` and round-trip back to `--due-date value` on dispatch.
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "ref", Type: "string", Required: true}},
		Flags: map[string]cmdhelp.Flag{
			"due-date":   {Type: "string"},
			"blocked-by": {Type: "string"},
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{
		"ref":        "TASK-5",
		"due_date":   "2026-06-01",
		"blocked_by": "TASK-3",
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	// Sorted-flag order: blocked-by < due-date.
	want := []string{
		"TASK-5",
		"--blocked-by", "TASK-3",
		"--due-date", "2026-06-01",
		"--format", "json",
	}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_HyphenatedRepeatableFlagRoundTrips(t *testing.T) {
	// TASK-964: Repeatable / slice flags also need the translation.
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{
			"add-tag": {Type: "stringSlice", Repeatable: true},
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{
		"add_tag": []any{"urgent", "ux"},
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--add-tag", "urgent", "--add-tag", "ux", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_KebabInputKeyIsIgnored(t *testing.T) {
	// TASK-964: agents that bypass the schema and send the kebab-case
	// form (`due-date`) shouldn't accidentally provide a value — they
	// passed an unknown property as far as the schema is concerned.
	// We don't error, we just skip it (consistent with how unknown
	// optional flags are handled today).
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{
			"due-date": {Type: "string"},
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{
		"due-date": "2026-06-01", // wrong: should be `due_date`
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	for _, a := range got {
		if strings.Contains(a, "2026-06-01") {
			t.Errorf("kebab-case input key should not pass through; got %v", got)
		}
	}
}

func TestBuildCLIArgs_StdinFlagDroppedDefensively(t *testing.T) {
	// Even if an agent sends stdin: true (perhaps from a stale
	// schema cache), BuildCLIArgs MUST drop it — the dispatcher
	// can't pipe agent stdin to the subprocess, so passing
	// --stdin would block on EOF and create empty content.
	// `--content` is the agent-friendly equivalent.
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{
			"stdin":   {Type: "bool"},
			"content": {Type: "string"},
		},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{
		"stdin":   true,
		"content": "hello world",
	}, "", nil)
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	for _, a := range got {
		if a == "--stdin" {
			t.Errorf("stdin flag should be dropped, got: %v", got)
		}
	}
	want := []string{"--content", "hello world", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_RootFlagsInjected(t *testing.T) {
	// --url at the root level should be forwarded to every dispatched
	// subprocess. Without this, MCP clients configured as
	// `pad --url X mcp serve` lose the URL on every tool call.
	got, err := BuildCLIArgs(cmdhelp.Command{}, map[string]any{}, "",
		map[string]string{"url": "https://api.example.com"})
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	want := []string{"--url", "https://api.example.com", "--format", "json"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildCLIArgs_RootFlagsEmptyValueSkipped(t *testing.T) {
	// Server startup passes `map[string]string{"url": urlFlag}`
	// unconditionally — when the user didn't pass --url at startup,
	// urlFlag is empty and we must NOT inject `--url ""`.
	got, err := BuildCLIArgs(cmdhelp.Command{}, map[string]any{}, "",
		map[string]string{"url": ""})
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	for _, a := range got {
		if a == "--url" {
			t.Errorf("empty root-flag value should be skipped, got: %v", got)
		}
	}
}

func TestBuildCLIArgs_RootFlagsDoNotOverrideExplicitInput(t *testing.T) {
	// If the agent explicitly passes `url` in the tool args, the
	// agent's value wins — startup-captured root flag is NOT
	// appended (would produce two --url flags; pflag picks last,
	// behaviour gets surprising).
	cmd := cmdhelp.Command{
		Flags: map[string]cmdhelp.Flag{"url": {Type: "string"}},
	}
	got, err := BuildCLIArgs(cmd, map[string]any{"url": "agent-url"}, "",
		map[string]string{"url": "startup-url"})
	if err != nil {
		t.Fatalf("BuildCLIArgs: %v", err)
	}
	count := 0
	for i, a := range got {
		if a == "--url" && i+1 < len(got) {
			count++
			if got[i+1] != "agent-url" {
				t.Errorf("agent value should win; got %q", got[i+1])
			}
		}
	}
	if count != 1 {
		t.Errorf("expected exactly 1 --url, got %d in %v", count, got)
	}
}

func TestBuildCLIArgs_RequiredArgMissingErrors(t *testing.T) {
	cmd := cmdhelp.Command{
		Args: []cmdhelp.Arg{{Name: "collection", Type: "string", Required: true}},
	}
	_, err := BuildCLIArgs(cmd, map[string]any{}, "", nil)
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
	got, err := BuildCLIArgs(cmd, map[string]any{}, "", nil)
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
	got, err := BuildCLIArgs(cmdhelp.Command{}, map[string]any{}, "docapp", nil)
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
	got, err := BuildCLIArgs(cmd, map[string]any{"workspace": "client-ws"}, "session-ws", nil)
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
	got, err := BuildCLIArgs(cmd, map[string]any{"ref": []any{"TASK-5", "TASK-8"}}, "", nil)
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
	got, err := BuildCLIArgs(cmd, map[string]any{"field": []any{"a=1", "b=2"}}, "", nil)
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
	got, err := BuildCLIArgs(cmd, map[string]any{"tag": []any{"x", "y"}}, "", nil)
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
	got, err := BuildCLIArgs(cmd, map[string]any{"format": "markdown"}, "", nil)
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
