package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PerpetualSoftware/pad/internal/cli"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

func needsListCmd() *cobra.Command {
	var view, cursor string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the items waiting on you, oldest first",
		Long:  "List the items waiting on you across workspaces, oldest-waiting first. Only an explicit --workspace narrows the list; a linked directory does not. --view hidden lists what your dismissals, deferrals and not-mine marks hide.",
		Example: `  pad needs list
  pad needs list --view hidden
  pad --workspace docapp needs list`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch view {
			case "", "active", "hidden":
			default:
				return fmt.Errorf("--view must be active or hidden")
			}
			ws := ""
			if cmd.Flags().Changed("workspace") {
				ws = workspaceFlag
			}
			client, _ := getClient()
			raw, err := client.ListNeeds(view, ws, cursor, limit)
			if err != nil {
				return err
			}
			if formatFlag == "json" {
				return printNeedsJSON(cmd, raw)
			}
			return printNeedsTable(cmd, raw, view == "hidden")
		},
	}
	cmd.Flags().StringVar(&view, "view", "", "active (default) or hidden")
	cmd.Flags().IntVar(&limit, "limit", 0, "rows per page (server default 50, max 200)")
	cmd.Flags().StringVar(&cursor, "cursor", "", "next_cursor from the previous page")
	return cmd
}

// needsGiveTarget is who a not-mine hands the item to, resolved once.
type needsGiveTarget struct {
	Kind  string `json:"kind"` // "role" or "user"
	ID    string `json:"id"`
	Label string `json:"label"`
}

func needsNotMineCmd() *cobra.Command {
	var toRole, toUser string
	cmd := &cobra.Command{
		Use:   "not-mine <ref>...",
		Short: "Mark items not mine, optionally giving them to a role or person",
		Long: "Hide items from your inbox until they come back to you. --to-role or --to-user first reassigns each item " +
			"(a role clears the person), guarded by the item's current seq; a refused reassignment writes nothing and stops. " +
			"Refs are processed in order and processing stops at the first error.",
		Example: `  pad needs not-mine TASK-5
  pad --workspace docapp needs not-mine TASK-5 TASK-6 --to-role reviewer
  pad needs not-mine TASK-5 --to-user alice@example.com`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			var target *needsGiveTarget
			switch {
			case toRole != "":
				role, err := client.GetAgentRole(ws, toRole)
				if err != nil {
					return fmt.Errorf("resolve role: %w", err)
				}
				if role == nil || role.ID == "" {
					return fmt.Errorf("role %q not found", toRole)
				}
				target = &needsGiveTarget{Kind: "role", ID: role.ID, Label: role.Name}
			case toUser != "":
				members, err := client.ListWorkspaceMembers(ws)
				if err != nil {
					return fmt.Errorf("resolve user: %w", err)
				}
				for _, m := range members {
					if strings.EqualFold(m.UserName, toUser) || strings.EqualFold(m.UserEmail, toUser) {
						label := m.UserName
						if label == "" {
							label = m.UserEmail
						}
						target = &needsGiveTarget{Kind: "user", ID: m.UserID, Label: label}
						break
					}
				}
				if target == nil {
					return fmt.Errorf("user %q not found in workspace members", toUser)
				}
			}
			var done []string
			var results []map[string]any
			for _, ref := range args {
				res, err := needsNotMineOne(client, ws, ref, target)
				if err != nil {
					if len(done) > 0 {
						return fmt.Errorf("%s: %w (already done: %s)", ref, err, strings.Join(done, ", "))
					}
					return fmt.Errorf("%s: %w", ref, err)
				}
				done = append(done, ref)
				results = append(results, res)
				if formatFlag != "json" {
					if target != nil {
						fmt.Fprintf(cmd.OutOrStdout(), "%s given to %s and marked not mine\n", ref, target.Label)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "%s marked not mine\n", ref)
					}
				}
			}
			if formatFlag == "json" {
				return printNeedsJSON(cmd, map[string]any{"results": results})
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&toRole, "to-role", "", "give each item to this agent role (slug) first; clears the person")
	cmd.Flags().StringVar(&toUser, "to-user", "", "give each item to this workspace member (name or email) first")
	cmd.MarkFlagsMutuallyExclusive("to-role", "to-user")
	return cmd
}

