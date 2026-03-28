package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	pad "github.com/xarmian/pad"
	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/collections"
	"github.com/xarmian/pad/internal/config"
	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
	"github.com/xarmian/pad/internal/server"
	"github.com/xarmian/pad/internal/store"
)

var (
	version       = "dev"
	workspaceFlag string
	formatFlag    string
	urlFlag       string
)

func main() {
	rootCmd := &cobra.Command{
		Use:     "pad",
		Short:   "Pad — project management for developers and AI agents",
		Version: version,
	}

	rootCmd.PersistentFlags().StringVar(&workspaceFlag, "workspace", "", "workspace slug override")
	rootCmd.PersistentFlags().StringVar(&formatFlag, "format", "table", "output format: table, json, markdown")
	rootCmd.PersistentFlags().StringVar(&urlFlag, "url", "", "server URL override (e.g., https://api.getpad.dev)")

	rootCmd.AddCommand(
		serveCmd(),
		stopCmd(),
		initCmd(),
		linkCmd(),
		onboardCmd(),
		workspacesCmd(),
		switchCmd(),
		skillsCmd(),
		completionCmd(),
		// v2 commands
		createCmd(),
		listCmd(),
		showCmd(),
		updateCmd(),
		deleteCmd(),
		searchCmd(),
		statusCmd(),
		nextCmd(),
		collectionsCmd(),
		editCmd(),
		libraryCmd(),
		commentCmd(),
		commentsCmd(),
		exportCmd(),
		importCmd(),
	)

	rootCmd.RegisterFlagCompletionFunc("workspace", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg, err := config.Load()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		client := cli.NewClientFromURL(cfg.BaseURL())
		workspaces, err := client.ListWorkspaces()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var slugs []string
		for _, ws := range workspaces {
			slugs = append(slugs, ws.Slug)
		}
		return slugs, cobra.ShellCompDirectiveNoFileComp
	})

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// --- helpers ---

func getConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		os.Exit(1)
	}
	// --url flag takes highest precedence
	if urlFlag != "" {
		cfg.URL = urlFlag
	}
	return cfg
}

func getClient() (*cli.Client, *config.Config) {
	cfg := getConfig()
	if err := cli.EnsureServer(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return cli.NewClientFromURL(cfg.BaseURL()), cfg
}

func getWorkspace() string {
	ws, err := cli.DetectWorkspace(workspaceFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	return ws
}

func outputJSON(v interface{}) {
	cli.PrintJSON(v)
}

// --- serve ---

func serveCmd() *cobra.Command {
	var host string
	var port int

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the Pad API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfig()

			if cmd.Flags().Changed("host") {
				cfg.Host = host
			}
			if cmd.Flags().Changed("port") {
				cfg.Port = port
			}

			s, err := store.New(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer s.Close()

			// Auto-upgrade: ensure all default collections exist in every workspace.
			// This is safe because SeedDefaultCollections skips collections that already exist.
			if workspaces, err := s.ListWorkspaces(); err == nil {
				log.Printf("Auto-upgrade: checking %d workspace(s) for missing default collections", len(workspaces))
				for _, ws := range workspaces {
					if err := s.SeedDefaultCollections(ws.ID); err != nil {
						log.Printf("Warning: failed to seed defaults for workspace %s: %v", ws.Slug, err)
					}
				}
			} else {
				log.Printf("Warning: failed to list workspaces for auto-upgrade: %v", err)
			}

			srv := server.New(s)

			// Attach event bus for real-time SSE
			srv.SetEventBus(events.New())

			// Mount embedded web UI if available
			webFS, err := fs.Sub(pad.WebUI, "web/build")
			if err == nil {
				if entries, err := fs.ReadDir(webFS, "."); err == nil && len(entries) > 0 {
					srv.SetWebUI(webFS)
					log.Println("Serving embedded web UI")
				}
			}

			return srv.ListenAndServe(cfg.Addr())
		},
	}

	cmd.Flags().StringVar(&host, "host", "127.0.0.1", "host address to listen on")
	cmd.Flags().IntVar(&port, "port", 7777, "port to listen on")

	return cmd
}

// --- stop ---

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the background Pad server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := getConfig()
			if err := cli.StopServer(cfg); err != nil {
				return err
			}
			fmt.Println("Server stopped.")
			return nil
		},
	}
}

// --- init ---

