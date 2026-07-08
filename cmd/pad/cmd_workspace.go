package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	pad "github.com/PerpetualSoftware/pad"
	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/config"

	"github.com/PerpetualSoftware/pad/internal/models"
	"golang.org/x/term"
)

func storageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "storage",
		Short: "Show workspace storage usage and effective limit",
		Long: `Print the workspace's current attachment storage usage versus the
effective limit for the workspace owner's plan.

The effective limit follows the resolution chain:
  1. Per-user storage_bytes override (admin-set)
  2. Platform plan_limits_<plan>_storage_bytes setting
  3. Hardcoded plan default (free=500MB, pro=10GB)

A limit of "unlimited" indicates no enforced cap (pro / self-hosted /
workspaces with no owner).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			raw, err := client.RawGet("/workspaces/" + ws + "/storage/usage")
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				fmt.Println(string(raw))
				return nil
			}

			var resp struct {
				UsedBytes      int64  `json:"used_bytes"`
				LimitBytes     int64  `json:"limit_bytes"`
				Plan           string `json:"plan"`
				OverrideActive bool   `json:"override_active"`
			}
			if err := json.Unmarshal(raw, &resp); err != nil {
				return fmt.Errorf("decode storage usage: %w", err)
			}

			used := humanBytes(resp.UsedBytes)
			if resp.LimitBytes < 0 {
				fmt.Printf("%s used (unlimited)\n", used)
			} else {
				pct := 0.0
				if resp.LimitBytes > 0 {
					pct = float64(resp.UsedBytes) / float64(resp.LimitBytes) * 100
				}
				fmt.Printf("%s used of %s (%.1f%%)\n", used, humanBytes(resp.LimitBytes), pct)
			}
			plan := resp.Plan
			if plan == "" {
				plan = "(none)"
			}
			suffix := ""
			if resp.OverrideActive {
				suffix = " — admin override active"
			}
			fmt.Printf("Plan: %s%s\n", plan, suffix)

			return nil
		},
	}
	return cmd
}

func membersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "members",
		Short: "List workspace members",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			var result struct {
				Members     json.RawMessage `json:"members"`
				Invitations json.RawMessage `json:"invitations"`
			}
			raw, err := client.RawGet("/workspaces/" + ws + "/members")
			if err != nil {
				return err
			}
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}

			if formatFlag == "json" {
				fmt.Println(string(raw))
				return nil
			}

			var members []struct {
				UserName  string `json:"user_name"`
				UserEmail string `json:"user_email"`
				Role      string `json:"role"`
			}
			json.Unmarshal(result.Members, &members)

			if len(members) == 0 {
				fmt.Println("No members (workspace has no users yet)")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tEMAIL\tROLE")
			for _, m := range members {
				fmt.Fprintf(w, "%s\t%s\t%s\n", m.UserName, m.UserEmail, m.Role)
			}
			w.Flush()

			var invitations []struct {
				Email string `json:"email"`
				Role  string `json:"role"`
				Code  string `json:"code"`
			}
			json.Unmarshal(result.Invitations, &invitations)

			if len(invitations) > 0 {
				fmt.Println()
				fmt.Println("Pending invitations:")
				for _, inv := range invitations {
					fmt.Printf("  %s (%s) — join code: %s\n", inv.Email, inv.Role, inv.Code)
				}
			}

			return nil
		},
	}
	return cmd
}

func inviteCmd() *cobra.Command {
	var roleFlag string

	cmd := &cobra.Command{
		Use:   "invite <email>",
		Short: "Invite a user to the workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			email := args[0]

			var result map[string]interface{}
			raw, err := json.Marshal(map[string]string{
				"email": email,
				"role":  roleFlag,
			})
			if err != nil {
				return err
			}
			if err := client.PostRaw("/workspaces/"+ws+"/members/invite", raw, &result); err != nil {
				// TASK-788: emit structured marker so MCP stdio classifier
				// can surface ErrPlanLimitExceeded instead of ErrServerError.
				if apiErr, ok := err.(*cli.APIError); ok {
					if apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("invite blocked: plan limit reached")
					}
				}
				return err
			}

			green := color.New(color.FgGreen).SprintFunc()

			if added, ok := result["added"].(bool); ok && added {
				name, _ := result["name"].(string)
				role, _ := result["role"].(string)
				fmt.Printf("%s Added %s (%s) as %s\n", green("✓"), name, email, role)
			} else {
				role, _ := result["role"].(string)
				fmt.Printf("%s Invitation created for %s (%s)\n", green("✓"), email, role)
				if joinURL, ok := result["join_url"].(string); ok && joinURL != "" {
					fmt.Printf("  Share this link: %s\n", joinURL)
				} else {
					code, _ := result["code"].(string)
					fmt.Printf("  Join code: %s\n", code)
					fmt.Printf("  They can accept with: pad workspace join %s\n", code)
				}
			}

			return nil
		},
	}
	cmd.Flags().StringVar(&roleFlag, "role", "editor", "role for the invited user (owner, editor, viewer)")
	return cmd
}

func joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <code>",
		Short: "Accept a workspace invitation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			code := args[0]

			var result map[string]interface{}
			if err := client.PostRaw("/invitations/"+code+"/accept", nil, &result); err != nil {
				return fmt.Errorf("failed to accept invitation: %w", err)
			}

			green := color.New(color.FgGreen).SprintFunc()
			role, _ := result["role"].(string)
			fmt.Printf("%s Joined workspace as %s\n", green("✓"), role)
			return nil
		},
	}
}

// workspaceRestoreCmd un-soft-deletes a workspace by slug, provided it is
// still inside the 30-day restore window (owner-only server-side).
func workspaceRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <slug>",
		Short: "Restore a soft-deleted workspace within the restore window",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			slug := args[0]

			ws, err := client.RestoreWorkspace(slug)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(ws)
			}

			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s Restored workspace %s (%s)\n", green("✓"), ws.Name, ws.Slug)
			return nil
		},
	}
}

// workspaceDeletedCmd lists the caller's soft-deleted workspaces that are
// still restorable — i.e. not yet past the purge window — along with the
// days remaining before each is permanently purged.
func workspaceDeletedCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deleted",
		Short: "List soft-deleted workspaces still within the restore window",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			deleted, err := client.ListDeletedWorkspaces()
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(deleted)
			}

			if len(deleted) == 0 {
				fmt.Println("No deleted workspaces within the restore window.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSLUG\tDELETED\tDAYS LEFT")
			for _, ws := range deleted {
				deletedAt := "—"
				if ws.DeletedAt != nil {
					deletedAt = ws.DeletedAt.Format("Jan 2, 2006")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", ws.Name, ws.Slug, deletedAt, ws.DaysLeft)
			}
			w.Flush()

			fmt.Println()
			fmt.Println("Restore one with: pad workspace restore <slug>")
			return nil
		},
	}
}

// --- workspace create (non-interactive) ---
//
// `pad workspace init` is the interactive entry point: it ensures
// auth, prompts when needed, and CWD-links the new workspace. That's
// the right shape for a human at a terminal. The MCP `pad_workspace.
// action: create` route needs a non-interactive equivalent that hits
// POST /api/v1/workspaces directly with structured args — no TTY
// prompts, no CWD-link side effect. `pad workspace create` is that
// surface; the stdio MCP dispatcher (ExecDispatcher) shells out to
// it via passThrough, and humans can call it too if they want the
// thin direct shape.
//
// See PLAN-1519 / TASK-1521 / IDEA-1517 §1.

