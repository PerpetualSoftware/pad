package main

import (
	"fmt"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

func forecastSetterCLI(f *models.CommitmentForecast) string {
	if f.SetByName == "" {
		if f.SetByKind == "agent" {
			return "an agent"
		}
		return "a member"
	}
	name := f.SetByName
	if f.SetByKind == "agent" {
		name += ", agent"
	}
	return name
}

func remindForecastCmd() *cobra.Command {
	c := &cobra.Command{Use: "forecast", RunE: unknownSubcommandRun, Short: "Set or clear a hand-set milestone estimate", Long: "A forecast arms nothing and is never a missed deadline."}
	var source string
	set := &cobra.Command{Use: "set <ref> <value>", Short: "Set a milestone forecast date or instant", Example: "  pad remind forecast set TASK-5 2031-03-12 --source estimate", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		client, _ := getClient()
		raw, err := client.SetCommitmentForecast(getWorkspace(), args[0], args[1], source)
		if err != nil {
			return err
		}
		if formatFlag == "json" {
			return cli.PrintJSON(raw)
		}
		f := decodeScheduleView(raw).Forecast
		if f == nil {
			return fmt.Errorf("the commitment response has no forecast")
		}
		detail := "set by " + forecastSetterCLI(f)
		if f.Source != "" {
			detail += "; source: " + f.Source
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Forecast for %s: %s (%s)\n", args[0], f.Value, detail)
		return nil
	}}
	set.Flags().StringVar(&source, "source", "", "forecast source (up to 280 characters)")
	clear := &cobra.Command{Use: "clear <ref>", Short: "Clear a milestone forecast", Example: "  pad remind forecast clear TASK-5", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, _ := getClient()
		raw, err := client.ClearCommitmentForecast(getWorkspace(), args[0])
		if err != nil {
			return err
		}
		if formatFlag == "json" {
			return cli.PrintJSON(raw)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Forecast cleared for %s\n", args[0])
		return nil
	}}
	c.AddCommand(set, clear)
	return c
}