func initCmd() *cobra.Command {
	var templateFlag string
	var listTemplates bool

	cmd := &cobra.Command{
		Use:   "init [name]",
		Short: "Create a workspace and link it to the current directory",
		Long: `Create a workspace and link it to the current directory.

Use --template to choose a workspace template:
  pad init myproject --template scrum

Use --list-templates to see available templates.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Handle --list-templates
			if listTemplates {
				tmpls := collections.ListTemplates()
				fmt.Println("Available templates:")
				for _, t := range tmpls {
					def := ""
					if t.Name == "startup" {
						def = " (default)"
					}
					fmt.Printf("  %-10s %s%s\n", t.Name, t.Description, def)
				}
				return nil
			}

			// Validate template name if provided
			if templateFlag != "" {
				tmpl := collections.GetTemplate(templateFlag)
				if tmpl == nil {
					fmt.Fprintf(os.Stderr, "Unknown template: %s\n\n", templateFlag)
					tmpls := collections.ListTemplates()
					fmt.Fprintln(os.Stderr, "Available templates:")
					for _, t := range tmpls {
						def := ""
						if t.Name == "startup" {
							def = " (default)"
						}
						fmt.Fprintf(os.Stderr, "  %-10s %s%s\n", t.Name, t.Description, def)
					}
					return fmt.Errorf("unknown template %q", templateFlag)
				}
			}

			client, _ := getClient()
			cwd, _ := os.Getwd()

			var name string
			if len(args) > 0 {
				name = args[0]
			} else {
				name = filepath.Base(cwd)
			}

			// Check if this directory is already linked to a valid workspace
			existingSlug, err := cli.DetectWorkspace("")
			if err == nil {
				ws, err := client.GetWorkspace(existingSlug)
				if err == nil && ws != nil {
					fmt.Printf("Already linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
					offerSkillInstall()
					return nil
				}
			}

			// Check if a workspace with this name already exists
			var ws *models.Workspace
			workspaces, err := client.ListWorkspaces()
			if err == nil {
				for i := range workspaces {
					if strings.EqualFold(workspaces[i].Name, name) {
						ws = &workspaces[i]
						break
					}
				}
			}

			newlyCreated := false
			if ws != nil {
				if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
					return fmt.Errorf("write .pad.toml: %w", err)
				}
				fmt.Printf("Workspace %q already exists (slug: %s). Linked to %s\n", ws.Name, ws.Slug, cwd)
			} else {
				ws, err = client.CreateWorkspace(models.WorkspaceCreate{
					Name:     name,
					Template: templateFlag,
				})
				if err != nil {
					return fmt.Errorf("create workspace: %w", err)
				}

				if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
					return fmt.Errorf("write .pad.toml: %w", err)
				}

				tmplMsg := ""
				if templateFlag != "" && templateFlag != "startup" {
					tmplMsg = fmt.Sprintf(" with %q template", templateFlag)
				}
				fmt.Printf("Created workspace %q (slug: %s)%s\n", ws.Name, ws.Slug, tmplMsg)
				fmt.Printf("Linked to %s\n", cwd)
				newlyCreated = true
			}

			offerSkillInstall()

			if newlyCreated {
				printOnboardingHints()
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&templateFlag, "template", "", "workspace template (startup, scrum, product)")
	cmd.Flags().BoolVar(&listTemplates, "list-templates", false, "list available workspace templates")

	return cmd
}

func linkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link <workspace>",
		Short: "Link the current directory to an existing workspace",
		Long: `Link the current directory to an existing workspace by creating a .pad.toml file.

Unlike 'pad init', this does NOT create a new workspace — it only links to one that already exists.

  pad link myproject

Use 'pad workspaces' to see available workspaces.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			cwd, _ := os.Getwd()
			nameOrSlug := args[0]

			// Check if already linked
			existingSlug, err := cli.DetectWorkspace("")
			if err == nil {
				ws, err := client.GetWorkspace(existingSlug)
				if err == nil && ws != nil {
					fmt.Printf("Already linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
					return nil
				}
			}

			// Find workspace by name or slug
			var ws *models.Workspace
			workspaces, err := client.ListWorkspaces()
			if err != nil {
				return fmt.Errorf("list workspaces: %w", err)
			}
			for i := range workspaces {
				if strings.EqualFold(workspaces[i].Name, nameOrSlug) || workspaces[i].Slug == nameOrSlug {
					ws = &workspaces[i]
					break
				}
			}

			if ws == nil {
				fmt.Fprintf(os.Stderr, "Workspace %q not found.\n\n", nameOrSlug)
				fmt.Fprintln(os.Stderr, "Available workspaces:")
				for _, w := range workspaces {
					fmt.Fprintf(os.Stderr, "  %-20s (slug: %s)\n", w.Name, w.Slug)
				}
				return fmt.Errorf("workspace %q does not exist — use 'pad init %s' to create it", nameOrSlug, nameOrSlug)
			}

			if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
				return fmt.Errorf("write .pad.toml: %w", err)
			}

			fmt.Printf("Linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			fmt.Printf("  %s/.pad.toml\n", cwd)
			offerSkillInstall()
			return nil
		},
	}
}

func offerSkillInstall() {
	location, installed := cli.SkillsInstalled()
	if installed {
		outdated, _ := cli.SkillsOutdated(pad.PadSkill)
		if outdated {
			fmt.Printf("\nClaude Code skills are outdated (%s).\n", location)
			if cli.IsTerminal() {
				fmt.Print("Update to latest version? (y/N): ")
				if choice := readChoice(); choice == "y" || choice == "Y" {
					path, err := cli.InstallSkill(pad.PadSkill, location)
					if err != nil {
						fmt.Fprintf(os.Stderr, "Error updating skill: %v\n", err)
						return
					}
					fmt.Printf("Updated /pad skill at %s\n", path)
				}
			} else {
				fmt.Println("Run 'pad skills update' to update.")
			}
		} else {
			fmt.Printf("\nClaude Code skills up to date (%s).\n", location)
		}
		return
	}

	if !cli.IsTerminal() {
		return
	}

	fmt.Println()
	fmt.Println("Claude Code skills are not installed.")
	fmt.Println("Install skills?")
	fmt.Println("  1. Project (.claude/skills/) — just this project")
	fmt.Println("  2. Global  (~/.claude/skills/) — all projects")
	fmt.Println("  3. Skip")
	fmt.Print("\nChoice (1/2/3): ")

	choice := readChoice()
	switch choice {
	case "1":
		path, err := cli.InstallSkill(pad.PadSkill, "project")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error installing skill: %v\n", err)
			return
		}
		fmt.Printf("Installed /pad skill to %s\n", path)
	case "2":
		path, err := cli.InstallSkill(pad.PadSkill, "global")
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error installing skill: %v\n", err)
			return
		}
		fmt.Printf("Installed /pad skill to %s\n", path)
	default:
		fmt.Println("Skipped. Run 'pad skills install' later.")
	}
}

func readChoice() string {
	var input string
	fmt.Scanln(&input)
	return strings.TrimSpace(input)
}

func printOnboardingHints() {
	fmt.Println()
	fmt.Println("Get started:")
	fmt.Println("  /pad scan this codebase and set up my workspace")
	fmt.Println("  /pad what conventions should this project follow?")
	fmt.Println("  /pad create a phase for what I'm working on")
	fmt.Println()
	fmt.Println("Or open the web UI at http://localhost:7777")
}

// --- onboard ---

func onboardCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "onboard",
		Short: "Analyze the project and suggest items to populate the workspace",
		Long: `Analyze the current project directory to detect tooling and suggest
conventions, then optionally create them in the workspace.

This scans for build config, CI setup, linters, and project structure to
recommend conventions from the built-in library.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			cwd, _ := os.Getwd()
			info := cli.DetectProject(cwd)

			// Print detection results
			fmt.Println("Scanning project...")
			if info.Language != "" {
				fmt.Printf("  Language:   %s\n", info.Language)
			}
			if info.BuildTool != "" {
				fmt.Printf("  Build:      %s\n", info.BuildTool)
			}
			if info.TestCmd != "" {
				fmt.Printf("  Tests:      %s\n", info.TestCmd)
			}
			if info.HasCI {
				fmt.Printf("  CI:         %s\n", info.CIProvider)
			}
			if info.HasLinter {
				fmt.Println("  Linter:     detected")
			}
			if info.Language == "" && info.BuildTool == "" {
				fmt.Println("  Could not detect project type.")
				fmt.Println()
				fmt.Println("Try using /pad to set up your workspace conversationally:")
				fmt.Println("  /pad scan this codebase and set up my workspace")
				return nil
			}

			fmt.Println()

			// Get suggested conventions
			suggestions := cli.SuggestedConventions(info)

			// Check which are already active
			existingConventions, _ := client.ListCollectionItems(ws, "conventions", nil)
			existingTitles := make(map[string]bool)
			for _, item := range existingConventions {
				existingTitles[item.Title] = true
			}

			// Filter to new suggestions only
			type suggestion struct {
				title   string
				content string
			}
			var newSuggestions []suggestion
			for title, content := range suggestions {
				if !existingTitles[title] {
					newSuggestions = append(newSuggestions, suggestion{title, content})
				}
			}

			if len(newSuggestions) == 0 {
				fmt.Println("All suggested conventions are already active.")
				return nil
			}

			fmt.Printf("Suggested conventions (%d new):\n", len(newSuggestions))
			for i, s := range newSuggestions {
				fmt.Printf("  %d. %s\n", i+1, s.title)
			}

			if !cli.IsTerminal() {
				// Non-interactive: just print suggestions
				fmt.Println()
				fmt.Println("Run 'pad onboard' in a terminal to activate, or use:")
				fmt.Println("  /pad what conventions should this project follow?")
				return nil
			}

			fmt.Print("\nCreate these conventions? (y/N): ")
			choice := readChoice()
			if choice != "y" && choice != "Y" {
				fmt.Println("Skipped. You can activate conventions from the library:")
				fmt.Printf("  http://localhost:7777/%s/library\n", ws)
				return nil
			}

			// Look up library conventions to get proper trigger/scope/priority
			libraryConventions := collections.ConventionLibrary()
			libraryMap := make(map[string]collections.LibraryConvention)
			for _, cat := range libraryConventions {
				for _, conv := range cat.Conventions {
					libraryMap[conv.Title] = conv
				}
			}

			created := 0
			for _, s := range newSuggestions {
				// Use library metadata if available, otherwise use sensible defaults
				trigger := "on-implement"
				scope := "all"
				priority := "should"
				if lc, ok := libraryMap[s.title]; ok {
					trigger = lc.Trigger
					scope = lc.Scope
					priority = lc.Priority
				}

				fieldsJSON := fmt.Sprintf(`{"status":"active","trigger":"%s","scope":"%s","priority":"%s"}`, trigger, scope, priority)
				_, err := client.CreateItem(ws, "conventions", models.ItemCreate{
					Title:   s.title,
					Content: s.content,
					Fields:  fieldsJSON,
				})
				if err != nil {
					fmt.Fprintf(os.Stderr, "  Failed to create %q: %v\n", s.title, err)
					continue
				}
				fmt.Printf("  Created: %s\n", s.title)
				created++
			}

			fmt.Printf("\n%d conventions created.\n", created)
			return nil
		},
	}
	return cmd
}

// --- skills ---

func skillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage Claude Code skill installation",
	}

	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install Claude Code skills",
		RunE: func(cmd *cobra.Command, args []string) error {
			global, _ := cmd.Flags().GetBool("global")
			target := "project"
			if global {
				target = "global"
			}
			path, err := cli.InstallSkill(pad.PadSkill, target)
			if err != nil {
				return err
			}
			fmt.Printf("Installed /pad skill to %s\n", path)
			return nil
		},
	}
	installCmd.Flags().Bool("global", false, "install to ~/.claude/skills/")

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Update installed skills to the version bundled in this binary",
		RunE: func(cmd *cobra.Command, args []string) error {
			location, installed := cli.SkillsInstalled()
			if !installed {
				return fmt.Errorf("skills not installed. Run 'pad skills install' first")
			}
			outdated, _ := cli.SkillsOutdated(pad.PadSkill)
			if !outdated {
				fmt.Println("Skills are already up to date.")
				return nil
			}
			path, err := cli.InstallSkill(pad.PadSkill, location)
			if err != nil {
				return err
			}
			fmt.Printf("Updated /pad skill at %s\n", path)
			return nil
		},
	}

	statusSubCmd := &cobra.Command{
		Use:   "status",
		Short: "Check if Claude Code skills are installed and up to date",
		Run: func(cmd *cobra.Command, args []string) {
			location, installed := cli.SkillsInstalled()
			if !installed {
				fmt.Println("Skills not installed. Run 'pad skills install' to install.")
				return
			}
			outdated, _ := cli.SkillsOutdated(pad.PadSkill)
			if outdated {
				fmt.Printf("Skills installed (%s) — UPDATE AVAILABLE\n", location)
				fmt.Println("  Run 'pad skills update' to update.")
			} else {
				fmt.Printf("Skills installed (%s) — up to date\n", location)
			}
		},
	}

	cmd.AddCommand(installCmd, updateCmd, statusSubCmd)
	return cmd
}

// --- workspaces ---

func workspacesCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "workspaces",
		Aliases: []string{"ws"},
		Short:   "List all workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			workspaces, err := client.ListWorkspaces()
			if err != nil {
				return err
			}
			if len(workspaces) == 0 {
				fmt.Println("No workspaces. Run 'pad init' to create one.")
				return nil
			}

			current, _ := cli.DetectWorkspace(workspaceFlag)

			for _, ws := range workspaces {
				marker := "  "
				if ws.Slug == current {
					marker = "* "
				}
				fmt.Printf("%s%s (%s) — updated %s\n",
					marker, ws.Name, ws.Slug, cli.RelativeTime(ws.UpdatedAt))
			}
			return nil
		},
	}
}

// --- switch ---

func switchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "switch <workspace>",
		Short: "Link current directory to a different workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws, err := client.GetWorkspace(args[0])
			if err != nil {
				return fmt.Errorf("workspace %q not found", args[0])
			}

			cwd, _ := os.Getwd()
			if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
				return err
			}
			fmt.Printf("Switched to workspace %q\n", ws.Name)
			return nil
		},
	}
}

// --- completion ---

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion scripts",
		Long: `Generate shell completion scripts for your shell.

