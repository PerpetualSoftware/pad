package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
)

func installCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "install [tool]",
		Short: "Install the /pad skill for your AI coding tools",
		Long: `Install the Pad skill file for AI coding tools.

With no arguments, auto-detects tools in use and offers to install for each.
Specify a tool name to install for that tool directly.

Supported tools:
  claude       Claude Code (.claude/skills/)
  cursor       Cursor (.agents/skills/) — also covers Codex, Windsurf & OpenCode
  codex        OpenAI Codex (.agents/skills/)
  windsurf     Windsurf (.agents/skills/)
  opencode     OpenCode (.agents/skills/)
  copilot      GitHub Copilot (.github/instructions/)
  amazon-q     Amazon Q Developer (.amazonq/rules/)
  junie        JetBrains Junie (.junie/guidelines/)

Examples:
  pad agent install              # Auto-detect and install
  pad agent install claude       # Install for Claude Code
  pad agent install cursor       # Install for Cursor/Codex/Windsurf/OpenCode
  pad agent install opencode     # Install for OpenCode
  pad agent install --all        # Install for all detected tools
  pad agent status               # Show supported tools and status`,
		ValidArgs: cli.AllToolNames(),
		RunE: func(cmd *cobra.Command, args []string) error {
			listFlag, _ := cmd.Flags().GetBool("list")
			allFlag, _ := cmd.Flags().GetBool("all")
			updateFlag, _ := cmd.Flags().GetBool("update")

			if listFlag {
				return installList()
			}

			if updateFlag {
				return installUpdate()
			}

			if len(args) > 0 {
				return installForTool(args[0])
			}

			if allFlag {
				return installAll()
			}

			return installInteractive()
		},
	}
	cmd.Flags().Bool("list", false, "list supported tools and installation status")
	cmd.Flags().Bool("all", false, "install for all detected tools")
	cmd.Flags().Bool("update", false, "update all installed tool integrations")
	return cmd
}

func installList() error {
	// Show local tool status (current directory)
	detected := map[string]bool{}
	for _, t := range cli.DetectTools() {
		detected[t.Name] = true
	}

	fmt.Println("Supported tools:")
	fmt.Println()
	for _, tool := range cli.SupportedTools {
		status := "  not installed"
		if cli.ToolInstalled(tool) {
			status = "  installed ✓"
		}
		det := ""
		if detected[tool.Name] {
			det = " (detected)"
		}
		aliases := ""
		if len(tool.Aliases) > 0 {
			aliases = fmt.Sprintf(" [aliases: %s]", strings.Join(tool.Aliases, ", "))
		}
		fmt.Printf("  %-12s %s%s%s%s\n", tool.Name, tool.Label, aliases, det, status)
	}

	// Show global installation registry
	reg, err := cli.LoadRegistry()
	if err != nil || len(reg.Installations) == 0 {
		return nil
	}

	reg.Prune()
	_ = reg.Save()

	statuses := reg.Status(pad.PadSkill)
	if len(statuses) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println("Tracked installations:")
	fmt.Println()

	outdatedCount := 0
	for _, s := range statuses {
		tool := cli.ResolveTool(s.Tool)
		toolLabel := s.Tool
		if tool != nil {
			toolLabel = tool.Label
		}

		state := "✓ up to date"
		if !s.Exists {
			state = "✗ missing"
		} else if s.Outdated {
			state = "⟳ update available"
			outdatedCount++
		}

		fmt.Printf("  %-40s  %-28s  %s\n", s.ProjectPath, toolLabel, state)
	}

	if outdatedCount > 0 {
		fmt.Printf("\n  %d installation(s) can be updated. Run 'pad agent update' to update all.\n", outdatedCount)
	}

	return nil
}

