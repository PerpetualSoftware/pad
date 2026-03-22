package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

// SkillsInstalled checks if the pad skill is installed at global or project level.
// Returns ("global", true), ("project", true), or ("", false).
func SkillsInstalled() (location string, installed bool) {
	// Check project-level: .claude/skills/pad/SKILL.md
	cwd, err := os.Getwd()
	if err == nil {
		if _, err := os.Stat(filepath.Join(cwd, ".claude", "skills", "pad", "SKILL.md")); err == nil {
			return "project", true
		}
	}

	// Check global: ~/.claude/skills/pad/SKILL.md
	homeDir, err := os.UserHomeDir()
	if err == nil {
		if _, err := os.Stat(filepath.Join(homeDir, ".claude", "skills", "pad", "SKILL.md")); err == nil {
			return "global", true
		}
	}

	return "", false
}

// SkillPath returns the full path to the installed skill file, or "" if not installed.
func SkillPath(location string) string {
	switch location {
	case "project":
		cwd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return filepath.Join(cwd, ".claude", "skills", "pad", "SKILL.md")
	case "global":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(homeDir, ".claude", "skills", "pad", "SKILL.md")
	}
	return ""
}

// SkillsOutdated checks if the installed skill differs from the embedded version.
// Returns true if the installed content doesn't match the embedded content.
func SkillsOutdated(embeddedContent []byte) (outdated bool, location string) {
	location, installed := SkillsInstalled()
	if !installed {
		return false, ""
	}

	path := SkillPath(location)
	if path == "" {
		return false, location
	}

	installedContent, err := os.ReadFile(path)
	if err != nil {
		return false, location
	}

	return !bytes.Equal(installedContent, embeddedContent), location
}

// InstallSkill writes the embedded pad SKILL.md to the target directory.
// target should be either "project" or "global".
func InstallSkill(skillContent []byte, target string) (string, error) {
	var baseDir string
	switch target {
	case "project":
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		baseDir = filepath.Join(cwd, ".claude")
	case "global":
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		baseDir = filepath.Join(homeDir, ".claude")
	default:
		return "", fmt.Errorf("invalid target: %s (use 'project' or 'global')", target)
	}

	skillDir := filepath.Join(baseDir, "skills", "pad")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", fmt.Errorf("create skill directory: %w", err)
	}

	destPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(destPath, skillContent, 0644); err != nil {
		return "", fmt.Errorf("write skill: %w", err)
	}

	return destPath, nil
}

// IsTerminal returns true if stdin is a terminal (not piped/redirected).
func IsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
