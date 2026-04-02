package main

import (
	"strings"
	"testing"

	"github.com/xarmian/pad/internal/models"
)

func TestBuildReconcileFindingsMergedPROpenTask(t *testing.T) {
	item := &models.Item{
		Fields:      `{"status":"open","github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/xarmian/pad/pull/41",
		Title:     "PR",
		State:     "MERGED",
		Branch:    "feat/test",
		Repo:      "xarmian/pad",
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
		Fields:      `{"status":"done","github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/xarmian/pad/pull/41",
		Title:     "PR",
		State:     "OPEN",
		Branch:    "feat/test",
		Repo:      "xarmian/pad",
		UpdatedAt: "2026-04-02T15:00:00Z",
	}

	findings := buildReconcileFindings(item, livePR, nil, nil, nil)
	if len(findings) != 1 || findings[0].Code != "task_closed_with_open_pr" {
		t.Fatalf("expected task_closed_with_open_pr only, got %#v", findings)
	}
}

func TestBuildReconcileFindingsMissingBranchOnOpenPR(t *testing.T) {
	item := &models.Item{
		Fields:      `{"status":"in-progress","github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"PR","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/xarmian/pad/pull/41",
		Title:     "PR",
		State:     "OPEN",
		Branch:    "feat/test",
		Repo:      "xarmian/pad",
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
		Fields:      `{"status":"open","github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"Old title","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`,
		CodeContext: models.ExtractItemCodeContext(`{"github_pr":{"number":41,"url":"https://github.com/xarmian/pad/pull/41","title":"Old title","state":"OPEN","branch":"feat/test","repo":"xarmian/pad","updated_at":"2026-04-02T15:00:00Z"}}`),
	}
	livePR := &GitHubPR{
		Number:    41,
		URL:       "https://github.com/xarmian/pad/pull/41",
		Title:     "New title",
		State:     "MERGED",
		Branch:    "feat/test",
		Repo:      "xarmian/pad",
		UpdatedAt: "2026-04-02T15:05:00Z",
	}

	if !needsPRMetadataRefresh(item, livePR) {
		t.Fatal("expected metadata refresh to be required")
	}
}

func TestMergeGitHubPRIntoFieldsPreservesOtherFields(t *testing.T) {
	fields, err := mergeGitHubPRIntoFields(`{"status":"open","priority":"high"}`, &GitHubPR{
		Number:    41,
		URL:       "https://github.com/xarmian/pad/pull/41",
		Title:     "PR",
		State:     "MERGED",
		Branch:    "feat/test",
		Repo:      "xarmian/pad",
		UpdatedAt: "2026-04-02T15:05:00Z",
	})
	if err != nil {
		t.Fatalf("mergeGitHubPRIntoFields error: %v", err)
	}
	if !strings.Contains(fields, `"priority":"high"`) {
		t.Fatalf("expected merged fields to preserve unrelated data, got %s", fields)
	}
	if !strings.Contains(fields, `"github_pr"`) {
		t.Fatalf("expected merged fields to contain github_pr payload, got %s", fields)
	}
}
