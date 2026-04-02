package models

import "testing"

func TestExtractItemCodeContextFromGitHubPRFields(t *testing.T) {
	context := ExtractItemCodeContext(`{"github_pr":{"number":42,"url":"https://github.com/xarmian/pad/pull/42","title":"Add branch metadata","state":"OPEN","branch":"feat/task-123-branch-pr-metadata","repo":"xarmian/pad","updated_at":"2026-04-02T14:00:00Z"}}`)
	if context == nil {
		t.Fatal("expected code context")
	}
	if context.Provider != "github" {
		t.Fatalf("expected provider github, got %q", context.Provider)
	}
	if context.Branch != "feat/task-123-branch-pr-metadata" {
		t.Fatalf("expected branch metadata, got %q", context.Branch)
	}
	if context.Repo != "xarmian/pad" {
		t.Fatalf("expected repo xarmian/pad, got %q", context.Repo)
	}
	if context.PullRequest == nil {
		t.Fatal("expected pull request metadata")
	}
	if context.PullRequest.Number != 42 {
		t.Fatalf("expected PR #42, got #%d", context.PullRequest.Number)
	}
}

func TestExtractItemCodeContextReturnsNilForUnrelatedFields(t *testing.T) {
	if got := ExtractItemCodeContext(`{"status":"open"}`); got != nil {
		t.Fatal("expected nil code context for unrelated fields")
	}
}
