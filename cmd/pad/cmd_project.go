package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/PerpetualSoftware/pad/internal/cli"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "dashboard",
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
				ActiveItems []struct {
					Slug           string `json:"slug"`
					Title          string `json:"title"`
					CollectionSlug string `json:"collection_slug"`
					CollectionIcon string `json:"collection_icon"`
					Priority       string `json:"priority"`
					Status         string `json:"status"`
					ItemRef        string `json:"item_ref"`
				} `json:"active_items"`
				ActivePlans []struct {
					Slug      string `json:"slug"`
					Title     string `json:"title"`
					Progress  int    `json:"progress"`
					TaskCount int    `json:"task_count"`
					DoneCount int    `json:"done_count"`
				} `json:"active_plans"`
				ByRole []struct {
					RoleName  string   `json:"role_name"`
					RoleSlug  string   `json:"role_slug"`
					RoleIcon  string   `json:"role_icon"`
					Tools     string   `json:"tools"`
					ItemCount int      `json:"item_count"`
					Users     []string `json:"assigned_users"`
				} `json:"by_role"`
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

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			headerColor := color.New(color.Bold, color.FgCyan)
			yellow := color.New(color.FgYellow)
			blue := color.New(color.FgBlue)
			green := color.New(color.FgGreen)

			headerColor.Printf("📊 Project Status (%d items)\n", dash.Summary.TotalItems)
			fmt.Println(dim.Sprint(strings.Repeat("═", 50)))

			// Collection summary
			if len(dash.Summary.ByCollection) > 0 {
				fmt.Println()
				for collSlug, statuses := range dash.Summary.ByCollection {
					parts := []string{}
					for status, count := range statuses {
						sc := cli.StatusColor(status)
						parts = append(parts, sc.Sprintf("%s: %d", status, count))
					}
					fmt.Printf("  %s  %s\n", bold.Sprintf("%-10s", collSlug), strings.Join(parts, ", "))
				}
			}

			// Active work
			if len(dash.ActiveItems) > 0 {
				fmt.Println()
				bold.Printf("🔨 Active Work (%d)\n", len(dash.ActiveItems))
				for _, ai := range dash.ActiveItems {
					ref := ""
					if ai.ItemRef != "" {
						ref = dim.Sprintf("%-10s ", ai.ItemRef)
					}
					status := cli.ColorizedStatus(ai.Status)
					priority := ""
					if ai.Priority != "" {
						priority = " " + cli.PriorityColor(ai.Priority).Sprint(ai.Priority)
					}
					fmt.Printf("  %s%s  %s%s\n", ref, bold.Sprint(ai.Title), status, priority)
				}
			}

			// Active plans
			if len(dash.ActivePlans) > 0 {
				fmt.Println()
				bold.Println("🏗️  Active Plans")
				for _, plan := range dash.ActivePlans {
					bar := colorProgressBar(plan.Progress, 20, green)
					fmt.Printf("  %s %s %s %s\n",
						bold.Sprint(plan.Title),
						bar,
						color.New(color.FgGreen).Sprintf("%d%%", plan.Progress),
						dim.Sprintf("(%d/%d tasks)", plan.DoneCount, plan.TaskCount),
					)
				}
			}

			// Role breakdown
			if len(dash.ByRole) > 0 {
				fmt.Println()
				bold.Println("🎭 Roles")
				for _, r := range dash.ByRole {
					icon := r.RoleIcon
					if icon != "" {
						icon += " "
					}
					name := r.RoleName
					if name == "" {
						name = "Unassigned"
					}
					users := ""
					if len(r.Users) > 0 {
						users = "  (" + strings.Join(r.Users, ", ") + ")"
					}
					tools := ""
					if r.Tools != "" {
						tools = "  [" + r.Tools + "]"
					}
					fmt.Printf("  %s%-14s %d items%s%s\n", icon, name, r.ItemCount, users, tools)
				}
			}

			// Attention items
			if len(dash.Attention) > 0 {
				fmt.Println()
				bold.Println("⚠️  Needs Attention")
				for _, a := range dash.Attention {
					fmt.Printf("  %s — %s\n", yellow.Sprint(a.ItemTitle), dim.Sprint(a.Reason))
				}
			}

			// Suggested next
			if len(dash.SuggestedNext) > 0 {
				fmt.Println()
				bold.Println("💡 Suggested Next")
				for _, s := range dash.SuggestedNext {
					fmt.Printf("  %s — %s\n", blue.Sprint(s.ItemTitle), dim.Sprint(s.Reason))
				}
			}

			fmt.Println()
			return nil
		},
	}
}

