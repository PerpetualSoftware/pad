package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	pad "github.com/xarmian/pad"
	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/collections"
	"github.com/xarmian/pad/internal/config"
	"github.com/xarmian/pad/internal/models"
)

func padInitCmd() *cobra.Command {
	var templateFlag string

	cmd := &cobra.Command{
		Use:   "init [name]",
		Short: "Set up Pad — configure, authenticate, and create a workspace",
		Long: `Initialize Pad for this project. This smart command detects what's needed and
walks you through each step:

  1. Configure connection (local server, remote, or Docker)
  2. Start local server if needed
  3. Create the first admin account (fresh installs)
  4. Log in if not authenticated
  5. Create or link a workspace for the current directory
  6. Install/update the /pad skill for detected AI tools

Safe to re-run anytime — it skips steps that are already done and shows
your current status.

Examples:
  pad init                    # Auto-detect everything, use directory name
  pad init myproject          # Specify workspace name
  pad init --template scrum   # Use scrum template for new workspace`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			// Install the SIGINT/SIGTERM handler first so the user can
			// abort cleanly at any interactive prompt. Defers run in LIFO
			// order: the cancellation check fires before the cleanup
			// removes the signal listener, so a sentinel error propagated
			// from a prompt is converted into the canonical exit before
			// returning to cobra.
			cleanup := installInitCancelHandler()
			defer cleanup()
			defer func() {
				if isCancellation(retErr) {
					cancelInit()
				}
			}()

			// Validate template name up front before any state changes
			if templateFlag != "" {
				tmpl := collections.GetTemplate(templateFlag)
				if tmpl == nil {
					fmt.Fprintf(os.Stderr, "Unknown template: %s\n\n", templateFlag)
					fmt.Fprintln(os.Stderr, "Available templates:")
					fmt.Fprintln(os.Stderr)
					printGroupedTemplates(os.Stderr)
					return fmt.Errorf("unknown template %q", templateFlag)
				}
			}

			green := color.New(color.FgGreen)
			bold := color.New(color.Bold)

			// Track whether we performed any actions (vs everything already set up)
			actioned := false

			// ── Step 1: Configuration ──────────────────────────────────
			cfg := getConfig()
			if !cfg.IsConfigured() {
				if !canPromptForConfig() {
					return fmt.Errorf("Pad is not configured. Run 'pad auth configure' first, or run 'pad init' in an interactive terminal")
				}
				fmt.Println("Welcome to Pad! Let's get you set up.")
				fmt.Println()
				fmt.Println(bold.Sprint("Step 1: Configure connection"))
				fmt.Println()
				if err := runConfigureFlow(cfg, configureValues{}); err != nil {
					return fmt.Errorf("configure: %w", err)
				}
				// Reload config after saving
				cfg = getConfig()
				green.Print("✓ ")
				fmt.Printf("Configured: %s mode", cfg.Mode)
				if cfg.Mode == config.ModeLocal {
					fmt.Printf(" (%s)", cfg.Addr())
				} else {
					fmt.Printf(" (%s)", cfg.BaseURL())
				}
				fmt.Println()
				fmt.Println()
				actioned = true
			}

			// ── Step 2: Ensure server is running ──────────────────────
			if err := cli.EnsureServer(cfg); err != nil {
				return fmt.Errorf("start server: %w", err)
			}

			client := cli.NewClientFromURL(cfg.BaseURL())

			// ── Step 3: First-time setup (bootstrap) ──────────────────
			session, err := client.CheckSession()
			if err != nil {
				return fmt.Errorf("failed to connect to server at %s: %w", cfg.BaseURL(), err)
			}

			if session.SetupRequired {
				// Only local mode can bootstrap inline
				if cfg.Mode != config.ModeLocal && cfg.Mode != "" {
					printSetupRequiredHint(cfg)
					return fmt.Errorf("this Pad instance has not been initialized yet")
				}

				if !canPromptForConfig() {
					printSetupRequiredHint(cfg)
					return fmt.Errorf("this Pad instance has not been initialized yet (run 'pad auth setup' in an interactive terminal)")
				}

				if actioned {
					fmt.Println(bold.Sprint("Step 2: Create admin account"))
				} else {
					fmt.Println(bold.Sprint("Create admin account"))
				}
				fmt.Println()
				email, name, password, err := promptForAccountDetails()
				if err != nil {
					return fmt.Errorf("setup: %w", err)
				}
				resp, err := client.Bootstrap(email, name, password)
				if err != nil {
					return fmt.Errorf("setup failed: %w", err)
				}
				if err := saveCredentials(cfg, resp); err != nil {
					return err
				}
				green.Print("✓ ")
				fmt.Printf("Admin account created — logged in as %s (%s)\n", resp.User.Name, resp.User.Email)
				fmt.Println()

				// Refresh client with credentials
				client = cli.NewClientFromURL(cfg.BaseURL())
				actioned = true
			}

			// ── Step 4: Authentication ────────────────────────────────
			if !session.Authenticated && !session.SetupRequired {
				// Check if we have saved credentials that still work
				creds, _ := cli.LoadCredentials()
				if creds != nil && creds.Token != "" {
					client.SetAuthToken(creds.Token)
					user, err := client.GetCurrentUser()
					if err == nil && user != nil {
						// Credentials are still valid, we're good
						goto authenticated
					}
				}

				if actioned {
					fmt.Println(bold.Sprint("Step 3: Log in"))
				} else {
					fmt.Println("Log in to continue.")
				}
				fmt.Println()
				if err := doBrowserLogin(client, cfg); err != nil {
					return fmt.Errorf("login: %w", err)
				}
				fmt.Println()
				client = cli.NewClientFromURL(cfg.BaseURL())
				actioned = true
			}

		authenticated:

			// ── Step 5: Workspace ─────────────────────────────────────
			cwd, _ := os.Getwd()
			var wsName string
			if len(args) > 0 {
				wsName = args[0]
			} else {
				wsName = filepath.Base(cwd)
			}

			ws, newlyCreated, err := ensureWorkspace(client, cfg, cwd, wsName, templateFlag)
			if err != nil {
				return err
			}
			if newlyCreated {
				actioned = true
			}

			// ── Step 6: Skill files ───────────────────────────────────
			skillResults := ensureSkills()
			if skillResults.installed > 0 || skillResults.updated > 0 {
				actioned = true
			}

			// ── Status summary ────────────────────────────────────────
			if !actioned {
				printInitStatus(client, cfg, ws, skillResults)
			} else if newlyCreated {
				printOnboardingHints(cfg)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&templateFlag, "template", "", "workspace template (omit for interactive picker; run 'pad workspace init --list-templates' to see all)")
	return cmd
}

// ── Shared helpers ────────────────────────────────────────────────────────────

// ensureWorkspace checks if the current directory is linked to a workspace.
// If not, it creates or links one. Returns the workspace, whether it was newly
// created, and any error.
func ensureWorkspace(client *cli.Client, cfg *config.Config, cwd, name, templateFlag string) (*models.Workspace, bool, error) {
	green := color.New(color.FgGreen)
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)

	// Check if already linked
	existingSlug, err := cli.DetectWorkspace("")
	if err == nil {
		ws, err := client.GetWorkspace(existingSlug)
		if err == nil && ws != nil {
			return ws, false, nil
		}
		// Linked but workspace doesn't exist on server — fall through to create/link
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

	if ws != nil {
		if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
			return nil, false, fmt.Errorf("write .pad.toml: %w", err)
		}
		green.Print("✓ ")
		fmt.Printf("Linked to existing workspace %s %s\n",
			bold.Sprint(ws.Name),
			dim.Sprintf("(slug: %s)", ws.Slug))
		return ws, false, nil
	}

	// Create new workspace. When the caller didn't pass --template:
	//   - If stdin/stdout are TTYs, prompt interactively with the grouped
	//     template picker.
	//   - Otherwise fall back to the "startup" default so scripts and
	//     non-interactive runs get the curated starter pack.
	// Tests and other API callers that want an empty workspace can still
	// POST with Template="" directly.
	effectiveTemplate := templateFlag
	if effectiveTemplate == "" {
		if canPromptForTemplate() {
			picked, perr := pickTemplateInteractive(os.Stdin, os.Stdout)
			if perr != nil {
				return nil, false, perr
			}
			effectiveTemplate = picked
		} else {
			effectiveTemplate = defaultTemplateName
		}
	}
	ws, err = client.CreateWorkspace(models.WorkspaceCreate{
		Name:     name,
		Template: effectiveTemplate,
	})
	if err != nil {
		return nil, false, fmt.Errorf("create workspace: %w", err)
	}

	if err := cli.WriteWorkspaceLink(cwd, ws.Slug); err != nil {
		return nil, false, fmt.Errorf("write .pad.toml: %w", err)
	}

	tmplMsg := ""
	if templateFlag != "" && templateFlag != "startup" {
		tmplMsg = dim.Sprintf(" with %s template", templateFlag)
	}
	green.Print("✓ ")
	fmt.Printf("Created workspace %s %s%s\n",
		bold.Sprint(ws.Name),
		dim.Sprintf("(slug: %s)", ws.Slug),
		tmplMsg)
	fmt.Printf("  Linked to %s\n", bold.Sprint(cwd))

	return ws, true, nil
}