func workspaceCreateCmd() *cobra.Command {
	var (
		slugFlag     string
		templateFlag string
	)
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a workspace non-interactively (use 'pad workspace init' for the guided flow)",
		Long: `Create a new workspace by name. Non-interactive — no prompts, no
CWD link side effect. Hits POST /api/v1/workspaces directly with the
supplied name + optional slug + template.

For the guided flow that ensures auth, picks a template interactively,
and links the CWD, use 'pad workspace init' instead. The 'create' shape
exists to back the MCP pad_workspace.action: create route + scripts
that just want the workspace row.

When called over an OAuth-bound MCP session whose grant has
may_create_workspaces=true, the new workspace is auto-added to that
connection's allow-list (PLAN-1519 / TASK-1521 / IDEA-1517 §1) so
the agent can use it immediately without re-auth.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws, err := client.CreateWorkspace(models.WorkspaceCreate{
				Name:     args[0],
				Slug:     slugFlag,
				Template: templateFlag,
			})
			if err != nil {
				// TASK-788: emit structured marker before wrapping so the MCP
				// stdio classifier can surface ErrPlanLimitExceeded with details.
				if apiErr, ok := err.(*cli.APIError); ok {
					if apiErr.AsPlanLimit() != nil {
						cli.WritePlanLimitError(os.Stderr, apiErr)
						return fmt.Errorf("workspace creation blocked: plan limit reached")
					}
				}
				return fmt.Errorf("create workspace: %w", err)
			}
			if formatFlag == "json" {
				return cli.PrintJSON(ws)
			}
			green := color.New(color.FgGreen).SprintFunc()
			fmt.Printf("%s Created workspace %q (slug: %s)\n", green("✓"), ws.Name, ws.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&slugFlag, "slug", "", "Workspace slug (default: derived from name)")
	cmd.Flags().StringVar(&templateFlag, "template", "", "Template to seed collections from (run 'pad workspace init --list-templates' to see options)")
	return cmd
}

// --- workspace claim ---
//
// `pad workspace claim <code> --workspace <slug>` redeems a 6-digit
// stateless HMAC claim code at POST /api/v1/oauth/claim, granting the
// calling OAuth connection access to one specific workspace.
//
// The CLI ships this command primarily so the stdio MCP dispatcher
// (ExecDispatcher) can shell out to it via passThrough for the
// `pad_workspace.action: claim` route — claim itself is meaningful
// only over a cloud-mode OAuth MCP session (PAT / CLI session tokens
// already see every workspace the user belongs to), but the handler
// returns a clear note when called outside that context so a confused
// CLI caller isn't left guessing.
//
// See PLAN-1519 / TASK-1521 / IDEA-1517 §4.

func workspaceClaimCmd() *cobra.Command {
	var workspaceSlug string
	cmd := &cobra.Command{
		Use:   "claim <code>",
		Short: "Redeem a 6-digit claim code to add a workspace to this OAuth connection's allow-list",
		Long: `Redeem a 6-digit claim code at POST /api/v1/oauth/claim, granting the
calling OAuth connection access to one specific workspace.

Generate the code in the workspace's web UI ("Connect project" modal,
avatar menu). The code is valid for 5–10 minutes (sliding window).

