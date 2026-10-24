package main

import (
	"bytes"
	"encoding/json"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/mcp"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, tool := range mcp.Catalog {
		if tool.Name == "pad_chat" {
			for _, action := range []string{"arm", "disarm"} {
				if _, ok := tool.Actions[action]; ok {
					t.Fatalf("MCP exposes %s.%s", tool.Name, action)
				}
			}
		}
	}
	paths, err := filepath.Glob("../../internal/mcp/catalog*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "accept-instructions") || strings.Contains(string(b), "chat arm") {
			t.Fatal(p)
		}
	}
}
