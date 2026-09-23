package main

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestBuildReconcileFindingsMergedPROpenTask(t *testing.T) {
	item := &models.Item{
		Fields:      `{"status":"open","github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/PerpetualSoftware/pad/pull/41",
		Title:     "PR",
		State:     "MERGED",
		Branch:    "feat/test",
		Repo:      "PerpetualSoftware/pad",
		UpdatedAt: "2026-04-02T15:05:00Z",
	}

	findings := buildReconcileFindings(item, livePR, nil, nil, nil)
	if len(findings) < 2 {
		t.Fatalf("expected at least stale metadata and task-open-after-merge findings, got %#v", findings)
	}

	var hasTaskMismatch bool
	for _, finding := range findings {
		if finding.Code == "task_open_after_merge" {
			hasTaskMismatch = true
			break
		}
	}
	if !hasTaskMismatch {
		t.Fatalf("expected task_open_after_merge finding, got %#v", findings)
	}
}

func TestBuildReconcileFindingsOpenPRDoneTask(t *testing.T) {
	item := &models.Item{
		Fields:      `{"status":"done","github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/PerpetualSoftware/pad/pull/41",
		Title:     "PR",
		State:     "OPEN",
		Branch:    "feat/test",
		Repo:      "PerpetualSoftware/pad",
		UpdatedAt: "2026-04-02T15:00:00Z",
	}

	findings := buildReconcileFindings(item, livePR, nil, nil, nil)
	if len(findings) != 1 || findings[0].Code != "task_closed_with_open_pr" {
		t.Fatalf("expected task_closed_with_open_pr only, got %#v", findings)
	}
}

func TestBuildReconcileFindingsMissingBranchOnOpenPR(t *testing.T) {
	item := &models.Item{
		Fields:      `{"status":"in-progress","github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/PerpetualSoftware/pad/pull/41",
		Title:     "PR",
		State:     "OPEN",
		Branch:    "feat/test",
		Repo:      "PerpetualSoftware/pad",
		UpdatedAt: "2026-04-02T15:00:00Z",
	}
	branchExists := false

	findings := buildReconcileFindings(item, livePR, nil, &branchExists, nil)
	if len(findings) != 1 || findings[0].Code != "missing_branch" || findings[0].Severity != "high" {
		t.Fatalf("expected high-severity missing_branch finding, got %#v", findings)
	}
}

func TestNeedsPRMetadataRefresh(t *testing.T) {
	item := &models.Item{
		Fields:      `{"status":"open","github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"Old title","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/PerpetualSoftware/pad/pull/41","title":"Old title","state":"OPEN","branch":"feat/test","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/PerpetualSoftware/pad/pull/41",
		Title:     "New title",
		State:     "MERGED",
		Branch:    "feat/test",
		Repo:      "PerpetualSoftware/pad",
		UpdatedAt: "2026-04-02T15:05:00Z",
	}

	if !needsPRMetadataRefresh(item, livePR) {
		t.Fatal("expected metadata refresh to be required")
	}
}

// BUG-3049: the reconcile write names ONE key. The predecessor of this test
// asserted that a full-blob merge PRESERVED unrelated fields, which was true of
// the blob it built and false of the row it landed on — anything written between
// the item read and the write was reverted. The property that actually holds is
// that the patch contains github_pr and nothing else, so no other field is
// reachable by this write.
func TestGitHubPRUpdateNamesOnlyGitHubPR(t *testing.T) {
	// Since BUG-2696 the write is the typed github_pr member, not a
	// fields_patch entry. BUG-3049's property still holds: nothing but the
	// PR is in the update, so no other field is reachable by it.
	up := gitHubPRUpdate(&GitHubPR{
		Number:    41,
		URL:       "https://github.com/PerpetualSoftware/pad/pull/41",
		Title:     "PR",
		State:     "MERGED",
		Branch:    "feat/test",
		Repo:      "PerpetualSoftware/pad",
		UpdatedAt: "2026-04-02T15:05:00Z",
	})
	if up.Fields != nil || up.FieldsPatch != nil {
		t.Fatalf("the PR update must carry no field blob or patch: %+v", up)
	}
	if up.GitHubPR == nil || up.GitHubPR.Number != 41 || up.GitHubPR.State != "MERGED" {
		t.Fatalf("the update carries the wrong PR: %+v", up.GitHubPR)
	}
}