Claim is meaningful only over cloud-mode OAuth MCP sessions where
workspace allow-lists actually constrain access. CLI session tokens
and PATs already see every workspace you belong to; running this from
the CLI succeeds (the code verifies) but the side effect no-ops —
the response 'note' field explains.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if workspaceSlug == "" {
				workspaceSlug = getWorkspace()
			}
			if workspaceSlug == "" {
				return fmt.Errorf("--workspace is required (no workspace linked in this directory)")
			}
			client, _ := getClient()
			resp, err := client.ClaimWorkspace(workspaceSlug, args[0])
			if err != nil {
				return fmt.Errorf("claim workspace: %w", err)
			}
			if formatFlag == "json" {
				return cli.PrintJSON(resp)
			}
			green := color.New(color.FgGreen).SprintFunc()
			dim := color.New(color.Faint)
			if resp.AlreadyAdded {
				fmt.Printf("%s Workspace %q already in this connection's allow-list (no change)\n",
					green("✓"), resp.Workspace)
			} else {
				fmt.Printf("%s Claimed workspace %q\n", green("✓"), resp.Workspace)
			}
			if resp.Note != "" {
				fmt.Println(dim.Sprint(resp.Note))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&workspaceSlug, "workspace", "", "Workspace slug to claim (defaults to CWD-linked workspace)")
	return cmd
}

// --- reset-password ---