// needsNotMineOne reassigns one item when target is set (PATCH guarded by the
// seq just read), then marks it not mine with an empty body. A refused PATCH
// returns before the PUT, so nothing is written.
func needsNotMineOne(client *cli.Client, ws, ref string, target *needsGiveTarget) (map[string]any, error) {
	if target != nil {
		item, err := client.GetItem(ws, ref)
		if err != nil {
			return nil, err
		}
		seq := item.Seq
		input := models.ItemUpdate{ExpectedSeq: &seq}
		id := target.ID
		if target.Kind == "role" {
			input.AgentRoleID = &id
			input.ClearAssignedUser = true
		} else {
			input.AssignedUserID = &id
		}
		if _, err := client.UpdateItem(ws, ref, input); err != nil {
			if apiErr := cli.AsAPIError(err); apiErr != nil {
				if uc := apiErr.AsUpdateConflict(); uc != nil {
					cli.WriteUpdateConflictError(os.Stderr, apiErr, uc)
					return nil, fmt.Errorf("reassignment rejected: item was modified by another writer; nothing was written")
				}
			}
			return nil, fmt.Errorf("reassign: %w", err)
		}
	}
	raw, err := client.NotMineNeeds(ws, ref)
	if err != nil {
		if target != nil {
			return nil, fmt.Errorf("gave it to %s, but marking it not mine failed: %w", target.Label, err)
		}
		return nil, err
	}
	res := map[string]any{}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	if target != nil {
		res["given_to"] = target
	}
	return res, nil
}

func needsRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <ref>...",
		Short: "Show items in your inbox again",
		Long:  "Remove your not-mine mark and your dismissal or deferral from each item, in order. Both removals are idempotent.",
		Example: `  pad needs restore TASK-5
  pad --workspace docapp needs restore TASK-5 TASK-6`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()
			var results []map[string]any
			for _, ref := range args {
				if _, err := client.UnNotMineNeeds(ws, ref); err != nil {
					return fmt.Errorf("%s: %w", ref, err)
				}
				if _, err := client.UnsnoozeNeeds(ws, ref); err != nil {
					return fmt.Errorf("%s: %w", ref, err)
				}
				results = append(results, map[string]any{"ref": ref, "not_mine": nil, "snooze": nil})
				if formatFlag != "json" {
					fmt.Fprintf(cmd.OutOrStdout(), "%s restored\n", ref)
				}
			}
			if formatFlag == "json" {
				return printNeedsJSON(cmd, map[string]any{"results": results})
			}
			return nil
		},
	}
}

func printNeedsJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

type needsListPage struct {
	Rows []struct {
		Workspace struct {
			Slug string `json:"slug"`
		} `json:"workspace"`
		Item struct {
			Ref   string `json:"ref"`
			Title string `json:"title"`
		} `json:"item"`
		Why struct {
			Text string `json:"text"`
		} `json:"why"`
		WaitingSince time.Time            `json:"waiting_since"`
		Snooze       *models.NeedsSnooze  `json:"snooze"`
		NotMine      *models.NeedsNotMine `json:"not_mine"`
	} `json:"rows"`
	NextCursor string `json:"next_cursor"`
	Total      int    `json:"total"`
	Hidden     int    `json:"hidden"`
}

func needsDate(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return cli.FormatInstant(t, "2006-01-02")
	}
	return s
}

func printNeedsTable(cmd *cobra.Command, raw json.RawMessage, hidden bool) error {
	var page needsListPage
	if err := json.Unmarshal(raw, &page); err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if len(page.Rows) == 0 {
		fmt.Fprintln(out, "Nothing is waiting on you.")
	} else {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		last := "WAITING"
		if hidden {
			last = "HIDDEN"
		}
		fmt.Fprintf(tw, "WORKSPACE\tREF\tTITLE\tWHY\t%s\n", last)
		for _, r := range page.Rows {
			col := ""
			if !hidden {
				col = cli.RelativeTime(r.WaitingSince)
			} else {
				switch {
				case r.NotMine != nil:
					col = "not mine since " + needsDate(r.NotMine.CreatedAt)
				case r.Snooze != nil && r.Snooze.Kind == models.NeedsSnoozeDefer:
					col = "deferred until " + needsDate(r.Snooze.DeferUntil)
				case r.Snooze != nil:
					col = "dismissed"
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Workspace.Slug, r.Item.Ref, r.Item.Title, r.Why.Text, col)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "\n%d of %d shown; %d hidden\n", len(page.Rows), page.Total, page.Hidden)
	if page.NextCursor != "" {
		fmt.Fprintf(out, "More: pad needs list --cursor %s (with the same --view)\n", page.NextCursor)
	}
	return nil
}