Usage:
  source <(pad completion bash)
  pad completion zsh > "${fpath[1]}/_pad"
  pad completion fish | source`,
		DisableFlagsInUseLine: true,
		ValidArgs:             []string{"bash", "zsh", "fish", "powershell"},
		Args:                  cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":
				return cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell":
				return cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell: %s", args[0])
			}
		},
	}
}

// =============================================================================
// v2 Commands: create, list, show, update, delete, search, status, next, collections
// =============================================================================

// --- create ---

func createCmd() *cobra.Command {
	var (
		content    string
		useStdin   bool
		priority   string
		status     string
		assignee   string
		phase      string
		category   string
		parentSlug string
		tags       string
		fieldFlags []string
	)

	cmd := &cobra.Command{
		Use:   "create <collection> <title>",
		Short: "Create a new item in a collection",
		Long: `Create a new item in the specified collection.

Examples:
  pad create task "Fix OAuth redirect" --priority high
  pad create idea "Real-time collaboration" --category infrastructure
  pad create phase "API Redesign" --status active
  pad create doc "Payment Architecture" --category architecture --stdin`,
		ValidArgsFunction: completeCollectionNames,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			collSlug := normalizeCollectionSlug(args[0])
			title := args[1]

			// Build fields JSON from flags
			fields := make(map[string]interface{})
			if status != "" {
				fields["status"] = status
			}
			if priority != "" {
				fields["priority"] = priority
			}
			if assignee != "" {
				fields["assignee"] = assignee
			}
			if phase != "" {
				// Resolve phase slug to item ID
				phaseItem, err := client.GetItem(ws, phase)
				if err != nil {
					return fmt.Errorf("phase %q not found: %w", phase, err)
				}
				fields["phase"] = phaseItem.ID
			}
			if category != "" {
				fields["category"] = category
			}

			// Apply arbitrary --field key=value flags
			for _, kv := range fieldFlags {
				if idx := strings.Index(kv, "="); idx > 0 {
					fields[kv[:idx]] = kv[idx+1:]
				}
			}

			fieldsJSON, _ := json.Marshal(fields)

			// Handle content from stdin
			body := content
			if useStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				body = string(data)
			}

			input := models.ItemCreate{
				Title:     title,
				Content:   body,
				Fields:    string(fieldsJSON),
				Tags:      tags,
				CreatedBy: "user",
				Source:    "cli",
			}

			item, err := client.CreateItem(ws, collSlug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(item)
			}

			icon := item.CollectionIcon
			if icon == "" {
				icon = "📦"
			}
			fmt.Printf("Created %s %s: %q (%s)\n", icon, item.CollectionName, item.Title, item.Slug)
			if summary := cli.FormatFieldSummary(item.Fields); summary != "" {
				fmt.Printf("  %s\n", summary)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&content, "content", "", "item body content")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read content from stdin")
	cmd.Flags().StringVar(&priority, "priority", "", "priority field value")
	cmd.Flags().StringVar(&status, "status", "", "status field value")
	cmd.Flags().StringVar(&assignee, "assignee", "", "assignee field value")
	cmd.Flags().StringVar(&phase, "phase", "", "phase relation (slug or ID)")
	cmd.Flags().StringVar(&category, "category", "", "category field value")
	cmd.Flags().StringVar(&parentSlug, "parent", "", "parent item slug for nesting")
	cmd.Flags().StringVar(&tags, "tags", "", "JSON array of tags")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "set arbitrary field (repeatable): --field key=value")

	// Shell completion for collection arg
	cmd.RegisterFlagCompletionFunc("status", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"open", "in_progress", "done", "draft", "active", "completed", "raw", "exploring", "decided"}, cobra.ShellCompDirectiveNoFileComp
	})
	cmd.RegisterFlagCompletionFunc("priority", func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return []string{"low", "medium", "high", "critical"}, cobra.ShellCompDirectiveNoFileComp
	})

	return cmd
}

// --- list ---

func listCmd() *cobra.Command {
	var (
		statusFilter   string
		priorityFilter string
		assigneeFilter string
		sortBy         string
		groupBy        string
		limitNum       int
		showAll        bool
		fieldFlags     []string
	)

	cmd := &cobra.Command{
		Use:   "list [collection]",
		Short: "List items, optionally filtered by collection",
		Long: `List items in the workspace. If a collection is specified, only items
in that collection are shown. Items with status "done" are hidden by default.

Examples:
  pad list                          # all items, all collections
  pad list tasks                    # tasks (open + in_progress by default)
  pad list tasks --status done      # only done tasks
  pad list ideas --status exploring # ideas being explored
  pad list --all                    # include done/completed items`,
		Aliases:           []string{"ls"},
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeCollectionNames,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}

			// Add field filters
			if statusFilter != "" {
				params.Set("status", statusFilter)
			} else if !showAll {
				// Default: exclude terminal statuses (done, completed, archived, etc.)
				// Rather than listing all active statuses, we fetch all items and
				// let the server filter. We use a broad inclusion list that covers
				// all built-in templates plus common custom statuses.
				params.Set("status", "open,in_progress,in-progress,active,draft,raw,exploring,decided,new,triaged,fixing,planned,published,paused,proposed,researching,building,ready,in_sprint,reviewed,planning")
			}
			if priorityFilter != "" {
				params.Set("priority", priorityFilter)
			}
			if assigneeFilter != "" {
				params.Set("assignee", assigneeFilter)
			}
			if sortBy != "" {
				params.Set("sort", sortBy)
			}
			if groupBy != "" {
				params.Set("group_by", groupBy)
			}
			if limitNum > 0 {
				params.Set("limit", fmt.Sprintf("%d", limitNum))
			}

			// Apply arbitrary --field key=value filters as query params
			for _, kv := range fieldFlags {
				if idx := strings.Index(kv, "="); idx > 0 {
					params.Set(kv[:idx], kv[idx+1:])
				}
			}

			var items []models.Item
			var err error

			if len(args) > 0 {
				items, err = client.ListCollectionItems(ws, normalizeCollectionSlug(args[0]), params)
			} else {
				items, err = client.ListItems(ws, params)
			}
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(items)
			}

			if len(items) == 0 {
				fmt.Println("No items found.")
				return nil
			}

			// Group by collection if listing all
			if len(args) == 0 && groupBy == "" {
				printItemsGroupedByCollection(items)
			} else {
				cli.PrintItemTable(items)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (comma-separated)")
	cmd.Flags().StringVar(&priorityFilter, "priority", "", "filter by priority")
	cmd.Flags().StringVar(&assigneeFilter, "assignee", "", "filter by assignee")
	cmd.Flags().StringVar(&sortBy, "sort", "", "sort order (e.g. priority:desc,created_at:asc)")
	cmd.Flags().StringVar(&groupBy, "group-by", "", "group results by field")
	cmd.Flags().IntVar(&limitNum, "limit", 0, "max number of items to return")
	cmd.Flags().BoolVar(&showAll, "all", false, "include done/completed/archived items")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "filter by field value (repeatable): --field key=value")

	return cmd
}