func initCmd() *cobra.Command {
	var templateFlag string
	var listTemplates bool

	cmd := &cobra.Command{
		Use:   "init [name]",
		Short: "Create a workspace and link it to the current directory",
		Long: `Create a workspace and link it to the current directory.

Use --template to choose a workspace template:
  pad workspace init myproject --template scrum
  pad workspace init myproject --template blank    # build it out via /pad onboard

The 'blank' template ships only the system collections (Conventions,
Playbooks) plus the canonical /pad onboard playbook. Run /pad onboard
inside the workspace and the agent will adapt collections, conventions,
roles, and playbooks to whatever your project actually is.

Use --list-templates to see available templates.

Tip: 'pad init' handles everything — configure, authenticate, and create
a workspace in one step.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			// Same SIGINT/SIGTERM handling as 'pad init'. Installed BEFORE
			// any interactive prompt so the user can abort cleanly. The
			// LIFO defer order ensures the cancellation check converts
			// errCancelled into the canonical exit before cleanup
			// detaches the signal listener.
			cleanup := installInitCancelHandler()
			defer cleanup()
			defer func() {
				if isCancellation(retErr) {
					cancelInit()
				}
			}()

			// Handle --list-templates
			if listTemplates {
				fmt.Println("Available templates:")
				fmt.Println()
				printGroupedTemplates(os.Stdout)
				return nil
			}

			// Validate template name if provided
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

			cfg := getConfiguredConfig()
			if err := cli.EnsureServer(cfg); err != nil {
				return err
			}
			client := cli.NewClientFromURL(cfg.BaseURL())
			cwd, _ := os.Getwd()

			// Ensure the user is authenticated before proceeding
			session, err := client.CheckSession()
			if err != nil {
				return fmt.Errorf("failed to check auth status: %w", err)
			}
			if session.SetupRequired {
				// Remote/cloud instances can only be bootstrapped from the
				// server host — a client machine can't create the first
				// admin. Keep the pointing-at-the-host hint for those.
				switch cfg.Mode {
				case config.ModeRemote, config.ModeCloud:
					printSetupRequiredHint(cfg)
					return fmt.Errorf("this Pad instance has not been initialized yet")
				}
				// Fresh local instance: drive the full first-run setup
				// (create the first admin + authorize this CLI) inline so
				// `pad init` is a genuine one-shot rather than bouncing the
				// user to `pad auth setup` and back. BUG-1843.
				fmt.Println("This Pad instance hasn't been set up yet — let's create your admin account.")
				if err := runBrowserSetup(cmd.Context(), cfg, client); err != nil {
					return err
				}
				fmt.Println()
				client = cli.NewClientFromURL(cfg.BaseURL())
			} else if !session.Authenticated {
				fmt.Println("Log in to continue.")
				fmt.Println()
				if err := doBrowserLogin(client, cfg); err != nil {
					return err
				}
				fmt.Println()
				client = cli.NewClientFromURL(cfg.BaseURL())
			}

			var name string
			if len(args) > 0 {
				name = args[0]
			} else {
				name = filepath.Base(cwd)
			}

			if workspaceFlag != "" && len(args) > 0 {
				fmt.Fprintf(os.Stderr, "Note: --workspace %q overrides positional name %q.\n", workspaceFlag, args[0])
			}

			ws, newlyCreated, createdTemplate, err := ensureWorkspace(client, cfg, cwd, name, workspaceFlag, templateFlag)
			if err != nil {
				return err
			}

			if !newlyCreated && ws != nil {
				fmt.Printf("Already linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			}

			offerSkillInstall()

			if newlyCreated {
				printOnboardingHints(cfg, createdTemplate)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&templateFlag, "template", "", "workspace template (use --list-templates to see available templates by category)")
	cmd.Flags().BoolVar(&listTemplates, "list-templates", false, "list available workspace templates")

	return cmd
}

func linkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link <workspace>",
		Short: "Link the current directory to an existing workspace",
		Long: `Link the current directory to an existing workspace by creating a .pad.toml file.

Unlike 'pad workspace init', this does NOT create a new workspace — it only links to one that already exists.

  pad workspace link myproject

Use 'pad workspace list' to see available workspaces.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, cfg := getClient()
			cwd, _ := os.Getwd()
			nameOrSlug := args[0]

			// Check if already linked
			existingSlug, err := cli.DetectWorkspace("")
			if err == nil {
				ws, err := client.GetWorkspace(existingSlug)
				if err == nil && ws != nil {
					fmt.Printf("Already linked to workspace %q (slug: %s)\n", ws.Name, ws.Slug)
					offerSkillInstall()
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
				return fmt.Errorf("workspace %q does not exist — use 'pad workspace init %s' to create it", nameOrSlug, nameOrSlug)
			}

			// Use the cfg returned by getClient() — it carries the
			// .pad.toml URL override that getClient just used to
			// reach the server. Re-deriving from raw getConfig()
			// would drop the URL when relinking inside an existing
			// remote-pinned directory whose global config is local.
			if err := cli.WriteWorkspaceLink(cwd, ws.Slug, padTomlURLFor(cfg)); err != nil {
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
	// Detect tools and install for all detected ones
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

	// Check if any are already installed
	allInstalled := true
	for _, tool := range detected {
		if !cli.ToolInstalled(tool) {
			allInstalled = false
			break
		}
	}

	if allInstalled && len(detected) > 0 {
		// Ensure existing installations are tracked in the registry
		for _, tool := range detected {
			path := cli.ToolSkillPath(tool)
			if path != "" {
				recordInstallation(tool.Name, path)
			}
		}
		fmt.Printf("\n/pad skill already installed for %d tool(s). Run 'pad agent update' to update.\n", len(detected))
		return
	}

	if !cli.IsTerminal() {
		// Non-interactive: silently install for all detected tools
		fmt.Println()
		for _, tool := range detected {
			if cli.ToolInstalled(tool) {
				continue
			}
			content := cli.FormatForTool(tool, pad.PadSkill)
			path, err := cli.InstallForTool(tool, content)
			if err != nil {
				continue
			}
			fmt.Printf("Installed /pad skill for %s → %s\n", tool.Label, path)
			recordInstallation(tool.Name, path)
		}
		return
	}

	fmt.Println()
	if len(detected) == 1 {
		fmt.Printf("Install /pad skill for %s? (Y/n): ", detected[0].Label)
	} else {
		fmt.Println("Detected AI coding tools:")
		for _, tool := range detected {
			installed := ""
			if cli.ToolInstalled(tool) {
				installed = " (installed)"
			}
			fmt.Printf("  • %s%s\n", tool.Label, installed)
		}
		fmt.Printf("\nInstall /pad skill for all? (Y/n): ")
	}

	choice := readChoice()
	if choice == "n" || choice == "N" {
		fmt.Println("Skipped. Run 'pad agent install' later.")
		return
	}

	fmt.Println()
	for _, tool := range detected {
		if cli.ToolInstalled(tool) {
			// Already installed — just ensure it's tracked in the registry
			path := cli.ToolSkillPath(tool)
			if path != "" {
				recordInstallation(tool.Name, path)
			}
			continue
		}
		content := cli.FormatForTool(tool, pad.PadSkill)
		path, err := cli.InstallForTool(tool, content)
		if err != nil {
			color.New(color.FgRed).Fprintf(os.Stderr, "  ✗ %s: %v\n", tool.Label, err)
			continue
		}
		color.New(color.FgGreen).Printf("  ✓ %s", tool.Label)
		fmt.Printf(" → %s\n", color.New(color.Faint).Sprint(path))
		recordInstallation(tool.Name, path)
	}
}

func readChoice() string {
	var input string
	fmt.Scanln(&input)
	return strings.TrimSpace(input)
}

func printOnboardingHints(cfg *config.Config, templateName string) {
	bold := color.New(color.Bold)
	dim := color.New(color.Faint)
	cyan := color.New(color.FgCyan)

	_ = templateName // PLAN-1496 / TASK-1502: was used to surface the
	// IDEA-1 / BACK-1 / FEAT-1 ref per template; that whole pattern
	// retired. /pad onboard is the single entry point now.

	fmt.Println()
	bold.Println("Get started:")
	fmt.Printf("  %s %s\n", cyan.Sprint("/pad"), "onboard")
	fmt.Println(dim.Sprint("    (open an agent session in this directory and run that command)"))
	fmt.Println()
	fmt.Printf("Or open the web UI at %s\n", bold.Sprint(cfg.BrowserURL()))
	fmt.Println(dim.Sprint("Run 'pad project dashboard' to see your project dashboard"))
}

// --- onboard ---
// The 'pad onboard' Cobra subcommand was retired in PLAN-1496 /
// TASK-1502. The pre-existing implementation scanned the project
// directory for build/test/CI markers and offered to seed library
// conventions — useful behavior, but CLI-only and not reachable from
// pure-MCP agent surfaces. The /pad onboard playbook (TASK-1499)
// is the replacement; it works through whatever surface the agent
// has and is auto-seeded into every new workspace (TASK-1500).
// The detection helpers in internal/cli/detect.go +
// workspace_context_detect.go stay — they're still used by the
// web-side workspace-context save path on workspace creation.

// --- install ---

func workspacesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			workspaces, err := client.ListWorkspaces()
			if err != nil {
				return err
			}

			current, _ := cli.DetectWorkspace(workspaceFlag)

			// JSON output: machine-readable shape consumed by the MCP
			// server's structured-error side channel (TASK-973's
			// classifyExecError populates available_workspaces from
			// this output). Each entry includes `default: true` for the
			// CWD-linked workspace so agents can prefer it without a
			// separate lookup.
			if formatFlag == "json" {
				type entry struct {
					Slug      string `json:"slug"`
					Name      string `json:"name"`
					UpdatedAt string `json:"updated_at,omitempty"`
					Default   bool   `json:"default,omitempty"`
				}
				out := make([]entry, 0, len(workspaces))
				for _, ws := range workspaces {
					out = append(out, entry{
						Slug:      ws.Slug,
						Name:      ws.Name,
						UpdatedAt: ws.UpdatedAt.Format(time.RFC3339),
						Default:   ws.Slug == current,
					})
				}
				return cli.PrintJSON(out)
			}

			if len(workspaces) == 0 {
				fmt.Println("No workspaces. Run 'pad workspace init' to create one.")
				return nil
			}
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
			client, cfg := getClient()
			ws, err := client.GetWorkspace(args[0])
			if err != nil {
				return fmt.Errorf("workspace %q not found", args[0])
			}

			cwd, _ := os.Getwd()
			// Reuse cfg from getClient() so the .pad.toml URL
			// override that just routed the API call also drives
			// the URL we write into the new .pad.toml.
			if err := cli.WriteWorkspaceLink(cwd, ws.Slug, padTomlURLFor(cfg)); err != nil {
				return err
			}
			fmt.Printf("Switched to workspace %q\n", ws.Name)
			return nil
		},
	}
}