// skillResult tracks what ensureSkills did.
type skillResult struct {
	installed int
	updated   int
	upToDate  int
	tools     []string // labels of all detected+installed tools
}

// ensureSkills detects AI tools, installs missing skill files, and updates
// outdated ones. Returns a summary of what it did.
func ensureSkills() skillResult {
	green := color.New(color.FgGreen)
	dim := color.New(color.Faint)
	result := skillResult{}

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

	for _, tool := range detected {
		expected := cli.FormatForTool(tool, pad.PadSkill)

		if cli.ToolInstalled(tool) {
			// Check if content is up to date
			path := cli.ToolSkillPath(tool)
			existing, err := os.ReadFile(path)
			if err == nil && bytes.Equal(existing, expected) {
				result.upToDate++
				result.tools = append(result.tools, tool.Label)
				continue
			}

			// Outdated — update silently
			path, err = cli.InstallForTool(tool, expected)
			if err != nil {
				continue
			}
			green.Print("✓ ")
			fmt.Printf("Updated /pad skill for %s %s\n", tool.Label, dim.Sprint("→ "+path))
			recordInstallation(tool.Name, path)
			result.updated++
			result.tools = append(result.tools, tool.Label)
		} else {
			// Not installed — install. In interactive mode this proceeds
			// without prompting because it's part of the init flow and the
			// user already opted in.
			path, err := cli.InstallForTool(tool, expected)
			if err != nil {
				continue
			}
			green.Print("✓ ")
			fmt.Printf("Installed /pad skill for %s %s\n", tool.Label, dim.Sprint("→ "+path))
			recordInstallation(tool.Name, path)
			result.installed++
			result.tools = append(result.tools, tool.Label)
		}
	}

	return result
}

