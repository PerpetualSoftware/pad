package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"regexp"

	"github.com/PerpetualSoftware/pad/internal/cli"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// GitHubPR holds PR data stored in item fields.
type GitHubPR struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	Branch    string `json:"branch"`
	Repo      string `json:"repo"`
	UpdatedAt string `json:"updated_at"`
}

func githubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "github",
		Short:   "Link GitHub pull requests to Pad items",
		Aliases: []string{"gh"},
		RunE:    unknownSubcommandRun,
		Long: `Link GitHub pull requests to Pad items and view their status.

Requires the GitHub CLI (gh) to be installed: https://cli.github.com/

Examples:
  pad github link TASK-5          # Link current branch's PR to TASK-5
  pad github link                 # Auto-detect item ref from branch name
  pad github status               # Show PR status for all linked items
  pad github status TASK-5        # Show PR status for a specific item
  pad github unlink TASK-5        # Remove PR link from an item`,
	}

	cmd.AddCommand(
		githubLinkCmd(),
		githubStatusCmd(),
		githubUnlinkCmd(),
	)

	return cmd
}

// getCurrentBranch returns the current git branch name.
func getCurrentBranch() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository or git not available")
	}
	return strings.TrimSpace(string(out)), nil
}

// extractItemRefFromBranch attempts to find a Pad item reference (e.g. TASK-5, BUG-3) in a branch name.
var itemRefPattern = regexp.MustCompile(`([A-Z]+-\d+)`)

func extractItemRefFromBranch(branch string) string {
	// Convert to uppercase for matching since branch names are often lowercase
	upper := strings.ToUpper(branch)
	match := itemRefPattern.FindString(upper)
	return match
}

// fetchGitHubPR fetches PR data for the current branch using the gh CLI.
func fetchGitHubPR() (*GitHubPR, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("GitHub CLI (gh) not found. Install it from https://cli.github.com/")
	}

	out, err := exec.Command("gh", "pr", "view", "--json", "number,url,title,state,headRefName,updatedAt").Output()
	if err != nil {
		return nil, fmt.Errorf("no pull request found for the current branch. Create one with: gh pr create")
	}

	var raw struct {
		Number    int    `json:"number"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		State     string `json:"state"`
		Branch    string `json:"headRefName"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("failed to parse gh output: %w", err)
	}

	// Extract owner/repo from the PR URL (e.g. https://github.com/PerpetualSoftware/pad/pull/5)
	repo := ""
	if parts := strings.Split(raw.URL, "/"); len(parts) >= 5 {
		repo = parts[3] + "/" + parts[4]
	}

	return &GitHubPR{
		Number:    raw.Number,
		URL:       raw.URL,
		Title:     raw.Title,
		State:     raw.State,
		Branch:    raw.Branch,
		Repo:      repo,
		UpdatedAt: raw.UpdatedAt,
	}, nil
}

func githubLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "link [item-ref]",
		Short: "Link the current branch's PR to a Pad item",
		Long: `Link the current branch's GitHub pull request to a Pad item.

If no item ref is provided, attempts to auto-detect from the branch name.
For example, branch "fix/TASK-5-oauth-bug" would auto-link to TASK-5.

Examples:
  pad github link TASK-5
  pad github link fix-oauth-bug
  pad github link                 # auto-detect from branch name`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			green := color.New(color.FgGreen, color.Bold)

			// Step 1: Get current branch
			branch, err := getCurrentBranch()
			if err != nil {
				return err
			}
			dim.Printf("Branch: %s\n", branch)

			// Step 2: Fetch PR info
			pr, err := fetchGitHubPR()
			if err != nil {
				return err
			}

			stateColor := prStateColor(pr.State)
			fmt.Printf("PR #%d  %s  %s\n", pr.Number, stateColor.Sprint(pr.State), dim.Sprint(pr.URL))
			fmt.Printf("  %s\n\n", bold.Sprint(pr.Title))

			// Step 3: Determine target item
			var itemRef string
			if len(args) > 0 {
				itemRef = args[0]
			} else {
				itemRef = extractItemRefFromBranch(branch)
				if itemRef == "" {
					return fmt.Errorf("could not detect item ref from branch %q. Specify one: pad github link TASK-5", branch)
				}
				dim.Printf("Auto-detected item ref: %s\n", itemRef)
			}

			// Step 4: Resolve the item
			item, err := client.GetItem(ws, itemRef)
			if err != nil {
				return fmt.Errorf("item %q not found: %w", itemRef, err)
			}

			// Step 5: Update item fields with PR data
			var fieldsMap map[string]interface{}
			if item.Fields != "" && item.Fields != "{}" {
				if err := json.Unmarshal([]byte(item.Fields), &fieldsMap); err != nil {
					fieldsMap = make(map[string]interface{})
				}
			} else {
				fieldsMap = make(map[string]interface{})
			}

			fieldsMap["github_pr"] = GitHubPR{
				Number:    pr.Number,
				URL:       pr.URL,
				Title:     pr.Title,
				State:     pr.State,
				Branch:    pr.Branch,
				Repo:      pr.Repo,
				UpdatedAt: pr.UpdatedAt,
			}

			fieldsJSON, err := json.Marshal(fieldsMap)
			if err != nil {
				return fmt.Errorf("failed to marshal fields: %w", err)
			}
			fields := string(fieldsJSON)

			_, err = client.UpdateItem(ws, item.Slug, models.ItemUpdate{
				Fields: &fields,
			})
			if err != nil {
				return fmt.Errorf("failed to update item: %w", err)
			}

			ref := cli.ItemRef(*item)
			green.Printf("✓ Linked PR #%d (%s) → %s %q\n", pr.Number, pr.Repo, ref, item.Title)
			return nil
		},
	}
}

func githubStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [item-ref]",
		Short: "Show GitHub PR status for linked items",
		Long: `Show the GitHub PR status for one or all items that have linked PRs.

Examples:
  pad github status               # Show all items with linked PRs
  pad github status TASK-5        # Show PR status for a specific item`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)

			if len(args) > 0 {
				// Single item mode
				item, err := client.GetItem(ws, args[0])
				if err != nil {
					return err
				}
				return showItemPRStatus(item, bold, dim)
			}

			// All items mode — scan across all collections for items with github_pr in fields
			colls, err := client.ListCollections(ws)
			if err != nil {
				return err
			}
			var items []models.Item
			for _, coll := range colls {
				collItems, err := client.ListCollectionItems(ws, coll.Slug, url.Values{
					"limit":            {"100"},
					"include_archived": {"true"},
				})
				if err != nil {
					continue
				}
				items = append(items, collItems...)
			}

			if formatFlag == "json" {
				type prStatus struct {
					Ref   string   `json:"ref"`
					Title string   `json:"title"`
					PR    GitHubPR `json:"github_pr"`
				}
				var results []prStatus
				for _, item := range items {
					pr := extractPRFromItem(&item)
					if pr != nil {
						results = append(results, prStatus{
							Ref:   cli.ItemRef(item),
							Title: item.Title,
							PR:    *pr,
						})
					}
				}
				return cli.PrintJSON(results)
			}

			tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("REF"), dim.Sprint("TITLE"), dim.Sprint("PR"), dim.Sprint("STATE"), dim.Sprint("UPDATED"))
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("───"), dim.Sprint("─────"), dim.Sprint("──"), dim.Sprint("─────"), dim.Sprint("───────"))

			count := 0
			for _, item := range items {
				pr := extractPRFromItem(&item)
				if pr == nil {
					continue
				}
				count++

				ref := cli.ItemRef(item)
				title := item.Title
				if len(title) > 40 {
					title = title[:37] + "..."
				}
				stateColor := prStateColor(pr.State)
				updatedAgo := ""
				if pr.UpdatedAt != "" {
					if t, err := time.Parse(time.RFC3339, pr.UpdatedAt); err == nil {
						updatedAgo = relativeTimeStr(t)
					}
				}

				fmt.Fprintf(tw, "%s\t%s\t#%d\t%s\t%s\n",
					bold.Sprint(ref), title, pr.Number, stateColor.Sprint(pr.State), dim.Sprint(updatedAgo))
			}
			tw.Flush()

			if count == 0 {
				fmt.Println(dim.Sprint("\nNo items have linked PRs. Use: pad github link TASK-5"))
			} else {
				fmt.Printf("\n%d item(s) with linked PRs\n", count)
			}
			return nil
		},
	}
}

func githubUnlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <item-ref>",
		Short: "Remove the GitHub PR link from an item",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			item, err := client.GetItem(ws, args[0])
			if err != nil {
				return err
			}

			var fieldsMap map[string]interface{}
			if err := json.Unmarshal([]byte(item.Fields), &fieldsMap); err != nil {
				return fmt.Errorf("failed to parse item fields: %w", err)
			}

			if _, ok := fieldsMap["github_pr"]; !ok {
				return fmt.Errorf("item %q has no linked PR", args[0])
			}

			delete(fieldsMap, "github_pr")
			fieldsJSON, _ := json.Marshal(fieldsMap)
			fields := string(fieldsJSON)

			_, err = client.UpdateItem(ws, item.Slug, models.ItemUpdate{
				Fields: &fields,
			})
			if err != nil {
				return err
			}

			green := color.New(color.FgGreen, color.Bold)
			green.Printf("✓ Removed PR link from %s %q\n", cli.ItemRef(*item), item.Title)
			return nil
		},
	}
}

// Helper functions for GitHub integration

func showItemPRStatus(item *models.Item, bold, dim *color.Color) error {
	pr := extractPRFromItem(item)
	if pr == nil {
		return fmt.Errorf("item %q has no linked PR", item.Slug)
	}

	ref := cli.ItemRef(*item)
	stateColor := prStateColor(pr.State)

	bold.Printf("%s  %s\n", ref, item.Title)
	fmt.Printf("PR #%d  %s  %s\n", pr.Number, stateColor.Sprint(pr.State), dim.Sprint(pr.URL))
	if pr.Branch != "" {
		fmt.Printf("Branch: %s\n", dim.Sprint(pr.Branch))
	}
	if pr.Repo != "" {
		fmt.Printf("Repo:   %s\n", dim.Sprint(pr.Repo))
	}
	if pr.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, pr.UpdatedAt); err == nil {
			fmt.Printf("Updated: %s\n", dim.Sprint(relativeTimeStr(t)))
		}
	}
	return nil
}

func extractPRFromItem(item *models.Item) *GitHubPR {
	if item == nil || item.CodeContext == nil || item.CodeContext.PullRequest == nil {
		return nil
	}
	pr := item.CodeContext.PullRequest
	return &GitHubPR{
		Number:    pr.Number,
		URL:       pr.URL,
		Title:     pr.Title,
		State:     pr.State,
		Branch:    item.CodeContext.Branch,
		Repo:      item.CodeContext.Repo,
		UpdatedAt: pr.UpdatedAt,
	}
}

func prStateColor(state string) *color.Color {
	switch state {
	case "OPEN":
		return color.New(color.FgGreen, color.Bold)
	case "MERGED":
		return color.New(color.FgMagenta, color.Bold)
	case "CLOSED":
		return color.New(color.FgRed)
	default:
		return color.New(color.Faint)
	}
}

func relativeTimeStr(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// --- database tools ---