func printItemsGroupedByCollection(items []models.Item) {
	groups := make(map[string][]models.Item)
	order := []string{}

	for _, item := range items {
		key := item.CollectionSlug
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], item)
	}

	for _, key := range order {
		groupItems := groups[key]
		icon := ""
		name := key
		if len(groupItems) > 0 {
			if groupItems[0].CollectionIcon != "" {
				icon = groupItems[0].CollectionIcon + " "
			}
			if groupItems[0].CollectionName != "" {
				name = groupItems[0].CollectionName
			}
		}
		fmt.Printf("\n%s%s (%d)\n", icon, name, len(groupItems))
		fmt.Println(strings.Repeat("─", 40))

		for _, item := range groupItems {
			statusStr := extractFieldFromJSON(item.Fields, "status")
			if statusStr != "" {
				statusStr = " [" + statusStr + "]"
			}
			fmt.Printf("  %s%s\n", item.Title, statusStr)
		}
	}
	fmt.Println()
}

// --- show ---

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <slug>",
		Short: "Show item detail (fields + content)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(item)
			}

			if formatFlag == "markdown" {
				fmt.Println(item.Content)
				return nil
			}

			// Table format: show metadata + fields + content
			cli.PrintItemMeta(item)

			// Print fields
			if item.Fields != "" && item.Fields != "{}" {
				var fields map[string]interface{}
				if err := json.Unmarshal([]byte(item.Fields), &fields); err == nil {
					for k, v := range fields {
						fmt.Printf("%-12s %v\n", k+":", v)
					}
					fmt.Println("---")
				}
			}

			if item.Content != "" {
				fmt.Println(item.Content)
			}

			// Show recent comments
			comments, err := client.ListComments(ws, item.Slug)
			if err == nil && len(comments) > 0 {
				fmt.Println("\n--- Comments ---")
				// Show last 5 comments
				start := 0
				if len(comments) > 5 {
					start = len(comments) - 5
					fmt.Printf("(%d earlier comments not shown)\n\n", start)
				}
				cli.PrintCommentTable(comments[start:])
			}

			return nil
		},
	}
}

// --- update ---

func updateCmd() *cobra.Command {
	var (
		title      string
		content    string
		useStdin   bool
		status     string
		priority   string
		assignee   string
		phase      string
		category   string
		tags       string
		fieldFlags []string
	)

	cmd := &cobra.Command{
		Use:   "update <slug> [--field value...]",
		Short: "Update an item's fields or content",
		Long: `Update an existing item. Only the specified fields are changed.

Examples:
  pad update fix-oauth --status done
  pad update api-redesign --status active --priority high
  pad update payment-arch --stdin < updated-doc.md`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			slug := args[0]

			// First get the current item to merge fields
			item, err := client.GetItem(ws, slug)
			if err != nil {
				return err
			}

			input := models.ItemUpdate{
				LastModifiedBy: "user",
				Source:         "cli",
			}

			if title != "" {
				input.Title = &title
			}

			// Handle content
			if useStdin {
				data, err := io.ReadAll(os.Stdin)
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				body := string(data)
				input.Content = &body
			} else if content != "" {
				input.Content = &content
			}

			if tags != "" {
				input.Tags = &tags
			}

			// Merge field changes with existing fields
			hasFieldChanges := status != "" || priority != "" || assignee != "" || phase != "" || category != "" || len(fieldFlags) > 0
			if hasFieldChanges {
				existingFields := make(map[string]interface{})
				if item.Fields != "" && item.Fields != "{}" {
					json.Unmarshal([]byte(item.Fields), &existingFields)
				}

				if status != "" {
					existingFields["status"] = status
				}
				if priority != "" {
					existingFields["priority"] = priority
				}
				if assignee != "" {
					existingFields["assignee"] = assignee
				}
				if phase != "" {
					// Resolve phase slug to item ID
					phaseItem, err := client.GetItem(ws, phase)
					if err != nil {
						return fmt.Errorf("phase %q not found: %w", phase, err)
					}
					existingFields["phase"] = phaseItem.ID
				}
				if category != "" {
					existingFields["category"] = category
				}

				// Apply arbitrary --field key=value flags
				for _, kv := range fieldFlags {
					if idx := strings.Index(kv, "="); idx > 0 {
						existingFields[kv[:idx]] = kv[idx+1:]
					}
				}

				fieldsJSON, _ := json.Marshal(existingFields)
				fieldsStr := string(fieldsJSON)
				input.Fields = &fieldsStr
			}

			updated, err := client.UpdateItem(ws, slug, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(updated)
			}

			fmt.Printf("Updated %q (%s)\n", updated.Title, updated.Slug)
			if summary := cli.FormatFieldSummary(updated.Fields); summary != "" {
				fmt.Printf("  %s\n", summary)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&title, "title", "", "update title")
	cmd.Flags().StringVar(&content, "content", "", "update body content")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read content from stdin")
	cmd.Flags().StringVar(&status, "status", "", "update status field")
	cmd.Flags().StringVar(&priority, "priority", "", "update priority field")
	cmd.Flags().StringVar(&assignee, "assignee", "", "update assignee field")
	cmd.Flags().StringVar(&phase, "phase", "", "update phase relation")
	cmd.Flags().StringVar(&category, "category", "", "update category field")
	cmd.Flags().StringVar(&tags, "tags", "", "update tags (JSON array)")
	cmd.Flags().StringArrayVarP(&fieldFlags, "field", "f", nil, "set arbitrary field (repeatable): --field key=value")

	return cmd
}

// --- delete ---

func deleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <slug>",
		Short: "Archive (soft-delete) an item",
		Aliases: []string{"rm"},
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			if err := client.DeleteItem(ws, args[0]); err != nil {
				return err
			}

			fmt.Printf("Archived %q\n", args[0])
			return nil
		},
	}
}