// printInitStatus prints a clean status summary when everything is already configured.
func printInitStatus(client *cli.Client, cfg *config.Config, ws *models.Workspace, skills skillResult) {
	green := color.New(color.FgGreen)
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)

	fmt.Println()
	fmt.Println(bold.Sprint("Pad is ready."))
	fmt.Println()

	// Server
	green.Print("  ✓ Server     ")
	serverAddr := cfg.BaseURL()
	if cfg.Mode == config.ModeLocal {
		serverAddr = cfg.Addr()
	}
	fmt.Println(serverAddr)

	// Auth
	creds, _ := cli.LoadCredentials()
	if creds != nil && creds.Email != "" {
		green.Print("  ✓ Logged in  ")
		fmt.Println(creds.Email)
	}

	// Workspace
	if ws != nil {
		green.Print("  ✓ Workspace  ")
		fmt.Print(bold.Sprint(ws.Name))

		// Try to get workspace stats from dashboard
		dashJSON, err := client.GetDashboard(ws.Slug)
		if err == nil {
			var dash struct {
				Summary struct {
					TotalItems   int                       `json:"total_items"`
					ByCollection map[string]map[string]int `json:"by_collection"`
				} `json:"summary"`
			}
			if json.Unmarshal(dashJSON, &dash) == nil && dash.Summary.TotalItems > 0 {
				// Count open + in-progress tasks
				taskStats := dash.Summary.ByCollection["tasks"]
				open := taskStats["open"]
				inProgress := taskStats["in-progress"]
				parts := []string{}
				if open > 0 {
					parts = append(parts, fmt.Sprintf("%d open", open))
				}
				if inProgress > 0 {
					parts = append(parts, fmt.Sprintf("%d in progress", inProgress))
				}
				if len(parts) > 0 {
					fmt.Print(dim.Sprintf(" (%s)", strings.Join(parts, ", ")))
				} else {
					fmt.Print(dim.Sprintf(" (%d items)", dash.Summary.TotalItems))
				}
			}
		}
		fmt.Println()
	}

	// Skills
	if len(skills.tools) > 0 {
		green.Print("  ✓ Skills     ")
		fmt.Print(strings.Join(skills.tools, ", "))
		if skills.updated > 0 {
			fmt.Print(dim.Sprintf(" (%d updated)", skills.updated))
		}
		fmt.Println()
	}

	// Version
	green.Print("  ✓ Version    ")
	fmt.Println(fullVersion())

	fmt.Println()
}
