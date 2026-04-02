package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/models"
)

func workspaceContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context",
		Short: "Show or update machine-readable workspace context",
		Long: `Show or update the structured workspace context stored in the current workspace.

Examples:
  pad workspace context --format json
  pad workspace context
  pad workspace context set --file workspace-context.json
  cat workspace-context.json | pad workspace context set --stdin`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			workspace, err := client.GetWorkspace(ws)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				if workspace.Context == nil {
					return cli.PrintJSON(map[string]any{})
				}
				return cli.PrintJSON(workspace.Context)
			}

			printWorkspaceContext(workspace)
			return nil
		},
	}

	cmd.AddCommand(
		workspaceContextSetCmd(),
	)

	return cmd
}

func workspaceContextSetCmd() *cobra.Command {
	var (
		filePath string
		useStdin bool
	)

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Replace the structured workspace context from JSON input",
		Long: `Replace the structured workspace context using a JSON object.

The JSON must match the workspace context schema, including optional sections like
repositories, paths, commands, stack, deployment, and assumptions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			context, err := readWorkspaceContextInput(filePath, useStdin)
			if err != nil {
				return err
			}

			client, _ := getClient()
			ws := getWorkspace()
			updated, err := client.UpdateWorkspace(ws, models.WorkspaceUpdate{
				Context: context,
			})
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				if updated.Context == nil {
					return cli.PrintJSON(map[string]any{})
				}
				return cli.PrintJSON(updated.Context)
			}

			fmt.Printf("Updated workspace context for %s (%s)\n", updated.Name, updated.Slug)
			printWorkspaceContext(updated)
			return nil
		},
	}

	cmd.Flags().StringVar(&filePath, "file", "", "path to a JSON file containing workspace context")
	cmd.Flags().BoolVar(&useStdin, "stdin", false, "read workspace context JSON from stdin")
	return cmd
}

func readWorkspaceContextInput(filePath string, useStdin bool) (*models.WorkspaceContext, error) {
	if filePath == "" && !useStdin {
		return nil, fmt.Errorf("provide --file or --stdin")
	}
	if filePath != "" && useStdin {
		return nil, fmt.Errorf("use either --file or --stdin, not both")
	}

	var data []byte
	var err error
	if filePath != "" {
		data, err = os.ReadFile(filePath)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", filePath, err)
		}
	} else {
		data, err = io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
	}

	var context models.WorkspaceContext
	if err := json.Unmarshal(data, &context); err != nil {
		return nil, fmt.Errorf("parse workspace context JSON: %w", err)
	}
	return &context, nil
}

func printWorkspaceContext(workspace *models.Workspace) {
	if workspace == nil {
		return
	}

	fmt.Printf("Workspace context for %s (%s)\n", workspace.Name, workspace.Slug)
	if workspace.Context == nil {
		fmt.Println("  No structured context configured.")
		return
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

	if len(workspace.Context.Repositories) > 0 {
		fmt.Fprintln(tw, "Repositories")
		for _, repo := range workspace.Context.Repositories {
			label := strings.TrimSpace(strings.Join([]string{repo.Role, repo.Name}, " "))
			fmt.Fprintf(tw, "  %s\tpath=%s\trepo=%s\n", strings.TrimSpace(label), repo.Path, repo.Repo)
		}
	}

	if paths := workspace.Context.Paths; paths != nil {
		printContextSection(tw, "Paths", [][2]string{
			{"root", paths.Root},
			{"docs_repo", paths.DocsRepo},
			{"web", paths.Web},
			{"server", paths.Server},
			{"skills", paths.Skills},
			{"config", paths.Config},
			{"install_root", paths.InstallRoot},
		})
	}

	if commands := workspace.Context.Commands; commands != nil {
		printContextSection(tw, "Commands", [][2]string{
			{"setup", commands.Setup},
			{"build", commands.Build},
			{"test", commands.Test},
			{"lint", commands.Lint},
			{"format", commands.Format},
			{"dev", commands.Dev},
			{"start", commands.Start},
			{"web", commands.Web},
		})
	}

	if stack := workspace.Context.Stack; stack != nil {
		printContextSection(tw, "Stack", [][2]string{
			{"languages", strings.Join(stack.Languages, ", ")},
			{"frameworks", strings.Join(stack.Frameworks, ", ")},
			{"package_managers", strings.Join(stack.PackageManagers, ", ")},
		})
	}

	if deployment := workspace.Context.Deployment; deployment != nil {
		printContextSection(tw, "Deployment", [][2]string{
			{"mode", deployment.Mode},
			{"base_url", deployment.BaseURL},
			{"host", deployment.Host},
		})
	}

	if len(workspace.Context.Assumptions) > 0 {
		fmt.Fprintln(tw, "Assumptions")
		for _, assumption := range workspace.Context.Assumptions {
			if strings.TrimSpace(assumption) == "" {
				continue
			}
			fmt.Fprintf(tw, "  -\t%s\n", assumption)
		}
	}

	_ = tw.Flush()
}

func printContextSection(tw *tabwriter.Writer, section string, fields [][2]string) {
	var printed bool
	for _, field := range fields {
		if strings.TrimSpace(field[1]) == "" {
			continue
		}
		if !printed {
			fmt.Fprintln(tw, section)
			printed = true
		}
		fmt.Fprintf(tw, "  %s\t%s\n", field[0], field[1])
	}
}