func colorProgressBar(pct, width int, filledColor *color.Color) string {
	filled := (pct * width) / 100
	if filled > width {
		filled = width
	}
	dim := color.New(color.Faint)
	return "[" + filledColor.Sprint(strings.Repeat("█", filled)) + dim.Sprint(strings.Repeat("░", width-filled)) + "]"
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

			// Decode once; both the JSON branch and the human-readable
			// branch use the same suggested_next slice.
			//
			// BUG-987 bug 6: previously the JSON branch dumped the
			// entire dashboard, making `project next --format json`
			// indistinguishable from `project dashboard --format json`.
			// Now it emits ONLY the recommended-next array (with the
			// item-ref + reason fields agents need), matching the human
			// branch's framing.
			var dash struct {
				SuggestedNext []struct {
					ItemSlug   string `json:"item_slug"`
					ItemRef    string `json:"item_ref,omitempty"`
					ItemTitle  string `json:"item_title"`
					Collection string `json:"collection"`
					Reason     string `json:"reason"`
				} `json:"suggested_next"`
			}

			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				return err
			}

			if formatFlag == "json" {
				return cli.PrintJSON(dash.SuggestedNext)
			}

			if len(dash.SuggestedNext) == 0 {
				fmt.Println("No suggestions — all tasks may be complete or no active plans found.")
				return nil
			}

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)

			bold.Println("💡 Recommended next:")
			for i, s := range dash.SuggestedNext {
				fmt.Printf("  %s %s\n     %s\n",
					dim.Sprintf("%d.", i+1),
					bold.Sprint(s.ItemTitle),
					dim.Sprint(s.Reason),
				)
			}
			return nil
		},
	}
}

// --- standup ---

