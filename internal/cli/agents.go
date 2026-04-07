package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AgentTool describes a supported AI coding tool and how to install the Pad skill for it.
type AgentTool struct {
	// Name is the canonical identifier (e.g., "claude", "agents", "copilot").
	Name string
	// Label is a human-readable name for display.
	Label string
	// Aliases are alternative names users can type (e.g., "cursor" → "agents").
	Aliases []string
	// DetectDirs are directories whose presence suggests this tool is in use.
	DetectDirs []string
	// SkillDir is the relative path from project root for the skill file directory.
	SkillDir string
	// SkillFile is the filename within SkillDir.
	SkillFile string
}

// SupportedTools lists all supported agent tools.
// The "agents" target covers Codex, Cursor, and Windsurf via the shared .agents/skills/ directory.
var SupportedTools = []AgentTool{
	{
		Name:       "claude",
		Label:      "Claude Code",
		Aliases:    nil,
		DetectDirs: []string{".claude"},
		SkillDir:   filepath.Join(".claude", "skills", "pad"),
		SkillFile:  "SKILL.md",
	},
	{
		Name:       "agents",
		Label:      "Codex / Cursor / Windsurf",
		Aliases:    []string{"codex", "cursor", "windsurf"},
		DetectDirs: []string{".cursor", ".windsurf", ".codex", ".agents"},
		SkillDir:   filepath.Join(".agents", "skills", "pad"),
		SkillFile:  "SKILL.md",
	},
	{
		Name:       "copilot",
		Label:      "GitHub Copilot",
		Aliases:    []string{"github-copilot"},
		DetectDirs: []string{filepath.Join(".github", "copilot"), filepath.Join(".github", "instructions")},
		SkillDir:   filepath.Join(".github", "instructions"),
		SkillFile:  "pad.instructions.md",
	},
	{
		Name:       "amazon-q",
		Label:      "Amazon Q",
		Aliases:    []string{"amazonq", "q"},
		DetectDirs: []string{".amazonq"},
		SkillDir:   filepath.Join(".amazonq", "rules"),
		SkillFile:  "pad.md",
	},
	{
		Name:       "junie",
		Label:      "JetBrains Junie",
		Aliases:    nil,
		DetectDirs: []string{".junie"},
		SkillDir:   filepath.Join(".junie", "guidelines"),
		SkillFile:  "pad.md",
	},
}

// ResolveTool finds an AgentTool by name or alias. Returns nil if not found.
func ResolveTool(nameOrAlias string) *AgentTool {
	lower := strings.ToLower(nameOrAlias)
	for i := range SupportedTools {
		t := &SupportedTools[i]
		if t.Name == lower {
			return t
		}
		for _, alias := range t.Aliases {
			if alias == lower {
				return t
			}
		}
	}
	return nil
}

// DetectTools returns the tools that appear to be in use based on directory presence.
func DetectTools() []AgentTool {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}

	var detected []AgentTool
	seen := map[string]bool{}
	for _, tool := range SupportedTools {
		if seen[tool.Name] {
			continue
		}
		for _, dir := range tool.DetectDirs {
			info, err := os.Stat(filepath.Join(cwd, dir))
			if err == nil && info.IsDir() {
				detected = append(detected, tool)
				seen[tool.Name] = true
				break
			}
		}
	}
	return detected
}

// ToolSkillPath returns the full path where a tool's skill file would be installed.
func ToolSkillPath(tool AgentTool) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return filepath.Join(cwd, tool.SkillDir, tool.SkillFile)
}

// ToolInstalled checks if the skill file exists for the given tool.
func ToolInstalled(tool AgentTool) bool {
	path := ToolSkillPath(tool)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// InstallForTool writes the skill content to the appropriate location for a tool.
func InstallForTool(tool AgentTool, content []byte) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}

	skillDir := filepath.Join(cwd, tool.SkillDir)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", fmt.Errorf("create directory %s: %w", skillDir, err)
	}

	destPath := filepath.Join(skillDir, tool.SkillFile)
	if err := os.WriteFile(destPath, content, 0644); err != nil {
		return "", fmt.Errorf("write skill file: %w", err)
	}

	return destPath, nil
}

// StripFrontmatter removes YAML frontmatter (between --- delimiters) from content.
func StripFrontmatter(content []byte) []byte {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return content
	}
	idx := strings.Index(s[4:], "\n---\n")
	if idx < 0 {
		// Try --- at end of file
		idx = strings.Index(s[4:], "\n---")
		if idx < 0 {
			return content
		}
		return []byte(strings.TrimLeft(s[4+idx+4:], "\n"))
	}
	return []byte(strings.TrimLeft(s[4+idx+5:], "\n"))
}

// FormatForTool takes the raw embedded skill content and formats it for a specific tool.
// Returns the appropriately formatted content with tool-specific frontmatter.
func FormatForTool(tool AgentTool, embeddedContent []byte) []byte {
	switch tool.Name {
	case "claude":
		// Claude Code uses the embedded content as-is (it already has the right frontmatter)
		return embeddedContent

	case "agents":
		// Codex/Cursor/Windsurf use name + description frontmatter
		body := StripFrontmatter(embeddedContent)
		fm := `---
name: pad
description: "Talk to your project. Natural-language project management — create items, check status, create plans, brainstorm ideas, and more."
---

`
		return append([]byte(fm), body...)

	case "copilot":
		// GitHub Copilot uses applyTo frontmatter
		body := StripFrontmatter(embeddedContent)
		fm := `---
applyTo: "**"
---

`
		return append([]byte(fm), body...)

	default:
		// Amazon Q, Junie, and others: no frontmatter, just the body
		return StripFrontmatter(embeddedContent)
	}
}

// AllToolNames returns all valid names and aliases that can be passed to `pad agent install`.
func AllToolNames() []string {
	var names []string
	for _, t := range SupportedTools {
		names = append(names, t.Name)
		names = append(names, t.Aliases...)
	}
	return names
}
