package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildTestRoot returns a minimal cobra tree that mirrors `pad`'s structure
// closely enough to exercise the help routing layer:
//
//	root
//	├── item (group)
//	│   ├── create
//	│   └── update
//	└── project (group)
//	    └── dashboard
//
// Using a synthetic root keeps the routing test independent of the real
// command tree (which can change frequently) and avoids spinning up the
// HTTP server that real subcommands need.
func buildTestRoot() *cobra.Command {
	root := &cobra.Command{Use: "padtest", Short: "test root"}

	itemGroup := &cobra.Command{Use: "item", Short: "item group"}
	itemGroup.AddCommand(
		&cobra.Command{Use: "create", Short: "create an item"},
		&cobra.Command{Use: "update", Short: "update an item"},
	)
	projectGroup := &cobra.Command{Use: "project", Short: "project group"}
	projectGroup.AddCommand(
		&cobra.Command{Use: "dashboard", Short: "show dashboard"},
	)

	root.AddCommand(itemGroup, projectGroup)
	root.SetHelpCommand(helpCmd())
	return root
}

// runHelp invokes the help command with the given args and returns
// (stdout, stderr, error).
func runHelp(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	root := buildTestRoot()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"help"}, args...))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestHelpCmd_DefaultTextRoutesToCobra(t *testing.T) {
	out, _, err := runHelp(t)
	if err != nil {
		t.Fatalf("pad help: unexpected error: %v", err)
	}
	if !strings.Contains(out, "Available Commands:") {
		t.Errorf("expected cobra-style text output with 'Available Commands:', got:\n%s", out)
	}
	for _, name := range []string{"item", "project"} {
		if !strings.Contains(out, name) {
			t.Errorf("expected root help to list subcommand %q, got:\n%s", name, out)
		}
	}
}

func TestHelpCmd_GroupScope(t *testing.T) {
	out, _, err := runHelp(t, "item")
	if err != nil {
		t.Fatalf("pad help item: unexpected error: %v", err)
	}
	if !strings.Contains(out, "create") || !strings.Contains(out, "update") {
		t.Errorf("expected `pad help item` to list child commands, got:\n%s", out)
	}
}

func TestHelpCmd_LeafScope(t *testing.T) {
	out, _, err := runHelp(t, "item", "create")
	if err != nil {
		t.Fatalf("pad help item create: unexpected error: %v", err)
	}
	if !strings.Contains(out, "create an item") {
		t.Errorf("expected leaf help to include leaf description, got:\n%s", out)
	}
}

func TestHelpCmd_FormatTextEquivalent(t *testing.T) {
	defaultOut, _, _ := runHelp(t)
	explicitOut, _, err := runHelp(t, "--format", "text")
	if err != nil {
		t.Fatalf("pad help --format text: unexpected error: %v", err)
	}
	if defaultOut == "" || explicitOut == "" {
		t.Fatalf("expected non-empty output from both default and --format=text")
	}
	if defaultOut != explicitOut {
		t.Errorf("expected --format=text to match default output exactly\n--- default ---\n%s\n--- explicit ---\n%s", defaultOut, explicitOut)
	}
}

func TestHelpCmd_FormatJSONEmits(t *testing.T) {
	// TASK-934 replaced the stub with a real emitter. Verify --format json
	// produces a parseable cmdhelp document with the expected envelope.
	out, _, err := runHelp(t, "--format", "json")
	if err != nil {
		t.Fatalf("--format json: unexpected error: %v\nstdout:\n%s", err, out)
	}
	var doc map[string]interface{}
	if uerr := json.Unmarshal([]byte(out), &doc); uerr != nil {
		t.Fatalf("--format json output is not valid JSON: %v\n%s", uerr, out)
	}
	if doc["cmdhelp_version"] != "0.1" {
		t.Errorf("cmdhelp_version = %v, want 0.1", doc["cmdhelp_version"])
	}
	if _, ok := doc["binary"]; !ok {
		t.Errorf("emitted JSON missing required key 'binary'")
	}
	cmds, ok := doc["commands"].(map[string]interface{})
	if !ok {
		t.Fatalf("emitted JSON missing required key 'commands' or wrong type: %T", doc["commands"])
	}
	for _, want := range []string{"item", "item create", "project", "project dashboard"} {
		if _, ok := cmds[want]; !ok {
			t.Errorf("expected command %q in emitted commands map", want)
		}
	}
}

func TestHelpCmd_FormatMarkdownStubError(t *testing.T) {
	for _, format := range []string{"md", "llm"} {
		t.Run(format, func(t *testing.T) {
			_, _, err := runHelp(t, "--format", format)
			if err == nil {
				t.Fatalf("expected --format %s to return a stub error (TASK-935 unimplemented)", format)
			}
			msg := err.Error()
			if !strings.Contains(msg, "TASK-935") {
				t.Errorf("expected --format %s stub error to reference TASK-935, got: %s", format, msg)
			}
		})
	}
}

func TestHelpCmd_FormatLLMAliasRoutesToMarkdown(t *testing.T) {
	// Both md and llm should produce the same error message body (modulo
	// "TASK-935") since llm is a renderer-level alias for md.
	_, _, mdErr := runHelp(t, "--format", "md")
	_, _, llmErr := runHelp(t, "--format", "llm")
	if mdErr == nil || llmErr == nil {
		t.Fatalf("expected both --format md and --format llm to return stub errors")
	}
	if mdErr.Error() != llmErr.Error() {
		t.Errorf("expected --format llm to alias --format md (same stub error)\nmd:  %s\nllm: %s", mdErr.Error(), llmErr.Error())
	}
}

func TestHelpCmd_FormatUnknownRejected(t *testing.T) {
	_, _, err := runHelp(t, "--format", "yaml")
	if err == nil {
		t.Fatalf("expected --format yaml to be rejected as unknown")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown") || !strings.Contains(msg, "yaml") {
		t.Errorf("expected unknown-format error to mention the bad value, got: %s", msg)
	}
}

func TestHelpCmd_UnknownTopicRejected(t *testing.T) {
	_, _, err := runHelp(t, "no-such-command")
	if err == nil {
		t.Fatalf("expected unknown topic to return an error")
	}
	if !strings.Contains(err.Error(), "unknown help topic") {
		t.Errorf("expected error to mention 'unknown help topic', got: %s", err.Error())
	}
}

func TestHelpCmd_DepthAndAllAccepted(t *testing.T) {
	// --depth and --all are part of the cmdhelp v0.1 surface but their
	// effect lives in the JSON/MD emitters (TASK-934/935). At this layer
	// we just need to confirm they're plumbed through and don't error.
	if _, _, err := runHelp(t, "--depth", "2"); err != nil {
		t.Errorf("--depth 2 should be accepted at default text format, got: %v", err)
	}
	if _, _, err := runHelp(t, "--all"); err != nil {
		t.Errorf("--all should be accepted at default text format, got: %v", err)
	}
}

func TestCmdhelpVersion_WireFormat(t *testing.T) {
	// Per cmdhelp v0.1 §9, the wire-format version is MAJOR.MINOR — never
	// includes a PATCH component. Lock that here so a stray "0.1.0" can't
	// land without breaking this test.
	parts := strings.Split(CmdhelpVersion, ".")
	if len(parts) != 2 {
		t.Errorf("CmdhelpVersion %q must be MAJOR.MINOR (no PATCH); got %d parts", CmdhelpVersion, len(parts))
	}
	for i, p := range parts {
		if p == "" {
			t.Errorf("CmdhelpVersion %q has empty component at index %d", CmdhelpVersion, i)
		}
	}
}