// --- comments ---

func commentCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "comment <slug> <message>",
		Short: "Add a comment to an item",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			input := models.CommentCreate{
				Body:      args[1],
				CreatedBy: "user",
				Source:    "cli",
			}

			comment, err := client.CreateComment(ws, args[0], input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(comment)
			}

			fmt.Printf("Comment added to %s\n", args[0])
			return nil
		},
	}
}

func commentsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "comments <slug>",
		Short: "List comments on an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			comments, err := client.ListComments(ws, args[0])
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(comments)
			}

			cli.PrintCommentTable(comments)
			return nil
		},
	}
}

// --- search ---

func searchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across all items",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}
			params.Set("q", strings.Join(args, " "))
			params.Set("workspace", ws)

			result, err := client.SearchItems(params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(result)
			}

			// Parse and display results
			var searchResp struct {
				Results []struct {
					Item    models.Item `json:"item"`
					Snippet string      `json:"snippet"`
				} `json:"results"`
				Total int `json:"total"`
			}

			if err := json.Unmarshal(result, &searchResp); err != nil {
				// Fallback: just print raw JSON
				fmt.Println(string(result))
				return nil
			}

			if searchResp.Total == 0 {
				fmt.Println("No results found.")
				return nil
			}

			for _, r := range searchResp.Results {
				icon := r.Item.CollectionIcon
				if icon == "" {
					icon = "📦"
				}
				fmt.Printf("%s %s (%s)\n", icon, r.Item.Title, r.Item.CollectionName)
				if r.Snippet != "" {
					fmt.Printf("  %s\n", r.Snippet)
				}
				fmt.Println()
			}
			fmt.Printf("%d result(s)\n", searchResp.Total)
			return nil
		},
	}
}

// --- status ---

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show project dashboard — progress, attention items, suggested next",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			dashJSON, err := client.GetDashboard(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(dashJSON)
			}

			// Parse the dashboard response
			var dash struct {
				Summary struct {
					TotalItems   int                       `json:"total_items"`
					ByCollection map[string]map[string]int `json:"by_collection"`
				} `json:"summary"`
				ActivePhases []struct {
					Slug      string `json:"slug"`
					Title     string `json:"title"`
					Progress  int    `json:"progress"`
					TaskCount int    `json:"task_count"`
					DoneCount int    `json:"done_count"`
				} `json:"active_phases"`
				Attention []struct {
					Type      string `json:"type"`
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"attention"`
				SuggestedNext []struct {
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"suggested_next"`
			}

			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				fmt.Println(string(dashJSON))
				return nil
			}

			fmt.Printf("📊 Project Status (%d items)\n", dash.Summary.TotalItems)
			fmt.Println(strings.Repeat("═", 50))

			// Collection summary
			if len(dash.Summary.ByCollection) > 0 {
				fmt.Println()
				for collSlug, statuses := range dash.Summary.ByCollection {
					parts := []string{}
					for status, count := range statuses {
						parts = append(parts, fmt.Sprintf("%s: %d", status, count))
					}
					fmt.Printf("  %-10s  %s\n", collSlug, strings.Join(parts, ", "))
				}
			}

			// Active phases
			if len(dash.ActivePhases) > 0 {
				fmt.Println()
				fmt.Println("🏗️  Active Phases")
				for _, p := range dash.ActivePhases {
					bar := progressBar(p.Progress, 20)
					fmt.Printf("  %s %s %d%% (%d/%d tasks)\n", p.Title, bar, p.Progress, p.DoneCount, p.TaskCount)
				}
			}

			// Attention items
			if len(dash.Attention) > 0 {
				fmt.Println()
				fmt.Println("⚠️  Needs Attention")
				for _, a := range dash.Attention {
					fmt.Printf("  %s — %s\n", a.ItemTitle, a.Reason)
				}
			}

			// Suggested next
			if len(dash.SuggestedNext) > 0 {
				fmt.Println()
				fmt.Println("💡 Suggested Next")
				for _, s := range dash.SuggestedNext {
					fmt.Printf("  %s — %s\n", s.ItemTitle, s.Reason)
				}
			}

			fmt.Println()
			return nil
		},
	}
}

func progressBar(pct, width int) string {
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	return "[" + strings.Repeat("█", filled) + strings.Repeat("░", width-filled) + "]"
}

// --- next ---

func nextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "next",
		Short: "Recommend the next task to work on",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			dashJSON, err := client.GetDashboard(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(dashJSON)
			}

			var dash struct {
				SuggestedNext []struct {
					ItemSlug  string `json:"item_slug"`
					ItemTitle string `json:"item_title"`
					Collection string `json:"collection"`
					Reason    string `json:"reason"`
				} `json:"suggested_next"`
			}

			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				return err
			}

			if len(dash.SuggestedNext) == 0 {
				fmt.Println("No suggestions — all tasks may be complete or no active phases found.")
				return nil
			}

			fmt.Println("💡 Recommended next:")
			for i, s := range dash.SuggestedNext {
				fmt.Printf("  %d. %s\n     %s\n", i+1, s.ItemTitle, s.Reason)
			}
			return nil
		},
	}
}

// --- collections ---

func collectionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collections",
		Short: "List and manage collections",
		Aliases: []string{"coll"},
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			colls, err := client.ListCollections(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(colls)
			}

			cli.PrintCollectionTable(colls)
			return nil
		},
	}

	cmd.AddCommand(collectionsCreateCmd())
	return cmd
}

