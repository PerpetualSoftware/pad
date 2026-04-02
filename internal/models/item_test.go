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

func TestExtractItemImplementationNotes(t *testing.T) {
	notes := ExtractItemImplementationNotes(`{"status":"open","implementation_notes":[{"id":"note-1","summary":"Used SSE refresh","details":"Reload phase tasks on visibility resume","created_at":"2026-04-02T15:00:00Z","created_by":"agent"}]}`)
	if len(notes) != 1 {
		t.Fatalf("expected 1 implementation note, got %d", len(notes))
	}
	if notes[0].Summary != "Used SSE refresh" {
		t.Fatalf("expected note summary, got %q", notes[0].Summary)
	}
	if notes[0].CreatedBy != "agent" {
		t.Fatalf("expected created_by agent, got %q", notes[0].CreatedBy)
	}
}

func TestExtractItemDecisionLog(t *testing.T) {
	log := ExtractItemDecisionLog(`{"status":"open","decision_log":[{"id":"decision-1","decision":"Use explicit setup state","rationale":"needs_setup was too ambiguous","created_at":"2026-04-02T15:05:00Z","created_by":"user"}]}`)
	if len(log) != 1 {
		t.Fatalf("expected 1 decision entry, got %d", len(log))
	}
	if log[0].Decision != "Use explicit setup state" {
		t.Fatalf("expected decision text, got %q", log[0].Decision)
	}
	if log[0].Rationale == "" {
		t.Fatal("expected rationale to be preserved")
	}
}

func TestAppendImplementationNotePreservesExistingFields(t *testing.T) {
	fields, err := AppendImplementationNote(`{"status":"open","priority":"high"}`, ItemImplementationNote{
		ID:      "note-1",
		Summary: "Added setup guidance",
		Details: "Updated the login screen copy",
	})
	if err != nil {
		t.Fatalf("AppendImplementationNote error: %v", err)
	}
	if got := ExtractItemImplementationNotes(fields); len(got) != 1 {
		t.Fatalf("expected 1 note after append, got %#v", got)
	}
	if ExtractItemCodeContext(fields) != nil {
		t.Fatal("did not expect code context")
	}
}

func TestAppendDecisionLogEntryPreservesExistingNotes(t *testing.T) {
	fields, err := AppendDecisionLogEntry(`{"implementation_notes":[{"id":"note-1","summary":"Keep docker as external-managed"}]}`, ItemDecisionLogEntry{
		ID:       "decision-1",
		Decision: "Keep docker in external-managed mode",
	})
	if err != nil {
		t.Fatalf("AppendDecisionLogEntry error: %v", err)
	}
	if got := ExtractItemImplementationNotes(fields); len(got) != 1 {
		t.Fatalf("expected implementation notes to be preserved, got %#v", got)
	}
	if got := ExtractItemDecisionLog(fields); len(got) != 1 {
		t.Fatalf("expected 1 decision log entry, got %#v", got)
	}
}
