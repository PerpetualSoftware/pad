package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func roleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role",
		Short: "Manage agent roles",
		Long:  "Agent roles define capability specializations (e.g. Planner, Implementer, Reviewer) for human-agent work assignment.",
	}
	cmd.AddCommand(roleListCmd(), roleCreateCmd(), roleUpdateCmd(), roleDeleteCmd())
	return cmd
}

func roleListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List agent roles in the workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			roles, err := client.ListAgentRoles(ws)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(roles)
			}
			if len(roles) == 0 {
				fmt.Println("No roles defined yet.")
				fmt.Println("Create one with: pad role create 'Implementer' --description 'Writes code, builds features'")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "SLUG\tNAME\tDESCRIPTION\tTOOLS\tITEMS")
			for _, r := range roles {
				icon := r.Icon
				if icon != "" {
					icon += " "
				}
				fmt.Fprintf(w, "%s\t%s%s\t%s\t%s\t%d\n", r.Slug, icon, r.Name, r.Description, r.Tools, r.ItemCount)
			}
			w.Flush()
			return nil
		},
	}
}

func roleCreateCmd() *cobra.Command {
	var description, icon, tools string

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new agent role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			input := models.AgentRoleCreate{
				Name:        args[0],
				Description: description,
				Icon:        icon,
				Tools:       tools,
			}
			role, err := client.CreateAgentRole(ws, input)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(role)
			}
			iconStr := ""
			if role.Icon != "" {
				iconStr = role.Icon + " "
			}
			fmt.Printf("Created role %s%s (%s)\n", iconStr, role.Name, role.Slug)
			return nil
		},
	}
	cmd.Flags().StringVar(&description, "description", "", "role description")
	cmd.Flags().StringVar(&icon, "icon", "", "role icon (emoji)")
	cmd.Flags().StringVar(&tools, "tools", "", "preferred tools/models (e.g. 'Claude Code + Sonnet 4.6')")
	return cmd
}

// roleUpdateCmd updates an existing agent role's name, description,
// icon, tools, slug, or sort_order. Only flags explicitly set are
// sent — every AgentRoleUpdate field is a pointer server-side
// (models/agent_role.go::AgentRoleUpdate) so omitted values are
// preserved.
//
// Built for the PLAN-1496 / onboard playbook (TASK-1499): the agent
// needs to rewrite role descriptions and icons during onboarding to
// match how the user's team actually divides work. Without this
// command, the agent could only create or delete roles — adapting
// the existing ones required deleting and re-creating, which loses
// the agent_role_id references on assigned items.
//
// The slug arg can be a role slug ("planner") or its UUID — the
// server-side store resolves both via ResolveAgentRoleID.
func roleUpdateCmd() *cobra.Command {
	var (
		name        string
		newSlug     string
		description string
		icon        string
		tools       string
		sortOrder   int
	)

	cmd := &cobra.Command{
		Use:   "update <slug>",
		Short: "Update an existing agent role",
		Long: `Update an existing agent role's name, slug, description,
icon, tools, or sort order.

Only flags you explicitly set are sent — every other property is left
untouched. The <slug> positional accepts either the role's current
slug ("planner") or its UUID.

NOTE: the rename flag is --new-slug, not --slug. This is deliberate:
the MCP catalog passes the positional via MCP property "slug" and
the rename target via "new_slug"; calling the CLI flag --slug too
would collide on the local stdio MCP path (Codex review on PR #574).

Examples:
  pad role update planner --description "Decomposes plans into tasks"
  pad role update reviewer --icon 👁️
  pad role update implementer --name "Engineer" --new-slug engineer`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			ref := args[0]

			input := models.AgentRoleUpdate{}

			if cmd.Flags().Changed("name") {
				input.Name = &name
			}
			if cmd.Flags().Changed("new-slug") {
				input.Slug = &newSlug
			}
			if cmd.Flags().Changed("description") {
				input.Description = &description
			}
			if cmd.Flags().Changed("icon") {
				input.Icon = &icon
			}
			if cmd.Flags().Changed("tools") {
				input.Tools = &tools
			}
			if cmd.Flags().Changed("sort-order") {
				input.SortOrder = &sortOrder
			}

			role, err := client.UpdateAgentRole(ws, ref, input)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(role)
			}
			iconStr := ""
			if role.Icon != "" {
				iconStr = role.Icon + " "
			}
			fmt.Printf("Updated role %s%s (%s)\n", iconStr, role.Name, role.Slug)
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "new display name")
	cmd.Flags().StringVar(&newSlug, "new-slug", "", "rename target — new slug (kebab-case identifier)")
	cmd.Flags().StringVar(&description, "description", "", "new description (empty string clears)")
	cmd.Flags().StringVar(&icon, "icon", "", "new emoji icon (empty string clears)")
	cmd.Flags().StringVar(&tools, "tools", "", "preferred tools/models string")
	cmd.Flags().IntVar(&sortOrder, "sort-order", 0, "new sort order (lower = appears first)")

	return cmd
}

func roleDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <slug>",
		Short: "Delete an agent role",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			if err := client.DeleteAgentRole(ws, args[0]); err != nil {
				return err
			}
			fmt.Printf("Deleted role %s\n", args[0])
			return nil
		},
	}
}

// --- webhooks ---
