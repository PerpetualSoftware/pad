package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPInstallCompactResultsUsesClientVisibleChannel(t *testing.T) {
	tests := []struct {
		agent string
		path  string
		want  string
		drop  string
	}{
		{agent: "cursor", path: filepath.Join(".cursor", "mcp.json"), want: "--text-only", drop: "--structured-only"},
		{agent: "codex", path: filepath.Join(".codex", "config.toml"), want: "--structured-only", drop: "--text-only"},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			cmd := mcpInstallCmd()
			cmd.SetArgs([]string{tt.agent, "--compact-results"})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			body, err := os.ReadFile(filepath.Join(home, tt.path))
			if err != nil {
				t.Fatalf("read installed config: %v", err)
			}
			if !strings.Contains(string(body), tt.want) || strings.Contains(string(body), tt.drop) {
				t.Fatalf("installed config = %s; want %s and not %s", body, tt.want, tt.drop)
			}
		})
	}
}

func TestMCPInstallCompactResultsRejectsUnsupportedClient(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := mcpInstallCmd()
	cmd.SetArgs([]string{"claude-desktop", "--compact-results"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "supported only for cursor and codex") {
		t.Fatalf("Execute error = %v, want unsupported-client error", err)
	}
}

func TestMCPServeRejectsConflictingResultModes(t *testing.T) {
	cmd := mcpServeCmd()
	cmd.SetArgs([]string{"--structured-only", "--text-only"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("Execute error = %v, want mutually exclusive modes", err)
	}
}
