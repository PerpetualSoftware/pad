package cli

import (
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

func TestAllToolNames(t *testing.T) {
	names := AllToolNames()
	// Should include both canonical names and aliases
	expected := map[string]bool{
		"claude":  true,
		"agents":  true,
		"cursor":  true,
		"codex":   true,
		"copilot": true,
		"junie":   true,
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
