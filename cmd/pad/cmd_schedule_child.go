package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

// renderMilestoneSummary writes the milestone's "Children:" line, as
// `pad remind show` prints it, and reports whether there was one to write.
func renderMilestoneSummary(w io.Writer, m models.CommitmentMilestone) bool {
	if m.Total == 0 && m.Result == nil {
		return false
	}
	result := "none"
	if m.Result != nil {
		result = *m.Result
	}
	if result == "unknown" {
		result += " (includes items you cannot see)"
	}
	fmt.Fprintf(w, "Children:\t%d of %d counted, %d waivers, result %s\n", m.Counted, m.Total, m.Waivers, result)
	return true
}

// renderMilestoneChildren prints the summary line and one row per selected
// child the caller can see.
func renderMilestoneChildren(out io.Writer, m models.CommitmentMilestone) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if !renderMilestoneSummary(w, m) {
		fmt.Fprintln(w, "Children:\tnone selected")
	}
	for _, c := range m.Children {
		state := c.CloseState
		if c.DoneValue != "" {
			state += " (" + c.DoneValue + ")"
		}
		detail := "not counted"
		if c.Counts {
			detail = "counted"
		}
		if c.Waived && c.Reason != "" {
			detail += "; reason: " + c.Reason
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", c.ItemRef, c.Kind, state, detail)
	}
	return w.Flush()
}

const childHelp = `Selecting or waiving a child needs edit permission on the milestone and read
permission on the child. Any item of the same workspace can be a child; no
link is needed. A waiver counts the child as delivered and needs a reason.
Removing keeps the selection as history; removing a child that is not
selected succeeds.`

func remindChildCmd() *cobra.Command {
	c := &cobra.Command{Use: "child", RunE: unknownSubcommandRun, Short: "Select, waive or remove a milestone's children", Long: childHelp}
	render := func(cmd *cobra.Command, raw json.RawMessage) error {
		if formatFlag == "json" {
			return cli.PrintJSON(raw)
		}
		return renderMilestoneChildren(cmd.OutOrStdout(), decodeScheduleView(raw).Milestone)
	}
	selectCmd := &cobra.Command{Use: "select <ref> <child-ref>", Short: "Select a child that the milestone's result counts", Long: childHelp, Example: "  pad remind child select TASK-5 TASK-12", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, _ := getClient()
		raw, err := client.SetMilestoneChild(getWorkspace(), args[0], args[1], "child", "")
		if err != nil {
			return err
		}
		return render(cmd, raw)
	}}
	var reason string
	waive := &cobra.Command{Use: "waive <ref> <child-ref>", Short: "Waive a child: it counts as delivered", Long: childHelp, Example: `  pad remind child waive TASK-5 TASK-12 --reason "descoped"`, Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required: say why the child is waived")
		}
		client, _ := getClient()
		raw, err := client.SetMilestoneChild(getWorkspace(), args[0], args[1], "waiver", reason)
		if err != nil {
			return err
		}
		return render(cmd, raw)
	}}
	waive.Flags().StringVar(&reason, "reason", "", "why the child is waived (required)")
	remove := &cobra.Command{Use: "remove <ref> <child-ref>", Short: "Remove a child or its waiver from the milestone", Long: childHelp, Example: "  pad remind child remove TASK-5 TASK-12", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, _ := getClient()
		raw, err := client.RemoveMilestoneChild(getWorkspace(), args[0], args[1])
		if err != nil {
			return err
		}
		return render(cmd, raw)
	}}
	c.AddCommand(selectCmd, waive, remove)
	return c
}