func collectionsCreateCmd() *cobra.Command {
	var (
		icon        string
		description string
		fieldsDSL   string
		layout      string
		defaultView string
		boardGroup  string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a custom collection",
		Long: `Create a new collection with custom fields.

Fields DSL format: key:type[:option1,option2,...]
Separate multiple fields with newlines or semicolons.

Examples:
  pad collections create "Bugs" --fields "status:select:new,triaged,fixing,resolved;severity:select:low,medium,high,critical;component:text"
  pad collections create "Decisions" --icon "⚖️" --fields "status:select:proposed,accepted,rejected;impact:select:low,medium,high"`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			name := args[0]

			// Parse fields DSL into schema JSON
			schema := models.CollectionSchema{}
			if fieldsDSL != "" {
				fields := strings.Split(fieldsDSL, ";")
				for _, f := range fields {
					f = strings.TrimSpace(f)
					if f == "" {
						continue
					}
					parts := strings.SplitN(f, ":", 3)
					if len(parts) < 2 {
						return fmt.Errorf("invalid field definition: %q (expected key:type[:options])", f)
					}
					fd := models.FieldDef{
						Key:   parts[0],
						Label: strings.Title(strings.ReplaceAll(parts[0], "_", " ")),
						Type:  parts[1],
					}
					if len(parts) == 3 && parts[2] != "" {
						fd.Options = strings.Split(parts[2], ",")
					}
					// First select field gets required+default
					if fd.Type == "select" && fd.Key == "status" {
						fd.Required = true
						if len(fd.Options) > 0 {
							fd.Default = fd.Options[0]
						}
					}
					schema.Fields = append(schema.Fields, fd)
				}
			}

			schemaJSON, _ := json.Marshal(schema)

			// Build settings
			settings := models.CollectionSettings{
				Layout:       layout,
				DefaultView:  defaultView,
				BoardGroupBy: boardGroup,
			}
			if settings.Layout == "" {
				settings.Layout = "fields-primary"
			}
			if settings.DefaultView == "" {
				settings.DefaultView = "list"
			}
			settingsJSON, _ := json.Marshal(settings)

			input := models.CollectionCreate{
				Name:     name,
				Icon:     icon,
				Description: description,
				Schema:   string(schemaJSON),
				Settings: string(settingsJSON),
			}

			coll, err := client.CreateCollection(ws, input)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(coll)
			}

			collIcon := coll.Icon
			if collIcon == "" {
				collIcon = "📦"
			}
			fmt.Printf("Created collection %s %s (slug: %s)\n", collIcon, coll.Name, coll.Slug)
			return nil
		},
	}

	cmd.Flags().StringVar(&icon, "icon", "", "collection emoji icon")
	cmd.Flags().StringVar(&description, "description", "", "collection description")
	cmd.Flags().StringVar(&fieldsDSL, "fields", "", "field definitions (key:type[:options]; ...)")
	cmd.Flags().StringVar(&layout, "layout", "fields-primary", "item detail layout: fields-primary, content-primary, balanced")
	cmd.Flags().StringVar(&defaultView, "default-view", "list", "default view type: list, board, table")
	cmd.Flags().StringVar(&boardGroup, "board-group-by", "status", "field to group by in board view")

	return cmd
}

// --- edit ---

func editCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <slug>",
		Short: "Open an item's content in $EDITOR",
		Long: `Open an item's rich content in your default editor. After editing
and saving, the content is updated in Pad.

Set EDITOR or VISUAL env var to choose your editor (default: vi).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			ws := getWorkspace()
			slug := args[0]

			item, err := client.GetItem(ws, slug)
			if err != nil {
				return err
			}

			edited, err := cli.OpenInEditor(cfg, item.Content, ".md")
			if err != nil {
				return err
			}

			if edited == item.Content {
				fmt.Println("No changes.")
				return nil
			}

			updated, err := client.UpdateItem(ws, slug, models.ItemUpdate{
				Content:        &edited,
				LastModifiedBy: "user",
				Source:         "cli",
			})
			if err != nil {
				return err
			}

			fmt.Printf("Updated %q (%s)\n", updated.Title, updated.Slug)
			return nil
		},
	}
}

// --- utility ---

// completeCollectionNames provides shell completion for collection names.
func completeCollectionNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	// Static list of common collection names (singular + plural)
	names := []string{"task", "tasks", "idea", "ideas", "phase", "phases", "doc", "docs", "bug", "bugs"}
	// Try to fetch dynamic collections from API
	cfg, err := config.Load()
	if err == nil {
		if cli.EnsureServer(cfg) == nil {
			client := cli.NewClientFromURL(cfg.BaseURL())
			if ws, err := cli.DetectWorkspace(workspaceFlag); err == nil {
				if colls, err := client.ListCollections(ws); err == nil {
					names = nil
					for _, c := range colls {
						names = append(names, c.Slug)
					}
				}
			}
		}
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// normalizeCollectionSlug maps common singular/short forms to actual collection slugs.
func normalizeCollectionSlug(input string) string {
	aliases := map[string]string{
		"task": "tasks", "t": "tasks",
		"idea": "ideas", "i": "ideas",
		"phase": "phases", "p": "phases",
		"doc": "docs", "d": "docs",
		"bug": "bugs",
		"convention": "conventions",
		"playbook": "playbooks",
	}
	if mapped, ok := aliases[input]; ok {
		return mapped
	}
	return input
}

// --- library ---

func libraryCmd() *cobra.Command {
	var categoryFilter string
	var typeFilter string

	cmd := &cobra.Command{
		Use:   "library",
		Short: "Browse and activate pre-built conventions and playbooks",
		Long: `Browse the convention and playbook libraries and activate items in your workspace.

