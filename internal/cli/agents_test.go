package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveTool(t *testing.T) {
	tests := []struct {
		input    string
		wantName string
		wantNil  bool
	}{
		{"claude", "claude", false},
		{"cursor", "agents", false},
		{"codex", "agents", false},
		{"windsurf", "agents", false},
		{"opencode", "agents", false},
		{"agents", "agents", false},
		{"copilot", "copilot", false},
		{"amazon-q", "amazon-q", false},
		{"amazonq", "amazon-q", false},
		{"junie", "junie", false},
		{"CLAUDE", "claude", false},
		{"Cursor", "agents", false},
		{"nonexistent", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			tool := ResolveTool(tt.input)
			if tt.wantNil {
				if tool != nil {
					t.Errorf("ResolveTool(%q) = %v, want nil", tt.input, tool)
				}
				return
			}
			if tool == nil {
				t.Fatalf("ResolveTool(%q) = nil, want %q", tt.input, tt.wantName)
			}
			if tool.Name != tt.wantName {
				t.Errorf("ResolveTool(%q).Name = %q, want %q", tt.input, tool.Name, tt.wantName)
			}
		})
	}
}

func TestStripFrontmatter(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "with frontmatter",
			input: "---\nname: pad\ndescription: test\n---\n\n# Body\nContent here",
			want:  "# Body\nContent here",
		},
		{
			name:  "no frontmatter",
			input: "# Just a body\nNo frontmatter here",
			want:  "# Just a body\nNo frontmatter here",
		},
		{
			name:  "complex frontmatter",
			input: "---\nname: pad\nallowed-tools:\n  - Bash\n  - Read\n---\n\nBody text",
			want:  "Body text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(StripFrontmatter([]byte(tt.input)))
			if got != tt.want {
				t.Errorf("StripFrontmatter() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatForTool(t *testing.T) {
	embedded := []byte(`---
name: pad
description: "Test skill"
allowed-tools:
  - Bash
---

# Pad Skill

Body content here.
`)

	t.Run("claude returns original", func(t *testing.T) {
		tool := *ResolveTool("claude")
		got := FormatForTool(tool, embedded)
		if string(got) != string(embedded) {
			t.Error("Claude format should return embedded content unchanged")
		}
	})

	t.Run("agents has name+description frontmatter", func(t *testing.T) {
		tool := *ResolveTool("agents")
		got := string(FormatForTool(tool, embedded))
		if got[:4] != "---\n" {
			t.Error("Agents format should start with frontmatter")
		}
		if !contains(got, "name: pad") {
			t.Error("Agents format should contain name: pad")
		}
		if !contains(got, "description:") {
			t.Error("Agents format should contain description")
		}
		if contains(got, "allowed-tools") {
			t.Error("Agents format should NOT contain allowed-tools")
		}
		if !contains(got, "# Pad Skill") {
			t.Error("Agents format should contain body")
		}
	})

	t.Run("copilot has applyTo frontmatter", func(t *testing.T) {
		tool := *ResolveTool("copilot")
		got := string(FormatForTool(tool, embedded))
		if !contains(got, "applyTo:") {
			t.Error("Copilot format should contain applyTo")
		}
		if contains(got, "allowed-tools") {
			t.Error("Copilot format should NOT contain allowed-tools")
		}
	})

	t.Run("junie has no frontmatter", func(t *testing.T) {
		tool := *ResolveTool("junie")
		got := string(FormatForTool(tool, embedded))
		if contains(got, "---") {
			t.Error("Junie format should NOT contain frontmatter delimiters")
		}
		if !contains(got, "# Pad Skill") {
			t.Error("Junie format should contain body")
		}
	})
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// isolateEnv points HOME and PATH at empty temp directories and chdirs into
// an empty temp directory, so DetectTools sees none of this machine's real
// signals (this dev machine has ~/.codex, ~/.claude, etc. — a naive test
// would false-pass without this).
func isolateEnv(t *testing.T) (home, bin string) {
	t.Helper()
	home = t.TempDir()
	bin = t.TempDir()
	cwd := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin)
	t.Chdir(cwd)
	return home, bin
}

// writeStubBinary creates an executable file named `name` in dir, so
// exec.LookPath(name) succeeds against it.
func writeStubBinary(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write stub binary %s: %v", path, err)
	}
}

func detectedNames(tools []AgentTool) map[string]bool {
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	return names
}

func TestDetectTools(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("stub binaries and HOME semantics differ on windows")
	}

	t.Run("no signal miss", func(t *testing.T) {
		isolateEnv(t)
		got := DetectTools()
		if len(got) != 0 {
			t.Errorf("DetectTools() = %v, want empty", got)
		}
	})

	t.Run("home dir hit for agents via ~/.codex", func(t *testing.T) {
		home, _ := isolateEnv(t)
		if err := os.Mkdir(filepath.Join(home, ".codex"), 0o755); err != nil {
			t.Fatal(err)
		}
		names := detectedNames(DetectTools())
		if !names["agents"] {
			t.Errorf("expected agents detected via ~/.codex, got %v", names)
		}
		if names["claude"] {
			t.Errorf("claude should not be detected, got %v", names)
		}
	})

	t.Run("home dir hit for agents via ~/.opencode", func(t *testing.T) {
		home, _ := isolateEnv(t)
		if err := os.Mkdir(filepath.Join(home, ".opencode"), 0o755); err != nil {
			t.Fatal(err)
		}
		names := detectedNames(DetectTools())
		if !names["agents"] {
			t.Errorf("expected agents detected via ~/.opencode, got %v", names)
		}
		if names["claude"] {
			t.Errorf("claude should not be detected, got %v", names)
		}
	})

	t.Run("home dir hit for claude via ~/.claude", func(t *testing.T) {
		home, _ := isolateEnv(t)
		if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		names := detectedNames(DetectTools())
		if !names["claude"] {
			t.Errorf("expected claude detected via ~/.claude, got %v", names)
		}
		if names["agents"] {
			t.Errorf("agents should not be detected, got %v", names)
		}
	})

	t.Run("binary hit for agents via codex on PATH", func(t *testing.T) {
		_, bin := isolateEnv(t)
		writeStubBinary(t, bin, "codex")
		names := detectedNames(DetectTools())
		if !names["agents"] {
			t.Errorf("expected agents detected via codex binary, got %v", names)
		}
	})

	t.Run("binary hit for agents via opencode on PATH", func(t *testing.T) {
		_, bin := isolateEnv(t)
		writeStubBinary(t, bin, "opencode")
		names := detectedNames(DetectTools())
		if !names["agents"] {
			t.Errorf("expected agents detected via opencode binary, got %v", names)
		}
	})

	t.Run("binary hit for claude via claude on PATH", func(t *testing.T) {
		_, bin := isolateEnv(t)
		writeStubBinary(t, bin, "claude")
		names := detectedNames(DetectTools())
		if !names["claude"] {
			t.Errorf("expected claude detected via claude binary, got %v", names)
		}
	})

	t.Run("project-local dir still works for tools with no machine signal", func(t *testing.T) {
		isolateEnv(t)
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(cwd, ".junie"), 0o755); err != nil {
			t.Fatal(err)
		}
		names := detectedNames(DetectTools())
		if !names["junie"] {
			t.Errorf("expected junie detected via project-local .junie, got %v", names)
		}
		if len(names) != 1 {
			t.Errorf("expected only junie detected, got %v", names)
		}
	})

	t.Run("project-local and home dir both present dedupes to one entry", func(t *testing.T) {
		home, _ := isolateEnv(t)
		if err := os.Mkdir(filepath.Join(home, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(cwd, ".claude"), 0o755); err != nil {
			t.Fatal(err)
		}
		got := DetectTools()
		count := 0
		for _, tool := range got {
			if tool.Name == "claude" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("expected claude detected exactly once, got %d (in %v)", count, got)
		}
	})
}

func TestAllToolNames(t *testing.T) {
	names := AllToolNames()
	// Should include both canonical names and aliases
	expected := map[string]bool{
		"claude":   true,
		"agents":   true,
		"cursor":   true,
		"codex":    true,
		"opencode": true,
		"copilot":  true,
		"junie":    true,
	}
	nameMap := map[string]bool{}
	for _, n := range names {
		nameMap[n] = true
	}
	for name := range expected {
		if !nameMap[name] {
			t.Errorf("AllToolNames() missing %q", name)
		}
	}
}