func standupCmd() *cobra.Command {
	var days int

	cmd := &cobra.Command{
		Use:   "standup",
		Short: "Auto-generate a daily standup report from recent activity",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Fetch dashboard data
			dashJSON, err := client.GetDashboard(ws)
			if err != nil {
				return err
			}

			// Parse dashboard
			var dash struct {
				ActiveItems []struct {
					Slug     string `json:"slug"`
					Title    string `json:"title"`
					Priority string `json:"priority"`
					Status   string `json:"status"`
					ItemRef  string `json:"item_ref"`
				} `json:"active_items"`
				// BUG-987 bug 8: previously omitted ItemRef from
				// Attention + SuggestedNext, leaving the JSON output's
				// blockers / suggested_next entries with empty refs.
				// The dashboard handler populates item_ref on both
				// arrays already; we just weren't reading the field.
				Attention []struct {
					Type      string `json:"type"`
					ItemSlug  string `json:"item_slug"`
					ItemRef   string `json:"item_ref"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"attention"`
				SuggestedNext []struct {
					ItemSlug  string `json:"item_slug"`
					ItemRef   string `json:"item_ref"`
					ItemTitle string `json:"item_title"`
					Reason    string `json:"reason"`
				} `json:"suggested_next"`
			}
			if err := json.Unmarshal(dashJSON, &dash); err != nil {
				return fmt.Errorf("parsing dashboard: %w", err)
			}

			// Fetch recently completed items (terminal statuses)
			doneStatuses := models.DefaultTerminalStatuses
			var completedItems []models.Item
			cutoff := time.Now().AddDate(0, 0, -days)

			for _, status := range doneStatuses {
				items, err := client.ListItems(ws, url.Values{
					"status": {status},
					"sort":   {"updated_at:desc"},
					"limit":  {"20"},
				})
				if err != nil {
					continue
				}
				for _, item := range items {
					if item.UpdatedAt.After(cutoff) {
						completedItems = append(completedItems, item)
					}
				}
			}

			// Fetch in-progress items
			inProgressItems, err := client.ListItems(ws, url.Values{
				"status": {"in-progress"},
				"sort":   {"updated_at:desc"},
			})
			if err != nil {
				inProgressItems = nil
			}

			// Build JSON output if requested
			if formatFlag == "json" {
				type standupItem struct {
					Ref      string `json:"ref"`
					Title    string `json:"title"`
					Status   string `json:"status,omitempty"`
					Priority string `json:"priority,omitempty"`
					Reason   string `json:"reason,omitempty"`
				}

				type standupJSON struct {
					Date          string        `json:"date"`
					Days          int           `json:"days"`
					Completed     []standupItem `json:"completed"`
					InProgress    []standupItem `json:"in_progress"`
					Blockers      []standupItem `json:"blockers"`
					SuggestedNext []standupItem `json:"suggested_next"`
				}

				output := standupJSON{
					Date:          time.Now().Format("2006-01-02"),
					Days:          days,
					Completed:     []standupItem{},
					InProgress:    []standupItem{},
					Blockers:      []standupItem{},
					SuggestedNext: []standupItem{},
				}

				for _, item := range completedItems {
					output.Completed = append(output.Completed, standupItem{
						Ref:    cli.ItemRef(item),
						Title:  item.Title,
						Status: extractFieldFromJSON(item.Fields, "status"),
					})
				}
				for _, item := range inProgressItems {
					output.InProgress = append(output.InProgress, standupItem{
						Ref:      cli.ItemRef(item),
						Title:    item.Title,
						Priority: extractFieldFromJSON(item.Fields, "priority"),
					})
				}
				for _, a := range dash.Attention {
					output.Blockers = append(output.Blockers, standupItem{
						Ref:    a.ItemRef,
						Title:  a.ItemTitle,
						Reason: a.Reason,
					})
				}
				for _, s := range dash.SuggestedNext {
					output.SuggestedNext = append(output.SuggestedNext, standupItem{
						Ref:    s.ItemRef,
						Title:  s.ItemTitle,
						Reason: s.Reason,
					})
				}

				return cli.PrintJSON(output)
			}

			// Human-readable output
			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			headerColor := color.New(color.Bold, color.FgCyan)
			yellow := color.New(color.FgYellow)
			blue := color.New(color.FgBlue)
			green := color.New(color.FgGreen)

			dateStr := time.Now().Format("January 2, 2006")
			headerColor.Printf("📋 Standup — %s\n", dateStr)
			fmt.Println(dim.Sprint(strings.Repeat("═", 40)))

			// Completed
			fmt.Println()
			bold.Println("✅ Completed")
			if len(completedItems) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				for _, item := range completedItems {
					ref := cli.ItemRef(item)
					refStr := ""
					if ref != "" {
						refStr = dim.Sprintf("%-10s", ref) + "  "
					}
					fmt.Printf("  %s%s\n", refStr, green.Sprint(item.Title))
				}
			}

			// In Progress
			fmt.Println()
			bold.Println("🔨 In Progress")
			if len(inProgressItems) == 0 && len(dash.ActiveItems) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				// Prefer dashboard active items (they include more metadata)
				if len(dash.ActiveItems) > 0 {
					for _, ai := range dash.ActiveItems {
						ref := ""
						if ai.ItemRef != "" {
							ref = dim.Sprintf("%-10s", ai.ItemRef) + "  "
						}
						priority := ""
						if ai.Priority != "" {
							priority = " (" + cli.PriorityColor(ai.Priority).Sprint(ai.Priority) + ")"
						}
						fmt.Printf("  %s%s%s\n", ref, bold.Sprint(ai.Title), priority)
					}
				} else {
					for _, item := range inProgressItems {
						ref := cli.ItemRef(item)
						refStr := ""
						if ref != "" {
							refStr = dim.Sprintf("%-10s", ref) + "  "
						}
						priorityStr := extractFieldFromJSON(item.Fields, "priority")
						priority := ""
						if priorityStr != "" {
							priority = " (" + cli.PriorityColor(priorityStr).Sprint(priorityStr) + ")"
						}
						fmt.Printf("  %s%s%s\n", refStr, bold.Sprint(item.Title), priority)
					}
				}
			}

			// Blockers
			fmt.Println()
			bold.Println("⚠️  Blockers")
			if len(dash.Attention) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				for _, a := range dash.Attention {
					fmt.Printf("  %s — %s\n", yellow.Sprint(a.ItemTitle), dim.Sprint(a.Reason))
				}
			}

			// Up Next
			fmt.Println()
			bold.Println("💡 Up Next")
			if len(dash.SuggestedNext) == 0 {
				fmt.Println("  " + dim.Sprint("(none)"))
			} else {
				for _, s := range dash.SuggestedNext {
					reason := ""
					if s.Reason != "" {
						reason = " (" + dim.Sprint(s.Reason) + ")"
					}
					fmt.Printf("  %s%s\n", blue.Sprint(s.ItemTitle), reason)
				}
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 1, "number of days to look back for completed items")
	return cmd
}

