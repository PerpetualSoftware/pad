package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// unknownSubcommandRun returns an error when the user passes an
// unrecognized subcommand (e.g. a typo like `pad item
// nonexistentsubcommand`). When called with no args the parent's help is
// shown and nil is returned.
func unknownSubcommandRun(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath())
	}
	return cmd.Help()
}

func authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Configure authentication and account access",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		configureCmd(),
		setupCmd(),
		loginCmd(),
		logoutCmd(),
		whoamiCmd(),
		resetPasswordCmd(),
	)
	return cmd
}

func serverCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "Manage the Pad server process and web UI",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		serveCmd(),
		stopCmd(),
		infoCmd(),
		openCmd(),
	)
	return cmd
}

func workspaceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workspace",
		Short: "Manage workspaces and workspace membership",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		initCmd(),
		workspaceCreateCmd(),
		workspaceClaimCmd(),
		linkCmd(),
		switchCmd(),
		workspacesCmd(),
		workspaceContextCmd(),
		// onboardCmd() retired in PLAN-1496 / TASK-1502 — replaced by
		// the /pad onboard library playbook (TASK-1499), which works
		// for MCP-only agents too.
		membersCmd(),
		inviteCmd(),
		joinCmd(),
		workspaceDeletedCmd(),
		workspaceRestoreCmd(),
		storageCmd(),
		exportCmd(),
		importCmd(),
		auditLogCmd(),
	)
	return cmd
}

func projectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Inspect project state, reports, and activity",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		statusCmd(),
		nextCmd(),
		readyCmd(),
		staleCmd(),
		standupCmd(),
		changelogCmd(),
		reportCmd(),
		activityCmd(),
		watchCmd(),
		reconcileCmd(),
	)
	return cmd
}

func itemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "item",
		Short: "Create, update, relate, and discuss Pad items",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		createCmd(),
		listCmd(),
		showCmd(),
		itemOpenCmd(),
		updateCmd(),
		historyCmd(),
		deleteCmd(),
		restoreCmd(),
		moveCmd(),
		itemExportCmd(),
		itemImportCmd(),
		editCmd(),
		searchCmd(),
		bulkUpdateCmd(),
		commentCmd(),
		commentsCmd(),
		noteCmd(),
		decideCmd(),
		blocksCmd(),
		blockedByCmd(),
		depsCmd(),
		unblockCmd(),
		splitFromCmd(),
		supersedesCmd(),
		implementsCmd(),
		unsplitCmd(),
		unsupersedeCmd(),
		unimplementsCmd(),
		relatedCmd(),
		implementedByCmd(),
		backlinksCmd(),
		starCmd(),
		unstarCmd(),
		starredCmd(),
	)
	return cmd
}

func collectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collection",
		Short: "List, create, update, and delete collections",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		collectionsCmd(),
		collectionsCreateCmd(),
		collectionsUpdateCmd(),
		collectionsDeleteCmd(),
	)
	return cmd
}

func libraryGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "library",
		Short: "Browse and activate pre-built conventions and playbooks",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		libraryCmd(),
		libraryGetCmd(),
		libraryActivateCmd(),
	)
	return cmd
}

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Install and manage Pad agent skills",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		installCmd(),
		agentUpdateCmd(),
		agentStatusCmd(),
	)
	return cmd
}

func dbCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Database backup, restore, and migration tools",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(
		dbBackupCmd(),
		dbRestoreCmd(),
		dbMigrateToPgCmd(),
	)
	return cmd
}

func agentUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update",
		Short: "Update installed Pad skills across all supported tools",
		RunE: func(cmd *cobra.Command, args []string) error {
			return installUpdate()
		},
	}
}

func agentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show installed Pad skill status across supported tools",
		RunE: func(cmd *cobra.Command, args []string) error {
			return installList()
		},
	}
}
