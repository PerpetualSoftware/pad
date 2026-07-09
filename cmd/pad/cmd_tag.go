package main

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"
)

func tagCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tag",
		Short: "Inspect tags used across the workspace",
		Long:  "Tags are free-form labels on items. They span collections, so a single tag groups items of any type (an Idea and a Bug can share one tag).",
		RunE:  unknownSubcommandRun,
	}
	cmd.AddCommand(tagListCmd())
	return cmd
}

func tagListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List distinct tags in the workspace with item counts",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			tags, err := client.ListTags(ws)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(tags)
			}
			if len(tags) == 0 {
				fmt.Println("No tags yet. Add tags to an item to start grouping across collections.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "TAG\tITEMS")
			for _, t := range tags {
				fmt.Fprintf(w, "%s\t%d\n", t.Tag, t.Count)
			}
			w.Flush()
			return nil
		},
	}
}
