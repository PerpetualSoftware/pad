package main

import (
	"fmt"
	"os"
	"strings"
	"unicode"

	"github.com/spf13/cobra"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
)

func agentGuideCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "guide [topic]",
		Short: "Print Pad agent guidance on demand",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := string(cli.StripFrontmatter(pad.PadSkill))
			if len(args) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Available Pad agent guide topics:")
				for _, topic := range agentGuideTopics(body) {
					fmt.Fprintf(cmd.OutOrStdout(), "  %s\n", topic)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "\nUse `pad agent guide <topic>` for one section or `pad agent guide all` for the full guide.")
				return nil
			}

			if args[0] == "all" {
				fmt.Fprint(cmd.OutOrStdout(), body)
				return nil
			}
			section, ok := agentGuideSection(body, args[0])
			if !ok {
				return fmt.Errorf("unknown guide topic %q; run `pad agent guide` to list topics", args[0])
			}
			fmt.Fprint(cmd.OutOrStdout(), section)
			return nil
		},
	}
}

func agentGuideTopics(markdown string) []string {
	var topics []string
	for _, line := range strings.Split(markdown, "\n") {
		_, topic, ok := agentGuideHeading(line)
		if ok {
			topics = append(topics, topic)
		}
	}
	return topics
}

func agentGuideSection(markdown, topic string) (string, bool) {
	want := agentGuideSlug(topic)
	lines := strings.Split(markdown, "\n")
	start, level := -1, 0
	for i, line := range lines {
		lineLevel, lineTopic, ok := agentGuideHeading(line)
		if start < 0 {
			if ok && lineTopic == want {
				start, level = i, lineLevel
			}
			continue
		}
		if ok && lineLevel <= level {
			return strings.Join(lines[start:i], "\n") + "\n", true
		}
	}
	if start >= 0 {
		return strings.Join(lines[start:], "\n"), true
	}
	return "", false
}

func agentGuideHeading(line string) (level int, topic string, ok bool) {
	line = strings.TrimSpace(line)
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level < 2 || level >= len(line) || line[level] != ' ' {
		return 0, "", false
	}
	topic = agentGuideSlug(line[level+1:])
	return level, topic, topic != ""
}

func agentGuideSlug(s string) string {
	var out strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if dash && out.Len() > 0 {
				out.WriteByte('-')
			}
			out.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return out.String()
}

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
			force, _ := cmd.Flags().GetBool("force")

			if listFlag {
				return installList()
			}

			if updateFlag {
				return installUpdate(force)
			}

			if len(args) > 0 {
				return installForTool(args[0], force)
			}

			if allFlag {
				return installAll(force)
			}

			return installInteractive(force)
		},
	}
	cmd.Flags().Bool("list", false, "list supported tools and installation status")
	cmd.Flags().Bool("all", false, "install for all detected tools")
	cmd.Flags().Bool("update", false, "update all installed tool integrations")
	cmd.Flags().Bool("force", false, "replace a skill file even when it was edited or written by a newer pad (BUG-3466)")
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

	statuses := reg.Status(pad.PadSkill, version)
	if len(statuses) == 0 {
		return nil
	}

	fmt.Println()
	fmt.Println("Tracked installations:")
	fmt.Println()

	outdatedCount, keptCount := 0, 0
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
		} else if s.Edited {
			state = "! edited, kept (--force replaces)"
			keptCount++
		} else if s.Newer {
			state = "! newer pad wrote it, kept (upgrade pad, or --force replaces)"
			keptCount++
		}

		fmt.Printf("  %-40s  %-28s  %s\n", s.ProjectPath, toolLabel, state)
	}

	if outdatedCount > 0 {
		fmt.Printf("\n  %d installation(s) can be updated. Run 'pad agent update' to update all.\n", outdatedCount)
	}
	if keptCount > 0 {
		fmt.Printf("\n  %d installation(s) are kept as they are. Run 'pad agent update --force' to replace them.\n", keptCount)
	}

	return nil
}