// --- changelog ---

func reportCmd() *cobra.Command {
	var window string
	var collections string

	cmd := &cobra.Command{
		Use:   "report",
		Short: "Windowed project report — throughput, net flow, completions, status mix",
		Long: `Show a time-windowed project report: items created vs completed per bucket,
net flow, completed-by-collection, and a current status-distribution snapshot.

--window day|week|2wk|month (default week; day buckets hourly, others daily).
--collections restricts to a comma-separated list of collection slugs.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			repJSON, err := client.GetReport(ws, window, collections)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return cli.PrintJSON(repJSON)
			}

			var rep struct {
				Window      string   `json:"window"`
				Granularity string   `json:"granularity"`
				RangeStart  string   `json:"range_start"`
				RangeEnd    string   `json:"range_end"`
				Collections []string `json:"collections"`
				Buckets     []struct {
					Bucket    string `json:"bucket"`
					Created   int    `json:"created"`
					Completed int    `json:"completed"`
				} `json:"buckets"`
				Totals struct {
					Created   int `json:"created"`
					Completed int `json:"completed"`
					NetFlow   int `json:"net_flow"`
				} `json:"totals"`
				CompletedByCollection []struct {
					Collection string `json:"collection"`
					Count      int    `json:"count"`
				} `json:"completed_by_collection"`
				StatusDistribution []struct {
					Collection string `json:"collection"`
					Status     string `json:"status"`
					Count      int    `json:"count"`
				} `json:"status_distribution"`
				CycleTime struct {
					SampleSize  int     `json:"sample_size"`
					MedianHours float64 `json:"median_hours"`
					P90Hours    float64 `json:"p90_hours"`
				} `json:"cycle_time"`
				WIP struct {
					OpenCount      int     `json:"open_count"`
					MedianAgeHours float64 `json:"median_age_hours"`
					AgingBuckets   []struct {
						Label string `json:"label"`
						Count int    `json:"count"`
					} `json:"aging_buckets"`
				} `json:"wip"`
			}
			if err := json.Unmarshal(repJSON, &rep); err != nil {
				fmt.Println(string(repJSON))
				return nil
			}

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			green := color.New(color.FgGreen)
			red := color.New(color.FgRed)

			bold.Printf("📊 Report — %s", rep.Window)
			if len(rep.Collections) > 0 {
				dim.Printf("  (%s)", strings.Join(rep.Collections, ", "))
			}
			fmt.Println()
			dim.Printf("   %s → %s\n\n", rep.RangeStart, rep.RangeEnd)

			net := fmt.Sprintf("%+d", rep.Totals.NetFlow)
			fmt.Printf("   Created: ")
			green.Printf("%d", rep.Totals.Created)
			fmt.Printf("   Completed: ")
			green.Printf("%d", rep.Totals.Completed)
			fmt.Printf("   Net flow: ")
			if rep.Totals.NetFlow >= 0 {
				bold.Print(net)
			} else {
				red.Print(net)
			}
			fmt.Print("\n\n")

			// Per-bucket throughput (skip all-zero buckets to keep it compact).
			any := false
			for _, b := range rep.Buckets {
				if b.Created == 0 && b.Completed == 0 {
					continue
				}
				if !any {
					bold.Println("   Throughput (created / completed)")
					any = true
				}
				fmt.Printf("   %-13s ", b.Bucket)
				green.Printf("+%d", b.Created)
				fmt.Print(" / ")
				red.Printf("-%d", b.Completed)
				fmt.Println()
			}
			if !any {
				dim.Println("   No activity in this window.")
			}

			if len(rep.CompletedByCollection) > 0 {
				fmt.Println()
				bold.Println("   Completed by collection")
				for _, c := range rep.CompletedByCollection {
					fmt.Printf("   %-16s %d\n", c.Collection, c.Count)
				}
			}

			if len(rep.StatusDistribution) > 0 {
				fmt.Println()
				bold.Println("   Status distribution")
				for _, s := range rep.StatusDistribution {
					fmt.Printf("   %-16s %-14s %d\n", s.Collection, s.Status, s.Count)
				}
			}

			fmtHours := func(h float64) string {
				if h >= 48 {
					return fmt.Sprintf("%.1fd", h/24)
				}
				return fmt.Sprintf("%.1fh", h)
			}
			if rep.CycleTime.SampleSize > 0 {
				fmt.Println()
				bold.Println("   Cycle time (created → completed)")
				fmt.Printf("   median %s   p90 %s   (n=%d)\n",
					fmtHours(rep.CycleTime.MedianHours), fmtHours(rep.CycleTime.P90Hours), rep.CycleTime.SampleSize)
			}
			if rep.WIP.OpenCount > 0 {
				fmt.Println()
				bold.Printf("   Work in progress: %d open", rep.WIP.OpenCount)
				dim.Printf("  (median age %s)\n", fmtHours(rep.WIP.MedianAgeHours))
				for _, b := range rep.WIP.AgingBuckets {
					if b.Count > 0 {
						fmt.Printf("   %-7s %d\n", b.Label, b.Count)
					}
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&window, "window", "week", "Report window: day|week|2wk|month")
	cmd.Flags().StringVar(&collections, "collections", "", "Comma-separated collection slugs to include (default: all visible)")
	return cmd
}

func changelogCmd() *cobra.Command {
	var days int
	var since string
	var parentRef string

	cmd := &cobra.Command{
		Use:   "changelog",
		Short: "Generate release notes from completed items",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			// Determine cutoff date
			var cutoff time.Time
			if since != "" {
				parsed, err := time.Parse("2006-01-02", since)
				if err != nil {
					return fmt.Errorf("invalid --since date (expected YYYY-MM-DD): %w", err)
				}
				cutoff = parsed
			} else {
				cutoff = time.Now().AddDate(0, 0, -days)
			}

			// Fetch completed items across all terminal statuses
			doneStatuses := models.DefaultTerminalStatuses
			var allItems []models.Item

			for _, status := range doneStatuses {
				items, err := client.ListItems(ws, url.Values{
					"status": {status},
					"sort":   {"updated_at:desc"},
					"limit":  {"100"},
				})
				if err != nil {
					continue
				}
				for _, item := range items {
					if item.UpdatedAt.After(cutoff) {
						allItems = append(allItems, item)
					}
				}
			}

			// Filter by parent if specified
			filterParent := parentRef
			if filterParent != "" {
				var filtered []models.Item
				for _, item := range allItems {
					// Check parent link (populated by API enrichment)
					if strings.EqualFold(item.ParentLinkID, filterParent) ||
						strings.EqualFold(item.ParentRef, filterParent) ||
						strings.EqualFold(item.ParentTitle, filterParent) {
						filtered = append(filtered, item)
					}
				}
				allItems = filtered
			}

			// Group by collection
			type collectionGroup struct {
				Name  string
				Icon  string
				Items []models.Item
			}
			groupOrder := []string{}
			groups := map[string]*collectionGroup{}

			for _, item := range allItems {
				key := item.CollectionSlug
				if key == "" {
					key = "other"
				}
				if _, exists := groups[key]; !exists {
					name := item.CollectionName
					if name == "" {
						name = key
					}
					groups[key] = &collectionGroup{
						Name: name,
						Icon: item.CollectionIcon,
					}
					groupOrder = append(groupOrder, key)
				}
				groups[key].Items = append(groups[key].Items, item)
			}

			// Determine period label
			periodLabel := fmt.Sprintf("last %d days", days)
			if since != "" {
				periodLabel = "since " + since
			}
			if filterParent != "" {
				periodLabel += " (parent: " + filterParent + ")"
			}

			// JSON output
			if formatFlag == "json" {
				type changelogItem struct {
					Ref    string `json:"ref"`
					Title  string `json:"title"`
					Status string `json:"status"`
				}
				type changelogGroup struct {
					Collection string          `json:"collection"`
					Icon       string          `json:"icon,omitempty"`
					Count      int             `json:"count"`
					Items      []changelogItem `json:"items"`
				}
				type changelogJSON struct {
					Period string           `json:"period"`
					Since  string           `json:"since"`
					Total  int              `json:"total"`
					Groups []changelogGroup `json:"groups"`
				}

				output := changelogJSON{
					Period: periodLabel,
					Since:  cutoff.Format("2006-01-02"),
					Total:  len(allItems),
					Groups: []changelogGroup{},
				}

				for _, key := range groupOrder {
					g := groups[key]
					cg := changelogGroup{
						Collection: g.Name,
						Icon:       g.Icon,
						Count:      len(g.Items),
						Items:      []changelogItem{},
					}
					for _, item := range g.Items {
						cg.Items = append(cg.Items, changelogItem{
							Ref:    cli.ItemRef(item),
							Title:  item.Title,
							Status: extractFieldFromJSON(item.Fields, "status"),
						})
					}
					output.Groups = append(output.Groups, cg)
				}

				return cli.PrintJSON(output)
			}

			// Markdown output
			if formatFlag == "markdown" {
				fmt.Printf("# Changelog — %s\n\n", periodLabel)

				if len(allItems) == 0 {
					fmt.Println("No completed items in this period.")
					return nil
				}

				for _, key := range groupOrder {
					g := groups[key]
					icon := g.Icon
					if icon == "" {
						icon = collectionDefaultIcon(key)
					}
					fmt.Printf("## %s %s (%d)\n\n", icon, g.Name, len(g.Items))
					for _, item := range g.Items {
						ref := cli.ItemRef(item)
						if ref != "" {
							fmt.Printf("- **%s** %s\n", ref, item.Title)
						} else {
							fmt.Printf("- %s\n", item.Title)
						}
					}
					fmt.Println()
				}

				return nil
			}

			// Human-readable table output (default)
			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			headerColor := color.New(color.Bold, color.FgCyan)
			green := color.New(color.FgGreen)

			headerColor.Printf("📦 Changelog — %s\n", periodLabel)
			fmt.Println(dim.Sprint(strings.Repeat("═", 40)))

			if len(allItems) == 0 {
				fmt.Println()
				fmt.Println(dim.Sprint("  No completed items in this period."))
				fmt.Println()
				return nil
			}

			for _, key := range groupOrder {
				g := groups[key]
				icon := g.Icon
				if icon == "" {
					icon = collectionDefaultIcon(key)
				}
				fmt.Println()
				bold.Printf("%s %s (%d)\n", icon, g.Name, len(g.Items))
				for _, item := range g.Items {
					ref := cli.ItemRef(item)
					refStr := ""
					if ref != "" {
						refStr = dim.Sprintf("%-10s", ref) + "  "
					}
					fmt.Printf("  %s%s\n", refStr, green.Sprint(item.Title))
				}
			}

			fmt.Println()
			return nil
		},
	}

	cmd.Flags().IntVar(&days, "days", 7, "show items completed in last N days")
	cmd.Flags().StringVar(&since, "since", "", "only show items completed after this date (YYYY-MM-DD)")
	cmd.Flags().StringVar(&parentRef, "parent", "", "only show items under a specific parent (ref, slug, or title)")

	return cmd
}

func activityCmd() *cobra.Command {
	var limit int
	var actor string
	var since string

	cmd := &cobra.Command{
		Use:   "activity",
		Short: "Show recent workspace activity — what agents and users changed (non-streaming)",
		Long: `Show a bounded, non-streaming snapshot of workspace activity.

Answers "what did other agents/users do since I last worked?" — the query
counterpart to the live 'pad project watch' stream. Backed by the same
enriched activity feed the web UI uses (item refs, titles, change details).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			params := url.Values{}
			if limit > 0 {
				params.Set("limit", strconv.Itoa(limit))
			}
			if actor != "" {
				params.Set("actor", actor)
			}
			if since != "" {
				// Validate locally for a friendly error before the round-trip;
				// the server applies the filter (a.created_at >= since) so LIMIT
				// counts post-filter rows.
				if _, err := time.Parse("2006-01-02", since); err != nil {
					return fmt.Errorf("invalid --since date (expected YYYY-MM-DD): %w", err)
				}
				params.Set("since", since)
			}

			activities, err := client.ListActivity(ws, params)
			if err != nil {
				return err
			}

			if formatFlag == "json" {
				if activities == nil {
					activities = []models.Activity{}
				}
				return cli.PrintJSON(activities)
			}

			if len(activities) == 0 {
				fmt.Println("No activity found.")
				return nil
			}

			dim := color.New(color.Faint)
			bold := color.New(color.Bold)
			cyan := color.New(color.FgCyan)

			for _, a := range activities {
				timeStr := dim.Sprint(a.CreatedAt.Format("2006-01-02 15:04"))

				actorName := a.ActorName
				if actorName == "" {
					actorName = a.Actor
				}
				if actorName == "" {
					actorName = "unknown"
				}

				// Target: item ref + title when the activity references an item,
				// otherwise a workspace-level (audit) event.
				target := ""
				if a.ItemRef != "" {
					target = " " + cyan.Sprint(a.ItemRef)
					if a.ItemTitle != "" {
						target += " " + a.ItemTitle
					}
				} else if a.ItemTitle != "" {
					target = " " + a.ItemTitle
				}

				fmt.Printf("%s  %s %s%s\n", timeStr, bold.Sprint(actorName), a.Action, target)

				// Surface field-level change details when present.
				if a.Metadata != "" {
					var meta struct {
						Changes string `json:"changes"`
					}
					if json.Unmarshal([]byte(a.Metadata), &meta) == nil && meta.Changes != "" {
						fmt.Printf("       %s\n", dim.Sprint(meta.Changes))
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of activity entries to return")
	cmd.Flags().StringVar(&actor, "actor", "", "filter by actor category: user or agent")
	cmd.Flags().StringVar(&since, "since", "", "only show activity on or after this date (YYYY-MM-DD)")

	return cmd
}

func watchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "watch",
		Short: "Stream real-time workspace activity (like kubectl get events --watch)",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg := getClient()
			ws := getWorkspace()

			sseURL := cfg.BaseURL() + "/api/v1/events?workspace=" + url.QueryEscape(ws)

			// Set up context with signal handling for graceful shutdown
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			dim := color.New(color.Faint)
			bold := color.New(color.Bold)
			greenColor := color.New(color.FgGreen)
			blueColor := color.New(color.FgBlue)
			grayColor := color.New(color.Faint)
			purpleColor := color.New(color.FgMagenta)

			fmt.Printf("👁  Watching %s... (Ctrl+C to stop)\n\n", bold.Sprint(ws))

			// Use an HTTP client with no timeout for SSE streaming
			httpClient := &http.Client{}

			req, err := http.NewRequestWithContext(ctx, "GET", sseURL, nil)
			if err != nil {
				return fmt.Errorf("create request: %w", err)
			}
			req.Header.Set("Accept", "text/event-stream")
			req.Header.Set("Cache-Control", "no-cache")

			resp, err := httpClient.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return nil // graceful shutdown
				}
				return fmt.Errorf("connect to event stream: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				return fmt.Errorf("event stream returned %d: %s", resp.StatusCode, string(body))
			}

			// Read SSE stream line by line
			scanner := bufio.NewScanner(resp.Body)
			var currentEvent string

			for scanner.Scan() {
				line := scanner.Text()

				if strings.HasPrefix(line, "event: ") {
					currentEvent = strings.TrimPrefix(line, "event: ")
					continue
				}

				if strings.HasPrefix(line, "data: ") {
					data := strings.TrimPrefix(line, "data: ")

					// Skip the initial "connected" event
					if currentEvent == "connected" {
						currentEvent = ""
						continue
					}

					// Parse the event data
					var evt struct {
						ItemID     string `json:"item_id"`
						Title      string `json:"title"`
						Collection string `json:"collection"`
						Actor      string `json:"actor"`
						Source     string `json:"source"`
						Timestamp  int64  `json:"timestamp"`
					}
					if err := json.Unmarshal([]byte(data), &evt); err != nil {
						currentEvent = ""
						continue
					}

					// Format timestamp
					ts := time.Now()
					if evt.Timestamp > 0 {
						ts = time.UnixMilli(evt.Timestamp)
					}
					timeStr := dim.Sprintf("%s", ts.Format("15:04:05"))

					// Determine emoji and color based on event type
					var emoji string
					var actionColor *color.Color
					var action string
					var prep string

					switch currentEvent {
					case "item_created":
						emoji = "✨"
						actionColor = greenColor
						action = "Created"
						prep = "in"
					case "item_updated":
						emoji = "✏️ "
						actionColor = blueColor
						action = "Updated"
						prep = "in"
					case "item_archived":
						emoji = "🗑️"
						actionColor = grayColor
						action = "Archived"
						prep = "from"
					case "item_restored":
						emoji = "♻️ "
						actionColor = greenColor
						action = "Restored"
						prep = "in"
					case "comment_created":
						emoji = "💬"
						actionColor = blueColor
						action = "Comment on"
						prep = "in"
					case "comment_deleted":
						emoji = "💬"
						actionColor = grayColor
						action = "Comment removed from"
						prep = "in"
					case "workspace_updated":
						emoji = "⚙️ "
						actionColor = blueColor
						action = "Workspace updated"
						prep = ""
					default:
						emoji = "•"
						actionColor = dim
						action = currentEvent
						prep = "in"
					}

					// Format actor with color
					actorStr := evt.Actor
					if actorStr == "" {
						actorStr = evt.Source
					}
					if actorStr == "" {
						actorStr = "unknown"
					}

					// Color agent actors in purple
					var actorFormatted string
					if actorStr == "agent" || actorStr == "cli" || evt.Source == "agent" || evt.Source == "cli" || evt.Source == "skill" {
						actorFormatted = purpleColor.Sprint(actorStr)
					} else {
						actorFormatted = actorStr
					}

					// Build the output line
					title := bold.Sprint(evt.Title)
					if evt.Title == "" && currentEvent == "workspace_updated" {
						fmt.Printf("%s  %s %s by %s\n",
							timeStr, emoji, actionColor.Sprint(action), actorFormatted)
					} else if evt.Collection != "" {
						fmt.Printf("%s  %s %s %q %s %s by %s\n",
							timeStr, emoji, actionColor.Sprint(action), title, prep, evt.Collection, actorFormatted)
					} else {
						fmt.Printf("%s  %s %s %q by %s\n",
							timeStr, emoji, actionColor.Sprint(action), title, actorFormatted)
					}

					currentEvent = ""
					continue
				}

				// Keepalive comments (lines starting with ":") — ignore
				// silently. We use a direct byte comparison rather than
				// strings.HasPrefix to dodge a long-standing staticcheck
				// SA4017 false positive on this specific branch (the two
				// sibling HasPrefix calls earlier in the loop don't trip
				// it; only this one does, which strongly suggests an SSA-
				// analysis quirk). Behaviour is identical for a single-
				// byte ASCII prefix.
				if len(line) > 0 && line[0] == ':' {
					continue
				}

				// Empty line is the event separator — already handled above
			}

			if err := scanner.Err(); err != nil {
				if ctx.Err() != nil {
					fmt.Println("\nStopped watching.")
					return nil
				}
				return fmt.Errorf("reading event stream: %w", err)
			}

			return nil
		},
	}
}

// --- agent roles ---
