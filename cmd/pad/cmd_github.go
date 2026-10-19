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
	// Set once the PR has merged (TASK-3617).
	MergeSHA string `json:"merge_sha,omitempty"`
	MergedAt string `json:"merged_at,omitempty"`
}

// ghPRJSONFields is the field list both gh pr view calls ask for.
const ghPRJSONFields = "number,url,title,state,headRefName,updatedAt,mergeCommit,mergedAt"

// ghMergeCommit is gh's mergeCommit object; null until the PR merges.
type ghMergeCommit struct {
	OID string `json:"oid"`
}

func (m *ghMergeCommit) sha() string {
	if m == nil {
		return ""
	}
	return m.OID
}

// ghMergedAt is gh's mergedAt, with Go's zero time read as "not merged".
// gh prints null for an open PR today (measured on #2008); a client that
// serialises an unset time.Time prints 0001-01-01T00:00:00Z instead, and
// storing that would claim a merge that never happened (TASK-3617 r4 Q2).
func ghMergedAt(s string) string {
	if strings.HasPrefix(s, "0001-01-01") {
		return ""
	}
	return s
}

func githubCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "github",
		Short:   "Link GitHub pull requests to Pad items",
		Aliases: []string{"gh"},
		RunE:    unknownSubcommandRun,
		Long: `Link GitHub pull requests to Pad items and view their status.

Requires the GitHub CLI (gh) to be installed: https://cli.github.com/`,
		Example: `  pad github link TASK-5          # Link current branch's PR to TASK-5
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
//
// The prefix grammar mirrors the ref grammar collections.IsValidPrefix
// defines — a letter followed by letters or digits (BUG-2943). This pattern
// runs against an upper-cased branch name, so it matches the validator
// literally; the client-side ones are case-insensitive by design. It was `[A-Z]+` and therefore could not see a
// ref on a collection whose prefix carries a digit, so `pad github link` on a
// branch named `fix/ab1-42-something` silently found nothing. That was
// invisible while the ref parser refused digits too; widening the parser
// without widening this would have left the two disagreeing again, which is
// the whole shape of this bug.
var itemRefPattern = regexp.MustCompile(`([A-Z][A-Z0-9]*-\d+)`)

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

	out, err := exec.Command("gh", "pr", "view", "--json", ghPRJSONFields).Output()
	if err != nil {
		return nil, fmt.Errorf("no pull request found for the current branch. Create one with: gh pr create")
	}

	var raw struct {
		Number    int            `json:"number"`
		URL       string         `json:"url"`
		Title     string         `json:"title"`
		State     string         `json:"state"`
		Branch    string         `json:"headRefName"`
		UpdatedAt string         `json:"updatedAt"`
		Merge     *ghMergeCommit `json:"mergeCommit"`
		MergedAt  string         `json:"mergedAt"`
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
		MergeSHA:  raw.Merge.sha(),
		MergedAt:  ghMergedAt(raw.MergedAt),
	}, nil
}

func githubLinkCmd() *cobra.Command {
	var supplied GitHubPR
	cmd := &cobra.Command{
		Use:   "link [item-ref]",
		Short: "Link the current branch's PR to a Pad item",
		Long: `Link the current branch's GitHub pull request to a Pad item.