Examples:
  pad library                          # List both conventions and playbooks
  pad library --type conventions       # List conventions only
  pad library --type playbooks         # List playbooks only
  pad library --category git           # Filter by category
  pad library --format json            # JSON output
  pad library activate "Commit after task completion"  # Activate a convention or playbook`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			showConventions := typeFilter == "" || typeFilter == "conventions"
			showPlaybooks := typeFilter == "" || typeFilter == "playbooks"

			if showConventions {
				lib, err := client.GetConventionLibrary()
				if err != nil {
					return err
				}

				if formatFlag == "json" && !showPlaybooks {
					return cli.PrintJSON(lib)
				}

				fmt.Printf("\n=== CONVENTIONS ===\n")
				for _, cat := range lib.Categories {
					if categoryFilter != "" && cat.Name != categoryFilter {
						continue
					}
					fmt.Printf("\n%s (%s)\n", strings.ToUpper(cat.Name), cat.Description)
					fmt.Println(strings.Repeat("─", 60))

					for _, conv := range cat.Conventions {
						priorityTag := ""
						switch conv.Priority {
						case "must":
							priorityTag = " [MUST]"
						case "should":
							priorityTag = " [SHOULD]"
						case "nice-to-have":
							priorityTag = " [NICE]"
						}
						fmt.Printf("  %-45s %s%s\n", conv.Title, conv.Trigger, priorityTag)
					}
				}
			}

			if showPlaybooks {
				plib, err := client.GetPlaybookLibrary()
				if err != nil {
					return err
				}

				if formatFlag == "json" && !showConventions {
					return cli.PrintJSON(plib)
				}

				fmt.Printf("\n=== PLAYBOOKS ===\n")
				for _, cat := range plib.Categories {
					if categoryFilter != "" && cat.Name != categoryFilter {
						continue
					}
					fmt.Printf("\n%s (%s)\n", strings.ToUpper(cat.Name), cat.Description)
					fmt.Println(strings.Repeat("─", 60))

					for _, pb := range cat.Playbooks {
						fmt.Printf("  %-45s %s [%s]\n", pb.Title, pb.Trigger, pb.Scope)
					}
				}
			}

			if formatFlag == "json" && showConventions && showPlaybooks {
				lib, _ := client.GetConventionLibrary()
				plib, _ := client.GetPlaybookLibrary()
				return cli.PrintJSON(map[string]interface{}{
					"conventions": lib,
					"playbooks":   plib,
				})
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().StringVar(&categoryFilter, "category", "", "filter by category")
	cmd.Flags().StringVar(&typeFilter, "type", "", "filter by type: conventions, playbooks")

	cmd.AddCommand(libraryActivateCmd())
	return cmd
}

func libraryActivateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "activate <title>",
		Short: "Activate a library convention or playbook in the current workspace",
		Long: `Look up a convention or playbook in the library by title and create it as an item
in the appropriate collection (conventions or playbooks) with all fields set.

Examples:
  pad library activate "Commit after task completion"    # Activates a convention
  pad library activate "Implementation Workflow"         # Activates a playbook`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			title := args[0]

			// First check conventions library
			lib, err := client.GetConventionLibrary()
			if err != nil {
				return err
			}

			var foundConvention *cli.LibraryConvention
			for _, cat := range lib.Categories {
				for i := range cat.Conventions {
					if cat.Conventions[i].Title == title {
						foundConvention = &cat.Conventions[i]
						break
					}
				}
				if foundConvention != nil {
					break
				}
			}

			if foundConvention != nil {
				// Build fields JSON for convention
				fields := map[string]interface{}{
					"status":   "active",
					"trigger":  foundConvention.Trigger,
					"scope":    foundConvention.Scope,
					"priority": foundConvention.Priority,
				}
				fieldsJSON, _ := json.Marshal(fields)

				input := models.ItemCreate{
					Title:     foundConvention.Title,
					Content:   foundConvention.Content,
					Fields:    string(fieldsJSON),
					CreatedBy: "user",
					Source:    "cli",
				}

				item, err := client.CreateItem(ws, "conventions", input)
				if err != nil {
					return err
				}

				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}

				fmt.Printf("Activated convention: %s (%s)\n", item.Title, item.Slug)
				return nil
			}

			// Then check playbooks library
			plib, err := client.GetPlaybookLibrary()
			if err != nil {
				return err
			}

			var foundPlaybook *cli.LibraryPlaybook
			for _, cat := range plib.Categories {
				for i := range cat.Playbooks {
					if cat.Playbooks[i].Title == title {
						foundPlaybook = &cat.Playbooks[i]
						break
					}
				}
				if foundPlaybook != nil {
					break
				}
			}

			if foundPlaybook != nil {
				// Build fields JSON for playbook
				fields := map[string]interface{}{
					"status":  "active",
					"trigger": foundPlaybook.Trigger,
					"scope":   foundPlaybook.Scope,
				}
				fieldsJSON, _ := json.Marshal(fields)

				input := models.ItemCreate{
					Title:     foundPlaybook.Title,
					Content:   foundPlaybook.Content,
					Fields:    string(fieldsJSON),
					CreatedBy: "user",
					Source:    "cli",
				}

				item, err := client.CreateItem(ws, "playbooks", input)
				if err != nil {
					return err
				}

				if formatFlag == "json" {
					return cli.PrintJSON(item)
				}

				fmt.Printf("Activated playbook: %s (%s)\n", item.Title, item.Slug)
				return nil
			}

			return fmt.Errorf("not found in convention or playbook library: %q", title)
		},
	}
}

// --- export ---

func exportCmd() *cobra.Command {
	var outputFile string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export workspace to JSON",
		Long:  `Export the current workspace (collections, items, comments, versions) to a portable JSON file.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			resp, err := client.RawGet("/workspaces/" + ws + "/export")
			if err != nil {
				return fmt.Errorf("export: %w", err)
			}

			if outputFile != "" {
				if err := os.WriteFile(outputFile, resp, 0644); err != nil {
					return fmt.Errorf("write file: %w", err)
				}
				fmt.Printf("Exported workspace %q to %s\n", ws, outputFile)
			} else {
				os.Stdout.Write(resp)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "output file path (default: stdout)")
	return cmd
}

// --- import ---

func importCmd() *cobra.Command {
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Import workspace from JSON export",
		Long:  `Import a workspace from a previously exported JSON file. Creates a new workspace with regenerated IDs.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			filePath := args[0]

			data, err := os.ReadFile(filePath)
			if err != nil {
				return fmt.Errorf("read file: %w", err)
			}

			path := "/workspaces/import"
			if nameFlag != "" {
				path += "?name=" + nameFlag
			}

			var ws models.Workspace
			if err := client.PostRaw(path, data, &ws); err != nil {
				return fmt.Errorf("import: %w", err)
			}

			fmt.Printf("Imported workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			fmt.Printf("  Collections: imported\n")
			fmt.Printf("  Items, comments, links, versions: imported\n")
			fmt.Printf("  All IDs regenerated\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&nameFlag, "name", "", "override workspace name")
	return cmd
}

func extractFieldFromJSON(fieldsJSON, key string) string {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return ""
	}
	var fields map[string]interface{}
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return ""
	}
	val, exists := fields[key]
	if !exists {
		return ""
	}
	switch v := val.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.0f", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}