// --- completion ---

func exportCmd() *cobra.Command {
	var outputFile string
	var jsonOnly bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export workspace as a self-contained tar.gz bundle",
		Long: `Export the current workspace (collections, items, comments, versions,
and attachments) to a portable tar.gz bundle:

  pad-export.json              — workspace metadata + items + collections + ...
  attachments/manifest.json    — uuid → {filename, mime, size, content_hash}
  attachments/<uuid>.<ext>     — original attachment blobs

Pass --json to emit the legacy items-only JSON file (no attachments).
Both formats can be re-imported via 'pad workspace import'.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			bundle := !jsonOnly

			path := "/workspaces/" + ws + "/export"
			defaultExt := ".json"
			if bundle {
				path += "?format=tar"
				defaultExt = ".tar.gz"
			}

			if outputFile == "" && bundle && term.IsTerminal(int(os.Stdout.Fd())) {
				// Refuse to dump binary tar.gz to a TTY — would render
				// as garbage and likely terminate the user's session
				// when control codes get interpreted.
				return fmt.Errorf("refusing to write binary tar.gz to a terminal; pass -o <file> or pipe to a file")
			}

			// Stream rather than buffer. Workspaces with multi-GB of
			// attachments would otherwise pin the entire bundle in
			// memory before the first byte hits disk — defeats the
			// server-side streaming design and risks OOM (Codex
			// review feedback on PR #305).
			var dest io.Writer = os.Stdout
			var f *os.File
			if outputFile != "" {
				if filepath.Ext(outputFile) == "" {
					outputFile += defaultExt
				}
				var err error
				f, err = os.Create(outputFile)
				if err != nil {
					return fmt.Errorf("create file: %w", err)
				}
				defer f.Close()
				dest = f
			}

			n, resp, err := client.RawStream(path, dest)
			if err != nil {
				if outputFile != "" {
					_ = os.Remove(outputFile) // don't leave a partial file behind
				}
				return fmt.Errorf("export: %w", err)
			}

			// Bundle responses carry an HTTP trailer that signals
			// whether the server-side stream completed cleanly. A
			// missing or non-"ok" value means the bundle is corrupt
			// (an attachment blob got truncated mid-stream, etc.) —
			// delete the partial file and surface failure rather
			// than printing success. The legacy JSON path doesn't
			// set the trailer; we only check it for --bundle.
			if bundle && resp != nil {
				status := resp.Trailer.Get("X-Bundle-Status")
				if status != "ok" {
					if outputFile != "" {
						_ = os.Remove(outputFile)
					}
					return fmt.Errorf("export bundle is incomplete (server X-Bundle-Status=%q); check server logs for stream errors", status)
				}
			}

			if f != nil {
				if err := f.Close(); err != nil {
					return fmt.Errorf("close file: %w", err)
				}
				fmt.Printf("Exported workspace %q to %s (%s)\n", ws, outputFile, humanBytes(n))
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&outputFile, "output", "o", "", "output file path (default: stdout)")
	cmd.Flags().BoolVar(&jsonOnly, "json", false, "emit legacy items-only JSON (no attachments)")
	return cmd
}

// --- import ---

func importCmd() *cobra.Command {
	var nameFlag string
	cmd := &cobra.Command{
		Use:   "import <file>",
		Short: "Import workspace from JSON export or tar.gz bundle",
		Long: `Import a workspace from a previously exported file. Creates a new
workspace with regenerated IDs.

Accepts both formats produced by 'pad workspace export':
  - .json (legacy, items only)
  - .tar.gz (new bundle, includes attachment blobs)

Format is detected by file extension. Override workspace name with --name.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			filePath := args[0]

			path := "/workspaces/import"
			if nameFlag != "" {
				path += "?name=" + nameFlag
			}

			// Detect bundle by extension. .tar.gz / .tgz route through
			// the gzip-stream import path; everything else goes the
			// legacy JSON route. We don't sniff magic bytes — extension
			// is the explicit signal the user gave us.
			contentType := "application/json"
			low := strings.ToLower(filePath)
			if strings.HasSuffix(low, ".tar.gz") || strings.HasSuffix(low, ".tgz") {
				contentType = "application/gzip"
			}

			// Stream the file rather than reading it all in. A multi-
			// GiB bundle would otherwise pin that much memory client-
			// side before the first byte hits the wire — defeats the
			// server's streaming import (Codex P2 on PR #306 round 1).
			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("open file: %w", err)
			}
			defer f.Close()

			var ws models.Workspace
			if err := client.PostStreamWithContentType(path, f, contentType, &ws); err != nil {
				return fmt.Errorf("import: %w", err)
			}

			fmt.Printf("Imported workspace %q (slug: %s)\n", ws.Name, ws.Slug)
			fmt.Printf("  Collections: imported\n")
			fmt.Printf("  Items, comments, links, versions: imported\n")
			if contentType == "application/gzip" {
				fmt.Printf("  Attachments: rehydrated from bundle\n")
			}
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

// --- watch ---

func auditLogCmd() *cobra.Command {
	var days int
	var actor string
	var action string
	var limit int

	cmd := &cobra.Command{
		Use:   "audit-log",
		Short: "View the compliance audit log (admin-only)",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()

			params := models.AuditLogParams{
				Days:   days,
				Actor:  actor,
				Action: action,
				Limit:  limit,
			}

			activities, err := client.GetAuditLog(params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(activities)
			}

			if len(activities) == 0 {
				fmt.Println("No audit log entries found.")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TIME\tACTION\tACTOR\tIP\tDETAILS")
			for _, a := range activities {
				ts := a.CreatedAt.Format("2006-01-02 15:04")
				actorName := a.ActorName
				if actorName == "" {
					actorName = a.UserID
				}
				ip := a.IPAddress
				if ip == "" {
					ip = "-"
				}
				detail := a.Metadata
				if detail == "" {
					detail = "-"
				}
				// Truncate long metadata
				if len(detail) > 60 {
					detail = detail[:57] + "..."
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ts, a.Action, actorName, ip, detail)
			}
			w.Flush()
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 30, "number of days to look back")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor (user ID)")
	cmd.Flags().StringVar(&action, "action", "", "filter by action type")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of entries")

	return cmd
}