If no item ref is provided, attempts to auto-detect from the branch name.
For example, branch "fix/TASK-5-oauth-bug" would auto-link to TASK-5.`,
		Example: `  pad github link TASK-5
  pad github link fix-oauth-bug
  pad github link                 # auto-detect from branch name
  pad github link TASK-5 --pr-number 42 --pr-url https://github.com/acme/app/pull/42 --pr-state open`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			bold := color.New(color.Bold)
			dim := color.New(color.Faint)
			green := color.New(color.FgGreen, color.Bold)

			// Supplied metadata (TASK-3611): an agent with no checkout or `gh`
			// names the PR itself, and nothing is fetched from GitHub. The
			// server validates the number and URL, as for a fetched PR.
			var branch string
			var pr *GitHubPR
			// ANY supplied PR flag opts in (codex r1): a lone --pr-title must
			// not fall through to the branch and `gh`.
			if anyFlagChanged(cmd, "pr-number", "pr-url", "pr-title", "pr-state", "pr-branch", "pr-repo") {
				if supplied.Number <= 0 || supplied.URL == "" {
					return fmt.Errorf("--pr-number and --pr-url are both required to link a PR you name")
				}
				if len(args) == 0 {
					return fmt.Errorf("an item ref is required with --pr-number: pad github link TASK-5 --pr-number N --pr-url URL")
				}
				p := supplied
				pr = &p
			} else {
				// Step 1: Get current branch
				b, err := getCurrentBranch()
				if err != nil {
					return err
				}
				branch = b
				if formatFlag != "json" {
					dim.Printf("Branch: %s\n", branch)
				}

				// Step 2: Fetch PR info
				fetched, err := fetchGitHubPR()
				if err != nil {
					return err
				}
				pr = fetched
			}

			text := formatFlag != "json" // JSON output carries only the item
			if text {
				stateColor := prStateColor(pr.State)
				fmt.Printf("PR #%d  %s  %s\n", pr.Number, stateColor.Sprint(pr.State), dim.Sprint(pr.URL))
				fmt.Printf("  %s\n\n", bold.Sprint(pr.Title))
			}

			// Step 3: Determine target item
			var itemRef string
			if len(args) > 0 {
				itemRef = args[0]
			} else {
				itemRef = extractItemRefFromBranch(branch)
				if itemRef == "" {
					return fmt.Errorf("could not detect item ref from branch %q. Specify one: pad github link TASK-5", branch)
				}
				if text {
					dim.Printf("Auto-detected item ref: %s\n", itemRef)
				}
			}

			// Step 4: Resolve the item
			item, err := client.GetItem(ws, itemRef)
			if err != nil {
				return fmt.Errorf("item %q not found: %w", itemRef, err)
			}

			// Step 5: write the PR onto the item as a one-key PATCH.
			//
			// BUG-3049: this used to decode the item's whole `fields` blob,
			// add `github_pr`, and PATCH the blob back — so anything written to
			// the item between the GetItem above and this write was reverted.
			// A patch names only `github_pr`, and the server merges it under the
			// row lock. Since BUG-2696 it goes through the typed, validated
			// github_pr member: fields_patch refuses the key, because a
			// `--field github_pr=...` write stored an unreadable string.
			updated, err := client.UpdateItem(ws, item.Slug, gitHubPRUpdate(pr))
			if err != nil {
				return fmt.Errorf("failed to update item: %w", err)
			}
			if err := verifyPRLinked(updated, pr.Number); err != nil {
				return err
			}

			// --format json answers with the item, as the remote MCP door does
			// (TASK-3611).
			if formatFlag == "json" {
				outputJSON(updated)
				return nil
			}
			ref := cli.ItemRef(*item)
			green.Printf("✓ Linked PR #%d (%s) → %s %q\n", pr.Number, pr.Repo, ref, item.Title)
			return nil
		},
	}
	cmd.Flags().IntVar(&supplied.Number, "pr-number", 0, "link this PR number instead of the current branch's (with --pr-url; nothing is fetched from GitHub)")
	cmd.Flags().StringVar(&supplied.URL, "pr-url", "", "the PR's URL, with --pr-number")
	cmd.Flags().StringVar(&supplied.Title, "pr-title", "", "the PR's title, with --pr-number")
	cmd.Flags().StringVar(&supplied.State, "pr-state", "", "the PR's state (e.g. open, merged, closed), with --pr-number")
	cmd.Flags().StringVar(&supplied.Branch, "pr-branch", "", "the PR's head branch, with --pr-number")
	cmd.Flags().StringVar(&supplied.Repo, "pr-repo", "", "the PR's repository (owner/name), with --pr-number")
	return cmd
}

func githubStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status [item-ref]",
		Short: "Show GitHub PR status for linked items",
		Long:  `Show the GitHub PR status for one or all items that have linked PRs.`,
		Example: `  pad github status               # Show all items with linked PRs
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
			items, err := githubStatusScanItems(client, ws)
			if err != nil {
				return err
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
		Use:     "unlink <item-ref>",
		Short:   "Remove the GitHub PR link from an item",
		Example: `  pad github unlink TASK-5`,
		Args:    cobra.ExactArgs(1),
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

			// BUG-3049: delete the one key, not rewrite the blob. Since
			// BUG-2696 through the typed clear_github_pr member (fields_patch
			// refuses the key); the server lowers it to a patch DELETE. The
			// decode above stays: it is how "has no linked PR" is detected.
			updated, err := client.UpdateItem(ws, item.Slug, models.ItemUpdate{ClearGitHubPR: true})
			if err != nil {
				return err
			}
			if err := verifyPRUnlinked(updated); err != nil {
				return err
			}

			green := color.New(color.FgGreen, color.Bold)
			if formatFlag == "json" {
				outputJSON(updated)
				return nil
			}
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
		MergeSHA:  pr.MergeSHA,
		MergedAt:  pr.MergedAt,
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

// anyFlagChanged reports whether any of names was set on cmd.
func anyFlagChanged(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// githubStatusScanItems is every item of every collection, archived included,
// that `pad github status` scans for linked PRs. Every page of each collection
// is read (BUG-3793): a 100-row limit silently dropped the rest of a larger
// collection from the report. A collection that fails to list is skipped, as
// before.
func githubStatusScanItems(client *cli.Client, ws string) ([]models.Item, error) {
	colls, err := client.ListCollections(ws)
	if err != nil {
		return nil, err
	}
	var items []models.Item
	for _, coll := range colls {
		collItems, err := client.ListAllCollectionItems(ws, coll.Slug, url.Values{
			"include_archived": {"true"},
		})
		if err != nil {
			continue
		}
		items = append(items, collItems...)
	}
	return items, nil
}
