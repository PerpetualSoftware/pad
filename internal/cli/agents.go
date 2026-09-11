package cli

import (
	"fmt"
	"os"
	"os/exec"
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
	// DetectHomeDirs are directories (relative to $HOME) whose presence
	// suggests this tool is installed on the machine, even outside the
	// current project. Only populated for tools with an unambiguous home
	// directory signal.
	DetectHomeDirs []string
	// DetectBinaries are executable names checked via exec.LookPath whose
	// presence on PATH suggests this tool is installed on the machine. Only
	// populated for tools with an unambiguous, unique binary name.
	DetectBinaries []string
	// SkillDir is the relative path from project root for the skill file directory.
	SkillDir string
	// SkillFile is the filename within SkillDir.
	SkillFile string
}

// SupportedTools lists all supported agent tools.
// The "agents" target covers Codex, Cursor, Windsurf, and OpenCode via the shared .agents/skills/ directory.
var SupportedTools = []AgentTool{
	{
		Name:           "claude",
		Label:          "Claude Code",
		Aliases:        nil,
		DetectDirs:     []string{".claude"},
		DetectHomeDirs: []string{".claude"},
		DetectBinaries: []string{"claude"},
		SkillDir:       filepath.Join(".claude", "skills", "pad"),
		SkillFile:      "SKILL.md",
	},
	{
		Name:           "agents",
		Label:          "Codex / Cursor / Windsurf / OpenCode",
		Aliases:        []string{"codex", "cursor", "windsurf", "opencode"},
		DetectDirs:     []string{".cursor", ".windsurf", ".codex", ".opencode", ".agents"},
		DetectHomeDirs: []string{".cursor", ".windsurf", ".codex", ".opencode", ".agents"},
		DetectBinaries: []string{"codex", "cursor", "windsurf", "opencode"},
		SkillDir:       filepath.Join(".agents", "skills", "pad"),
		SkillFile:      "SKILL.md",
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

// agentsSkillBody is the eager dispatcher installed for tools that share the
// .agents skill format. The full, canonical guide stays embedded in Pad and is
// available by topic through `pad agent guide`; repeating it here would charge
// Cursor and Codex for reference material on every skill load.
const agentsSkillBody = `# Pad — Talk to Your Project

Use Pad when the user discusses project work: issues, tasks, plans, ideas,
progress, dependencies, conventions, roles, standups, or retrospectives.

## Start every invocation

Run ` + "`pad bootstrap --format json`" + ` before acting. It returns the workspace,
user, collections and schemas, always-on conventions, convention index, roles,
playbooks, dashboard, and recent activity in one call.

- If ` + "`pad`" + ` is missing, ask the user to install it or add it to PATH.
- If bootstrap fails, run ` + "`pad agent guide context-loading`" + ` and follow that
  section. Never initialize authentication blindly.
- Follow every body in ` + "`conventions`" + `. Before meaningful work, inspect
  ` + "`convention_index`" + ` and load bodies for the matching trigger.
- If ` + "`needs_onboarding`" + ` is true, offer setup and wait for consent.
- If roles exist and none was chosen in this conversation, ask once; do not block
  if the user declines.

## Act safely

- Use issue IDs such as ` + "`TASK-5`" + `, never slugs.
- Read an item before updating it. Send only changed fields and include a comment
  with status changes.
- Read collection schemas instead of guessing field names or terminal statuses.
- Confirm each mutation from the returned object before saying it succeeded.
- Use active playbooks when intent or an invocation slug matches; never run draft
  or deprecated playbooks unless the user explicitly asks.
- Prefer summary reads and bounded lists. Fetch full bodies only when needed.

## Common commands

` + "```bash" + `
pad project dashboard --format json
pad item show TASK-5 --format json
pad item list [collection] --format json
pad item create <collection> "Title" [flags]
pad item update TASK-5 [flags]
pad item comment TASK-5 -m "Message"
pad playbook list --format json
pad playbook show <slug> --format markdown
` + "```" + `

Use ` + "`pad <group> <command> --help`" + ` for exact flags.

## Load details only when needed

` + "```bash" + `
pad agent guide                         # list available topics
pad agent guide items                   # item commands and contracts
pad agent guide before-performing-work  # convention routing
pad agent guide role-awareness          # role behavior
pad agent guide multi-step-workflows    # planning, ideation, retro, onboarding
pad agent guide all                     # explicit full reference
` + "```" + `
`

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

// DetectTools returns the tools that appear to be in use, based on any of:
// a project-local directory, a home-directory install, or a binary on PATH.
func DetectTools() []AgentTool {
	cwd, cwdErr := os.Getwd()
	homeDir, homeErr := os.UserHomeDir()

	var detected []AgentTool
	for _, tool := range SupportedTools {
		switch {
		case cwdErr == nil && dirPresent(cwd, tool.DetectDirs):
		case homeErr == nil && dirPresent(homeDir, tool.DetectHomeDirs):
		case binaryPresent(tool.DetectBinaries):
		default:
			continue
		}
		detected = append(detected, tool)
	}
	return detected
}

// dirPresent reports whether any of dirs (relative to base) exists as a directory.
func dirPresent(base string, dirs []string) bool {
	for _, dir := range dirs {
		info, err := os.Stat(filepath.Join(base, dir))
		if err == nil && info.IsDir() {
			return true
		}
	}
	return false
}

// binaryPresent reports whether any of the given executable names is found on PATH.
func binaryPresent(bins []string) bool {
	for _, bin := range bins {
		if _, err := exec.LookPath(bin); err == nil {
			return true
		}
	}
	return false
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
		// Codex/Cursor/Windsurf/OpenCode use a compact eager dispatcher.
		// Detailed guidance remains available through `pad agent guide`.
		fm := `---
name: pad
description: "Talk to your project. Natural-language project management — create items, check status, create plans, brainstorm ideas, and more."
---

`
		return []byte(fm + agentsSkillBody)

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
