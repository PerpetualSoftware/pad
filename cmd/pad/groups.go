package main

import "github.com/spf13/cobra"

func authCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Configure authentication and account access",
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
	}
	cmd.AddCommand(
		initCmd(),
		linkCmd(),
		switchCmd(),
		workspacesCmd(),
		workspaceContextCmd(),
		onboardCmd(),
		membersCmd(),
		inviteCmd(),
		joinCmd(),
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
	}
	cmd.AddCommand(
		statusCmd(),
		nextCmd(),
		readyCmd(),
		staleCmd(),
		standupCmd(),
		changelogCmd(),
		watchCmd(),
		reconcileCmd(),
	)
	return cmd
}

func itemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "item",
		Short: "Create, update, relate, and discuss Pad items",
	}
	cmd.AddCommand(
		createCmd(),
		listCmd(),
		showCmd(),
		updateCmd(),
		deleteCmd(),
		moveCmd(),
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
	)
	return cmd
}

func collectionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "collection",
		Short: "List and create collections",
	}
	cmd.AddCommand(
		collectionsCmd(),
		collectionsCreateCmd(),
	)
	return cmd
}

func libraryGroupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "library",
		Short: "Browse and activate pre-built conventions and playbooks",
	}
	cmd.AddCommand(
		libraryCmd(),
		libraryActivateCmd(),
	)
	return cmd
}

func agentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Install and manage Pad agent skills",
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