func installUpdate() error {
	// Phase 1: Update tools installed in the current directory
	localUpdated := 0
	for _, tool := range cli.SupportedTools {
		if !cli.ToolInstalled(tool) {
			continue
		}
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		fmt.Printf("  ✓ Updated %s → %s\n", tool.Label, path)
		recordInstallation(tool.Name, path)
		localUpdated++
	}

	// Phase 2: Update all tracked installations across other projects
	reg, err := cli.LoadRegistry()
	if err != nil {
		if localUpdated == 0 {
			fmt.Println("No tools installed. Run 'pad agent install' first.")
		}
		return nil
	}

	cwd, _ := os.Getwd()
	reg.Prune()
	globalUpdated, updateErrors := reg.UpdateAll(pad.PadSkill, version)
	_ = reg.Save()

	for _, e := range updateErrors {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", e)
	}

	// Subtract local updates that were also counted as global (same project path)
	overlapCount := 0
	for _, inst := range reg.Installations {
		if inst.ProjectPath == cwd {
			overlapCount++
		}
	}

	remoteUpdated := globalUpdated
	total := localUpdated + remoteUpdated
	if total == 0 {
		if localUpdated == 0 && len(reg.Installations) == 0 {
			fmt.Println("No tools installed. Run 'pad agent install' first.")
		} else {
			fmt.Println("All installations are up to date.")
		}
	} else {
		if remoteUpdated > 0 {
			fmt.Printf("\nUpdated %d installation(s) across all projects.\n", total)
		} else {
			fmt.Printf("\nUpdated %d tool(s) in current project.\n", localUpdated)
		}
	}
	return nil
}

// recordInstallation stores a skill install in the global registry (~/.pad/installations.json).
func recordInstallation(toolName, skillPath string) {
	reg, err := cli.LoadRegistry()
	if err != nil {
		return // best-effort — don't break install on registry errors
	}

	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	ws, _ := cli.DetectWorkspace("")
	reg.Record(cwd, ws, toolName, skillPath, version)
	_ = reg.Save()
}

func installForTool(name string) error {
	tool := cli.ResolveTool(name)
	if tool == nil {
		return fmt.Errorf("unknown tool %q. Run 'pad agent status' to see supported tools", name)
	}

	content := cli.FormatForTool(*tool, pad.PadSkill)
	path, err := cli.InstallForTool(*tool, content)
	if err != nil {
		return err
	}
	fmt.Printf("Installed /pad skill for %s → %s\n", tool.Label, path)
	recordInstallation(tool.Name, path)
	return nil
}

func installAll() error {
	detected := cli.DetectTools()
	if len(detected) == 0 {
		fmt.Println("No AI coding tools detected. Installing for Claude Code by default.")
		detected = []cli.AgentTool{cli.SupportedTools[0]} // Claude
	}

	for _, tool := range detected {
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		fmt.Printf("  ✓ %s → %s\n", tool.Label, path)
		recordInstallation(tool.Name, path)
	}
	return nil
}

func installInteractive() error {
	detected := cli.DetectTools()

	// Always include Claude if not already detected
	hasClaude := false
	for _, t := range detected {
		if t.Name == "claude" {
			hasClaude = true
			break
		}
	}
	if !hasClaude {
		detected = append([]cli.AgentTool{cli.SupportedTools[0]}, detected...)
	}

	if !cli.IsTerminal() {
		// Non-interactive: install for all detected tools
		for _, tool := range detected {
			content := cli.FormatForTool(tool, pad.PadSkill)
			path, err := cli.InstallForTool(tool, content)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
				continue
			}
			fmt.Printf("  ✓ %s → %s\n", tool.Label, path)
		}
		return nil
	}

	fmt.Println("Detected AI coding tools:")
	fmt.Println()
	for i, tool := range detected {
		installed := ""
		if cli.ToolInstalled(tool) {
			installed = " (already installed)"
		}
		fmt.Printf("  %d. %s%s\n", i+1, tool.Label, installed)
	}
	fmt.Println()
	fmt.Printf("Install /pad skill for all %d? (Y/n): ", len(detected))

	choice := readChoice()
	if choice == "n" || choice == "N" {
		fmt.Println()
		fmt.Println("Install individually with: pad agent install <tool>")
		fmt.Println("Supported tools:", strings.Join(cli.AllToolNames(), ", "))
		return nil
	}

	fmt.Println()
	for _, tool := range detected {
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		fmt.Printf("  ✓ %s → %s\n", tool.Label, path)
		recordInstallation(tool.Name, path)
	}
	return nil
}

// --- workspaces ---