func installUpdate(force bool) error {
	// Phase 1: Update tools installed in the current directory. A kept file
	// (edited, or a newer pad's) is reported by writeSkill and counted, so
	// the summary below never claims nothing is installed (BUG-3466).
	localUpdated, kept := 0, 0
	for _, tool := range cli.SupportedTools {
		if !cli.ToolInstalled(tool) {
			continue
		}
		res, err := writeSkill(tool, force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		if res.Wrote {
			fmt.Printf("  ✓ Updated %s → %s\n", tool.Label, res.Path)
			localUpdated++
		} else if res.Action == cli.SkillKeepEdited || res.Action == cli.SkillKeepNewer {
			kept++
		}
	}

	// Phase 2: Update all tracked installations across other projects
	reg, err := cli.LoadRegistry()
	if err != nil {
		printUpdateSummary(localUpdated, 0, kept, false)
		return nil
	}

	cwd, _ := os.Getwd()
	reg.Prune()
	globalUpdated, keptElsewhere, updateErrors := reg.UpdateAll(pad.PadSkill, version, force)
	_ = reg.Save()

	for _, e := range updateErrors {
		fmt.Fprintf(os.Stderr, "  warning: %v\n", e)
	}
	for _, k := range keptElsewhere {
		// This project's own kept files were reported in phase 1.
		if strings.HasPrefix(k, cwd+" (") {
			continue
		}
		fmt.Fprintf(os.Stderr, "  ! %s\n", k)
		kept++
	}
	printUpdateSummary(localUpdated, globalUpdated, kept, len(reg.Installations) > 0)
	return nil
}

// printUpdateSummary closes pad agent update. Kept files are named, so a run
// that kept an edited skill never ends by saying nothing is installed.
func printUpdateSummary(local, global, kept int, tracked bool) {
	total := local + global
	switch {
	case total > 0 && global > 0:
		fmt.Printf("\nUpdated %d installation(s) across all projects.\n", total)
	case total > 0:
		fmt.Printf("\nUpdated %d tool(s) in current project.\n", local)
	case kept == 0 && !tracked && local == 0:
		fmt.Println("No tools installed. Run 'pad agent install' first.")
	case kept == 0:
		fmt.Println("All installations are up to date.")
	}
	if kept > 0 {
		fmt.Printf("%d skill file(s) kept: edited, or written by a newer pad. Add --force to replace them.\n", kept)
	}
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

func installForTool(name string, force bool) error {
	tool := cli.ResolveTool(name)
	if tool == nil {
		return fmt.Errorf("unknown tool %q. Run 'pad agent status' to see supported tools", name)
	}

	res, err := writeSkill(*tool, force)
	if err != nil {
		return err
	}
	switch {
	case res.Wrote:
		fmt.Printf("Installed /pad skill for %s → %s\n", tool.Label, res.Path)
	case res.Action == cli.SkillUnchanged:
		fmt.Printf("/pad skill for %s is up to date → %s\n", tool.Label, res.Path)
	}
	return nil
}

func installAll(force bool) error {
	detected := cli.DetectTools()
	if len(detected) == 0 {
		fmt.Println("No AI coding tools detected. Installing for Claude Code by default.")
		detected = []cli.AgentTool{cli.SupportedTools[0]} // Claude
	}

	for _, tool := range detected {
		res, err := writeSkill(tool, force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		if res.Wrote {
			fmt.Printf("  ✓ %s → %s\n", tool.Label, res.Path)
		}
	}
	return nil
}

func installInteractive(force bool) error {
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

	// BUG-2593: gate on canPromptForConfig() (stdin AND stdout are
	// terminals) rather than cli.IsTerminal() (stdin only) — the same
	// swap offerSkillInstall got for BUG-2577 (PR #1111, which see for
	// the boundary): a pty-backed harness can make stdin look like a
	// char device with nobody able to answer, which left the "(Y/n): "
	// prompt printed even though the choice auto-defaults. A caller with
	// BOTH stdin and stdout attached to a pty but nothing driving it
	// still reads as promptable — that case can't be distinguished from
	// a real interactive terminal by any check available here.
	if !canPromptForConfig() {
		// Non-interactive: install for all detected tools
		for _, tool := range detected {
			res, err := writeSkill(tool, force)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
				continue
			}
			if res.Wrote {
				fmt.Printf("  ✓ %s → %s\n", tool.Label, res.Path)
			}
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
		res, err := writeSkill(tool, force)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		if res.Wrote {
			fmt.Printf("  ✓ %s → %s\n", tool.Label, res.Path)
		}
	}
	return nil
}

// --- workspaces ---
