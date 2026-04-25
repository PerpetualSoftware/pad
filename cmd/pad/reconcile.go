package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/xarmian/pad/internal/cli"
	"github.com/xarmian/pad/internal/models"
)

type reconcileFinding struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

type reconcileResult struct {
	ItemRef     string             `json:"item_ref"`
	ItemTitle   string             `json:"item_title"`
	Collection  string             `json:"collection"`
	Status      string             `json:"status,omitempty"`
	Repo        string             `json:"repo,omitempty"`
	Branch      string             `json:"branch,omitempty"`
	PullRequest *GitHubPR          `json:"pull_request,omitempty"`
	Findings    []reconcileFinding `json:"findings,omitempty"`
	Updated     bool               `json:"updated"`
}

type livePRPayload struct {
	Number    int    `json:"number"`
	URL       string `json:"url"`
	Title     string `json:"title"`
	State     string `json:"state"`
	HeadRef   string `json:"headRefName"`
	UpdatedAt string `json:"updatedAt"`
}

func reconcileCmd() *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:   "reconcile [item-ref]",
		Short: "Detect stale task, branch, and pull request state",
		Long: `Inspect items with linked code metadata, compare their stored GitHub PR and branch state
against live GitHub data, and report drift.

By default, reconcile only reports findings. With --apply, it refreshes stored GitHub PR metadata
on items when the live PR state differs, but it does not automatically change item statuses.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, _ := getClient()
			ws := getWorkspace()

			items, err := reconcileTargetItems(client, ws, args)
			if err != nil {
				return err
			}

			results := make([]reconcileResult, 0, len(items))
			updatedCount := 0
			for i := range items {
				result, err := reconcileItem(client, ws, &items[i], apply)
				if err != nil {
					return err
				}
				if result == nil {
					continue
				}
				if result.Updated {
					updatedCount++
				}
				if len(result.Findings) > 0 || result.Updated {
					results = append(results, *result)
				}
			}

			if formatFlag == "json" {
				return cli.PrintJSON(map[string]any{
					"apply":   apply,
					"updated": updatedCount,
					"results": results,
				})
			}

			if len(results) == 0 {
				if apply {
					fmt.Println("No stale code or task state found. No metadata updates were needed.")
				} else {
					fmt.Println("No stale code or task state found.")
				}
				return nil
			}

			dim := color.New(color.Faint)
			bold := color.New(color.Bold)
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("REF"), dim.Sprint("TITLE"), dim.Sprint("STATUS"), dim.Sprint("PR"), dim.Sprint("FINDINGS"))
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				dim.Sprint("───"), dim.Sprint("─────"), dim.Sprint("──────"), dim.Sprint("──"), dim.Sprint("────────"))
			for _, result := range results {
				prLabel := "—"
				if result.PullRequest != nil && result.PullRequest.Number > 0 {
					prLabel = fmt.Sprintf("#%d", result.PullRequest.Number)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
					bold.Sprint(result.ItemRef), result.ItemTitle, result.Status, prLabel, len(result.Findings))
			}
			tw.Flush()

			for _, result := range results {
				fmt.Printf("\n%s %s\n", bold.Sprint(result.ItemRef), result.ItemTitle)
				for _, finding := range result.Findings {
					fmt.Printf("  - [%s] %s\n", strings.ToUpper(finding.Severity), finding.Message)
				}
				if result.Updated {
					fmt.Println("  - [APPLIED] Refreshed stored PR metadata from GitHub")
				}
			}

			if apply {
				fmt.Printf("\nUpdated %d item(s).\n", updatedCount)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&apply, "apply", false, "refresh stored PR metadata from GitHub when it is stale")
	return cmd
}

func reconcileTargetItems(client *cli.Client, ws string, args []string) ([]models.Item, error) {
	if len(args) > 0 {
		item, err := client.GetItem(ws, args[0])
		if err != nil {
			return nil, err
		}
		if item.CodeContext == nil {
			return []models.Item{*item}, nil
		}
		return []models.Item{*item}, nil
	}

	items, err := client.ListItems(ws, url.Values{
		"include_archived": {"true"},
		"limit":            {"500"},
	})
	if err != nil {
		return nil, err
	}

	filtered := make([]models.Item, 0, len(items))
	for _, item := range items {
		if item.CodeContext != nil && item.CodeContext.Provider == "github" {
			filtered = append(filtered, item)
		}
	}
	return filtered, nil
}

func reconcileItem(client *cli.Client, ws string, item *models.Item, apply bool) (*reconcileResult, error) {
	if item == nil {
		return nil, nil
	}

	status := extractItemStatus(item.Fields)
	result := &reconcileResult{
		ItemRef:    cli.ItemRef(*item),
		ItemTitle:  item.Title,
		Collection: item.CollectionSlug,
		Status:     status,
	}

	if item.CodeContext == nil || item.CodeContext.Provider != "github" {
		return result, nil
	}

	result.Repo = item.CodeContext.Repo
	result.Branch = item.CodeContext.Branch
	storedPR := extractPRFromItem(item)
	result.PullRequest = storedPR

	var livePR *GitHubPR
	var prErr error
	if storedPR != nil && item.CodeContext.Repo != "" {
		livePR, prErr = fetchGitHubPRByNumber(item.CodeContext.Repo, storedPR.Number)
	}

	var branchExists *bool
	var branchErr error
	if item.CodeContext.Repo != "" && item.CodeContext.Branch != "" {
		exists, err := githubBranchExists(item.CodeContext.Repo, item.CodeContext.Branch)
		branchExists = &exists
		branchErr = err
	}

	result.Findings = buildReconcileFindings(item, livePR, prErr, branchExists, branchErr)

	if apply && livePR != nil && needsPRMetadataRefresh(item, livePR) {
		fields, err := mergeGitHubPRIntoFields(item.Fields, livePR)
		if err != nil {
			return nil, err
		}
		updatedItem, err := client.UpdateItem(ws, item.Slug, models.ItemUpdate{Fields: &fields})
		if err != nil {
			return nil, err
		}
		item.Fields = updatedItem.Fields
		item.CodeContext = updatedItem.CodeContext
		result.PullRequest = extractPRFromItem(updatedItem)
		result.Branch = updatedItem.CodeContext.Branch
		result.Repo = updatedItem.CodeContext.Repo
		result.Updated = true
	}

	return result, nil
}

func fetchGitHubPRByNumber(repo string, number int) (*GitHubPR, error) {
	if repo == "" || number == 0 {
		return nil, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("GitHub CLI (gh) not found. Install it from https://cli.github.com/")
	}

	out, err := exec.Command("gh", "pr", "view", fmt.Sprintf("%d", number), "--repo", repo, "--json", "number,url,title,state,headRefName,updatedAt").Output()
	if err != nil {
		return nil, fmt.Errorf("fetch PR #%d from %s: %w", number, repo, err)
	}

	var raw livePRPayload
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("parse PR metadata: %w", err)
	}

	return &GitHubPR{
		Number:    raw.Number,
		URL:       raw.URL,
		Title:     raw.Title,
		State:     raw.State,
		Branch:    raw.HeadRef,
		Repo:      repo,
		UpdatedAt: raw.UpdatedAt,
	}, nil
}

func githubBranchExists(repo, branch string) (bool, error) {
	if repo == "" || branch == "" {
		return false, nil
	}
	if _, err := exec.LookPath("gh"); err != nil {
		return false, fmt.Errorf("GitHub CLI (gh) not found. Install it from https://cli.github.com/")
	}

	ref := fmt.Sprintf("repos/%s/branches/%s", repo, url.PathEscape(branch))
	out, err := exec.Command("gh", "api", ref, "--silent").CombinedOutput()
	if err == nil {
		return true, nil
	}
	if strings.Contains(string(out), "HTTP 404") || strings.Contains(string(out), "Not Found") {
		return false, nil
	}
	return false, fmt.Errorf("check branch %s on %s: %w", branch, repo, err)
}

// buildReconcileFindings returns the reconcile findings for the given
// item. The item argument MUST be non-nil — the production caller
// (reconcileItem) and the reconcile_test.go fixtures all guarantee
// that. The previous `if item != nil && item.CodeContext == nil`
// guard was misleading: extractItemStatus(item.Fields) above would
// have already panicked on a nil item, so the nil-check half of the
// condition could never fire. Staticcheck flagged it as SA5011.
func buildReconcileFindings(item *models.Item, livePR *GitHubPR, prErr error, branchExists *bool, branchErr error) []reconcileFinding {
	findings := []reconcileFinding{}
	status := extractItemStatus(item.Fields)

	if item.CodeContext == nil {
		return findings
	}
	if prErr != nil {
		findings = append(findings, reconcileFinding{
			Code:     "pr_lookup_failed",
			Severity: "high",
			Message:  prErr.Error(),
		})
	}
	if branchErr != nil {
		findings = append(findings, reconcileFinding{
			Code:     "branch_lookup_failed",
			Severity: "medium",
			Message:  branchErr.Error(),
		})
	}

	if livePR != nil {
		if needsPRMetadataRefresh(item, livePR) {
			findings = append(findings, reconcileFinding{
				Code:     "stale_pr_metadata",
				Severity: "low",
				Message:  fmt.Sprintf("stored PR metadata is stale; GitHub now reports %s on %s", livePR.State, livePR.Branch),
			})
		}
		switch livePR.State {
		case "MERGED":
			if !isTerminalItemStatus(status) {
				findings = append(findings, reconcileFinding{
					Code:     "task_open_after_merge",
					Severity: "high",
					Message:  "pull request is merged, but the item status is still non-terminal",
				})
			}
		case "OPEN":
			if isTerminalItemStatus(status) {
				findings = append(findings, reconcileFinding{
					Code:     "task_closed_with_open_pr",
					Severity: "medium",
					Message:  "item is marked done, but its pull request is still open",
				})
			}
		case "CLOSED":
			if !isTerminalItemStatus(status) {
				findings = append(findings, reconcileFinding{
					Code:     "task_open_with_closed_pr",
					Severity: "medium",
					Message:  "pull request is closed without merge, but the item still looks active",
				})
			}
		}
	}

	if branchExists != nil && !*branchExists {
		severity := "low"
		message := "linked branch no longer exists on GitHub"
		if livePR != nil && livePR.State == "OPEN" {
			severity = "high"
			message = "pull request is open, but the linked branch no longer exists on GitHub"
		}
		findings = append(findings, reconcileFinding{
			Code:     "missing_branch",
			Severity: severity,
			Message:  message,
		})
	}

	return findings
}

func extractItemStatus(fieldsJSON string) string {
	if fieldsJSON == "" || fieldsJSON == "{}" {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return ""
	}
	status, _ := fields["status"].(string)
	return status
}

func isTerminalItemStatus(status string) bool {
	return models.IsTerminalStatusDefault(status)
}

func needsPRMetadataRefresh(item *models.Item, livePR *GitHubPR) bool {
	stored := extractPRFromItem(item)
	if stored == nil || livePR == nil {
		return false
	}
	if stored.Number != livePR.Number {
		return true
	}
	if stored.URL != livePR.URL || stored.Title != livePR.Title || stored.State != livePR.State || stored.Branch != livePR.Branch || stored.Repo != livePR.Repo {
		return true
	}
	return stored.UpdatedAt != livePR.UpdatedAt
}

func mergeGitHubPRIntoFields(fieldsJSON string, livePR *GitHubPR) (string, error) {
	fields := map[string]any{}
	if strings.TrimSpace(fieldsJSON) != "" && strings.TrimSpace(fieldsJSON) != "{}" {
		if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
			return "", fmt.Errorf("parse item fields: %w", err)
		}
	}

	fields["github_pr"] = GitHubPR{
		Number:    livePR.Number,
		URL:       livePR.URL,
		Title:     livePR.Title,
		State:     livePR.State,
		Branch:    livePR.Branch,
		Repo:      livePR.Repo,
		UpdatedAt: livePR.UpdatedAt,
	}

	payload, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshal item fields: %w", err)
	}
	return string(payload), nil
}
