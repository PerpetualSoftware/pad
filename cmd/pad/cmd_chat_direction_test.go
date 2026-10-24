package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/cmdhelp"
	"github.com/PerpetualSoftware/pad/internal/mcp"
)

func chatSessionEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	socket := filepath.Join(t.TempDir(), "sock")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)
	if _, err := cli.WriteArmState(); err != nil {
		t.Fatal(err)
	}
}
func TestChatArmRequiresFlag(t *testing.T) {
	chatSessionEnv(t)
	cmd := newChatArmCmd()
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "pad session arm") {
		t.Fatal(err)
	}
	if cli.ChatDirectionOn() {
		t.Fatal("bare arm wrote")
	}
}
func TestChatArmPrintsWarningEveryTime(t *testing.T) {
	chatSessionEnv(t)
	old := formatFlag
	t.Cleanup(func() { formatFlag = old })
	for _, format := range []string{"text", "text", "json"} {
		formatFlag = format
		cmd := newChatArmCmd()
		var errout bytes.Buffer
		cmd.SetErr(&errout)
		cmd.SetArgs([]string{"--accept-instructions"})
		out := captureStdout(t, func() {
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
		for _, phrase := range []string{"instructions sent through Pad chat will drive this agent", "Pad does not authenticate the sender", "any process holding your Pad credential can send as you", "this session only", "pad session arm", "confirmed in-session"} {
			if !strings.Contains(errout.String(), phrase) {
				t.Fatalf("missing %q: %s", phrase, errout.String())
			}
		}
		if format == "json" {
			var v map[string]any
			if err := json.Unmarshal([]byte(out), &v); err != nil {
				t.Fatal(err)
			}
			if v["warning"] != strings.TrimSpace(errout.String()) {
				t.Fatal(out)
			}
		}
		if !cli.ChatDirectionOn() {
			t.Fatal("readback off")
		}
	}
}
func TestChatArmHeadlessRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", "")
	t.Setenv("CLAUDE_PID", "1")
	cmd := newChatArmCmd()
	cmd.SetArgs([]string{"--accept-instructions"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("headless allowed")
	}
	if cli.ChatDirectionOn() {
		t.Fatal("headless on")
	}
}
func TestChatDisarmLeavesPushArm(t *testing.T) {
	chatSessionEnv(t)
	cli.WriteChatDirection()
	if err := newChatDisarmCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	if cli.ChatDirectionOn() || cli.SessionArmState() != cli.LocalArmOn {
		t.Fatal("wrong flags")
	}
}
func TestSessionDisarmClearsDirection(t *testing.T) {
	chatSessionEnv(t)
	cli.WriteChatDirection()
	if err := sessionDisarmCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	cli.WriteArmState()
	if cli.ChatDirectionOn() {
		t.Fatal("direction revived")
	}
}
func TestSessionResetClearsDirection(t *testing.T) {
	chatSessionEnv(t)
	cli.WriteChatDirection()
	if err := sessionResetCmd().Execute(); err != nil {
		t.Fatal(err)
	}
	cli.WriteArmState()
	if cli.ChatDirectionOn() {
		t.Fatal("direction revived")
	}
}
func TestChatArmHasNoMCPPath(t *testing.T) {
	root := newRootCmd()
	doc := cmdhelp.Build(root, root, cmdhelp.Options{Binary: "pad", MaxDepth: -1})
	for _, path := range []string{"chat arm", "chat disarm"} {
		if _, ok := doc.Commands[path]; ok {
			t.Errorf("unwired constructor is exposed as a command: %s", path)
		}
	}
	server := mcp.NewServer(mcp.Options{})
	if _, err := mcp.Register(server.MCP(), mcp.RegistryOptions{
		Doc: doc, Workspace: mcp.NewSharedWorkspaceState(), Dispatcher: &recordingDispatcher{},
	}); err != nil {
		t.Fatal(err)
	}
	tools := server.MCP().ListTools()
	if len(tools) == 0 {
		t.Fatal("MCP projection must enumerate real tools")
	}
	for name, tool := range tools {
		if name != "pad_chat" {
			continue
		}
		// Inspect the schema actually advertised to MCP, rather than source
		// strings or a catalog entry that may not be registered.
		data, err := json.Marshal(tool.Tool.InputSchema.Properties["action"])
		if err != nil {
			t.Fatal(err)
		}
		var action struct {
			Enum []string `json:"enum"`
		}
		if err := json.Unmarshal(data, &action); err != nil {
			t.Fatal(err)
		}
		for _, value := range action.Enum {
			if value == "arm" || value == "disarm" {
				t.Errorf("MCP exposes %s.%s", name, value)
			}
		}
	}
	// Also inspect the catalog's public tool/action descriptor projection.
	data, err := mcp.ToolSurfaceJSON()
	if err != nil {
		t.Fatal(err)
	}
	var surface struct {
		Tools []struct {
			Name    string
			Actions []struct{ Name string }
		}
	}
	if err := json.Unmarshal(data, &surface); err != nil {
		t.Fatal(err)
	}
	if len(surface.Tools) == 0 {
		t.Fatal("empty catalog tool projection")
	}
	for _, tool := range surface.Tools {
		for _, action := range tool.Actions {
			if tool.Name == "pad_chat" && (action.Name == "arm" || action.Name == "disarm") {
				t.Errorf("tool surface exposes %s.%s", tool.Name, action.Name)
			}
		}
	}
}
