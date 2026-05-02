package models

import "testing"

func TestExtractItemCodeContextFromGitHubPRFields(t *testing.T) {
	context := ExtractItemCodeContext(`{"github_pr":{"number":42,"url":"https://github.com/PerpetualSoftware/pad/pull/42","title":"Add branch metadata","state":"OPEN","branch":"feat/task-123-branch-pr-metadata","repo":"PerpetualSoftware/pad","updated_at":"2026-04-02T14:00:00Z"}}`)
	if context == nil {
		t.Fatal("expected code context")
	}
	if context.Provider != "github" {
		t.Fatalf("expected provider github, got %q", context.Provider)
	}
	if context.Branch != "feat/task-123-branch-pr-metadata" {
		t.Fatalf("expected branch metadata, got %q", context.Branch)
	}
	if context.Repo != "PerpetualSoftware/pad" {
		t.Fatalf("expected repo PerpetualSoftware/pad, got %q", context.Repo)
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

func TestExtractItemConventionMetadataFromStructuredFields(t *testing.T) {
	metadata := ExtractItemConventionMetadata(`{"status":"active","convention":{"category":"build","trigger":"on-pr-create","surfaces":["backend","docs"],"enforcement":"must","commands":["go test ./...","make install"]}}`)
	if metadata == nil {
		t.Fatal("expected convention metadata")
	}
	if metadata.Category != "build" {
		t.Fatalf("expected category build, got %q", metadata.Category)
	}
	if metadata.Trigger != "on-pr-create" {
		t.Fatalf("expected trigger on-pr-create, got %q", metadata.Trigger)
	}
	if metadata.Enforcement != "must" {
		t.Fatalf("expected enforcement must, got %q", metadata.Enforcement)
	}
	if len(metadata.Surfaces) != 2 || metadata.Surfaces[0] != "backend" || metadata.Surfaces[1] != "docs" {
		t.Fatalf("expected surfaces backend/docs, got %#v", metadata.Surfaces)
	}
	if len(metadata.Commands) != 2 || metadata.Commands[0] != "go test ./..." {
		t.Fatalf("expected commands to be preserved, got %#v", metadata.Commands)
	}
}

func TestExtractItemConventionMetadataFallsBackToLegacyFields(t *testing.T) {
	metadata := ExtractItemConventionMetadata(`{"status":"active","category":"quality","trigger":"on-commit","scope":"all","priority":"should"}`)
	if metadata == nil {
		t.Fatal("expected convention metadata")
	}
	if metadata.Category != "quality" || metadata.Trigger != "on-commit" || metadata.Enforcement != "should" {
		t.Fatalf("unexpected convention metadata %#v", metadata)
	}
	if len(metadata.Surfaces) != 1 || metadata.Surfaces[0] != "all" {
		t.Fatalf("expected legacy scope to map to surfaces, got %#v", metadata.Surfaces)
	}
}

// TestExtractItemConventionMetadata_NoLeakOnNonConventionItems is the
// regression test for BUG-987 bug 13. Previously every Task / Idea /
// Plan with a `priority` field got a phantom
// `convention.enforcement: <priority>` surfaced on its response,
// because the legacy fallback in ExtractItemConventionMetadata
// unconditionally treated `priority` as the Convention enforcement
// tier. Tasks have priority but aren't Conventions; the metadata
// must NOT be synthesized for them.
func TestExtractItemConventionMetadata_NoLeakOnNonConventionItems(t *testing.T) {
	cases := []struct {
		name   string
		fields string
	}{
		{"task with priority", `{"status":"open","priority":"high"}`},
		{"task with priority and category", `{"status":"open","priority":"high","category":"frontend"}`},
		{"idea with priority", `{"status":"new","priority":"medium","impact":"high"}`},
		{"plan with start_date and priority", `{"status":"active","priority":"high","start_date":"2026-01-01"}`},
		{"category alone is not a Convention signal", `{"category":"agent-integration","status":"new"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractItemConventionMetadata(tc.fields)
			if got != nil {
				t.Errorf("expected nil metadata for non-Convention item; got %+v", got)
			}
		})
	}
}

// TestExtractItemConventionMetadata_ConventionWithLegacyPriority
// exercises the path where priority→enforcement legacy fallback IS
// expected to fire — items that carry Convention-specific markers
// (trigger, scope, etc.) but use the legacy `priority` field for
// enforcement. The bug 13 fix preserves this path.
func TestExtractItemConventionMetadata_ConventionWithLegacyPriority(t *testing.T) {
	got := ExtractItemConventionMetadata(`{"status":"active","trigger":"on-commit","scope":"all","priority":"must"}`)
	if got == nil {
		t.Fatal("expected metadata for Convention with legacy priority field")
	}
	if got.Enforcement != "must" {
		t.Errorf("Enforcement = %q, want must (priority legacy fallback)", got.Enforcement)
	}
	if got.Trigger != "on-commit" {
		t.Errorf("Trigger = %q, want on-commit", got.Trigger)
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

func TestApplyItemConventionMetadataPreservesStatusAndWritesAliases(t *testing.T) {
	fields, err := ApplyItemConventionMetadata(`{"status":"active"}`, &ItemConventionMetadata{
		Category:    "pm",
		Trigger:     "on-task-start",
		Surfaces:    []string{"all"},
		Enforcement: "must",
		Commands:    []string{"pad item update <ref> --status in-progress"},
	})
	if err != nil {
		t.Fatalf("ApplyItemConventionMetadata error: %v", err)
	}

	metadata := ExtractItemConventionMetadata(fields)
	if metadata == nil || metadata.Category != "pm" || metadata.Trigger != "on-task-start" {
		t.Fatalf("expected structured convention metadata, got %#v", metadata)
	}

	if got := ExtractItemImplementationNotes(fields); got != nil {
		t.Fatalf("did not expect implementation notes, got %#v", got)
	}
}
