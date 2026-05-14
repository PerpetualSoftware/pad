package models

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

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

// TestExtractItemConventionMetadata_LegacyConvention_ScopeOnly is
// the regression test for Codex's PR #361 round-1 finding: a legacy
// Convention carrying only `{scope, priority}` (no trigger, no
// commands, no structured convention field) must still resolve
// priority→enforcement. Pre-fix, the fallback ran BEFORE scope had
// flipped hasConventionShape, so enforcement got silently dropped.
func TestExtractItemConventionMetadata_LegacyConvention_ScopeOnly(t *testing.T) {
	got := ExtractItemConventionMetadata(`{"status":"active","scope":"all","priority":"must"}`)
	if got == nil {
		t.Fatal("expected metadata for legacy Convention with scope+priority")
	}
	if got.Enforcement != "must" {
		t.Errorf("Enforcement = %q, want must (priority fallback after scope flips shape)",
			got.Enforcement)
	}
	if len(got.Surfaces) != 1 || got.Surfaces[0] != "all" {
		t.Errorf("Surfaces = %v, want [all]", got.Surfaces)
	}
}

// TestExtractItemConventionMetadata_LegacyConvention_CommandsOnly
// covers the equivalent path for the commands marker.
func TestExtractItemConventionMetadata_LegacyConvention_CommandsOnly(t *testing.T) {
	got := ExtractItemConventionMetadata(`{"status":"active","commands":["go test"],"priority":"should"}`)
	if got == nil {
		t.Fatal("expected metadata for legacy Convention with commands+priority")
	}
	if got.Enforcement != "should" {
		t.Errorf("Enforcement = %q, want should (priority fallback after commands flips shape)",
			got.Enforcement)
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

// TestItemUpdateUnmarshalFlexFields covers BUG-1144: PATCH /items
// must accept `fields` and `tags` as either a JSON-encoded string
// (the canonical historical shape) or the natural nested object /
// array shape any reasonable HTTP client would send.
func TestItemUpdateUnmarshalFlexFields(t *testing.T) {
	t.Run("fields as nested object", func(t *testing.T) {
		var u ItemUpdate
		if err := json.Unmarshal([]byte(`{"fields":{"reading_time":6,"status":"open"}}`), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Fields == nil {
			t.Fatal("expected u.Fields to be set")
		}
		// Re-decode the canonical string and check semantic equality.
		var got map[string]any
		if err := json.Unmarshal([]byte(*u.Fields), &got); err != nil {
			t.Fatalf("u.Fields not valid JSON: %v (was %q)", err, *u.Fields)
		}
		if got["reading_time"].(float64) != 6 || got["status"].(string) != "open" {
			t.Fatalf("round-trip lost data: %#v", got)
		}
	})

	t.Run("fields as JSON-encoded string (back-compat)", func(t *testing.T) {
		var u ItemUpdate
		if err := json.Unmarshal([]byte(`{"fields":"{\"reading_time\":6}"}`), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Fields == nil || *u.Fields != `{"reading_time":6}` {
			t.Fatalf("expected stringified fields passthrough, got %v", u.Fields)
		}
	})

	t.Run("fields null leaves pointer nil", func(t *testing.T) {
		var u ItemUpdate
		if err := json.Unmarshal([]byte(`{"fields":null}`), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Fields != nil {
			t.Fatalf("expected nil pointer for null fields, got %q", *u.Fields)
		}
	})

	t.Run("fields absent leaves pointer nil", func(t *testing.T) {
		var u ItemUpdate
		if err := json.Unmarshal([]byte(`{"title":"hi"}`), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Fields != nil {
			t.Fatal("expected u.Fields nil when key absent")
		}
		if u.Title == nil || *u.Title != "hi" {
			t.Fatal("title should still decode normally")
		}
	})

	t.Run("fields wrong shape returns domain error", func(t *testing.T) {
		var u ItemUpdate
		err := json.Unmarshal([]byte(`{"fields":42}`), &u)
		if err == nil {
			t.Fatal("expected error for non-object non-string fields")
		}
		if !errors.Is(err, ErrInvalidFieldsType) {
			t.Fatalf("expected ErrInvalidFieldsType, got %v", err)
		}
		if strings.Contains(err.Error(), "Go struct field") {
			t.Fatalf("error leaked Go internals: %q", err.Error())
		}
	})

	t.Run("fields as array is invalid (object expected)", func(t *testing.T) {
		var u ItemUpdate
		err := json.Unmarshal([]byte(`{"fields":["a","b"]}`), &u)
		if !errors.Is(err, ErrInvalidFieldsType) {
			t.Fatalf("expected ErrInvalidFieldsType for array fields, got %v", err)
		}
	})

	t.Run("tags as nested array", func(t *testing.T) {
		var u ItemUpdate
		if err := json.Unmarshal([]byte(`{"tags":["foo","bar"]}`), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Tags == nil {
			t.Fatal("expected u.Tags to be set")
		}
		var got []string
		if err := json.Unmarshal([]byte(*u.Tags), &got); err != nil {
			t.Fatalf("u.Tags not valid JSON: %v (was %q)", err, *u.Tags)
		}
		if len(got) != 2 || got[0] != "foo" || got[1] != "bar" {
			t.Fatalf("round-trip lost data: %#v", got)
		}
	})

	t.Run("tags as JSON-encoded string (back-compat)", func(t *testing.T) {
		var u ItemUpdate
		if err := json.Unmarshal([]byte(`{"tags":"[\"foo\"]"}`), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Tags == nil || *u.Tags != `["foo"]` {
			t.Fatalf("expected stringified tags passthrough, got %v", u.Tags)
		}
	})

	t.Run("tags as object is invalid (array expected)", func(t *testing.T) {
		var u ItemUpdate
		err := json.Unmarshal([]byte(`{"tags":{"x":1}}`), &u)
		if !errors.Is(err, ErrInvalidTagsType) {
			t.Fatalf("expected ErrInvalidTagsType for object tags, got %v", err)
		}
	})

	t.Run("other fields decode normally", func(t *testing.T) {
		var u ItemUpdate
		body := `{"title":"new","content":"body","pinned":true,"source":"web","fields":{"x":1}}`
		if err := json.Unmarshal([]byte(body), &u); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u.Title == nil || *u.Title != "new" {
			t.Fatal("title not decoded")
		}
		if u.Content == nil || *u.Content != "body" {
			t.Fatal("content not decoded")
		}
		if u.Pinned == nil || !*u.Pinned {
			t.Fatal("pinned not decoded")
		}
		if u.Source != "web" {
			t.Fatal("source not decoded")
		}
		if u.Fields == nil || *u.Fields != `{"x":1}` {
			t.Fatalf("fields not normalized, got %v", u.Fields)
		}
	})
}

// TestItemCreateUnmarshalFlexFields covers BUG-1432: POST /items must
// accept `fields` and `tags` as either a JSON-encoded string (the
// canonical historical shape ItemCreate stored internally) or the
// natural nested object / array shape any reasonable HTTP client —
// including the MCP HTTP dispatcher — would send.
//
// Pre-fix, ItemCreate.Tags rejected `["foo","bar"]` with "cannot
// unmarshal array into Go struct field ItemCreate.tags of type
// string" — agents over MCP saw this as a generic validation_failed
// envelope and (per BUG-1409) blamed "tags is the culprit." The Codex
// investigation called out the asymmetry with ItemUpdate (which already
// handled both shapes per BUG-1144) as the root architectural fix.
func TestItemCreateUnmarshalFlexFields(t *testing.T) {
	t.Run("tags as nested array (agent-natural shape)", func(t *testing.T) {
		var c ItemCreate
		if err := json.Unmarshal([]byte(`{"title":"X","tags":["foo","bar"]}`), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var got []string
		if err := json.Unmarshal([]byte(c.Tags), &got); err != nil {
			t.Fatalf("c.Tags not valid JSON: %v (was %q)", err, c.Tags)
		}
		if len(got) != 2 || got[0] != "foo" || got[1] != "bar" {
			t.Fatalf("round-trip lost data: %#v", got)
		}
	})

	t.Run("tags as JSON-encoded string (back-compat with CLI)", func(t *testing.T) {
		var c ItemCreate
		if err := json.Unmarshal([]byte(`{"title":"X","tags":"[\"foo\"]"}`), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Tags != `["foo"]` {
			t.Fatalf("expected stringified tags passthrough, got %q", c.Tags)
		}
	})

	t.Run("tags absent leaves empty string", func(t *testing.T) {
		var c ItemCreate
		if err := json.Unmarshal([]byte(`{"title":"X"}`), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Tags != "" {
			t.Fatalf("expected empty tags when absent, got %q", c.Tags)
		}
	})

	t.Run("tags null leaves empty string", func(t *testing.T) {
		var c ItemCreate
		if err := json.Unmarshal([]byte(`{"title":"X","tags":null}`), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Tags != "" {
			t.Fatalf("expected empty tags for null, got %q", c.Tags)
		}
	})

	t.Run("tags as object is invalid (array expected)", func(t *testing.T) {
		var c ItemCreate
		err := json.Unmarshal([]byte(`{"title":"X","tags":{"x":1}}`), &c)
		if !errors.Is(err, ErrInvalidTagsType) {
			t.Fatalf("expected ErrInvalidTagsType for object tags, got %v", err)
		}
	})

	t.Run("tags as number is invalid (array/string expected)", func(t *testing.T) {
		var c ItemCreate
		err := json.Unmarshal([]byte(`{"title":"X","tags":42}`), &c)
		if !errors.Is(err, ErrInvalidTagsType) {
			t.Fatalf("expected ErrInvalidTagsType for numeric tags, got %v", err)
		}
		if strings.Contains(err.Error(), "Go struct field") {
			t.Fatalf("error leaked Go internals: %q", err.Error())
		}
	})

	t.Run("fields as nested object (agent-natural shape)", func(t *testing.T) {
		var c ItemCreate
		if err := json.Unmarshal([]byte(`{"title":"X","fields":{"status":"open","priority":"high"}}`), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(c.Fields), &got); err != nil {
			t.Fatalf("c.Fields not valid JSON: %v (was %q)", err, c.Fields)
		}
		if got["status"].(string) != "open" || got["priority"].(string) != "high" {
			t.Fatalf("round-trip lost data: %#v", got)
		}
	})

	t.Run("fields as JSON-encoded string (back-compat)", func(t *testing.T) {
		var c ItemCreate
		if err := json.Unmarshal([]byte(`{"title":"X","fields":"{\"status\":\"open\"}"}`), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Fields != `{"status":"open"}` {
			t.Fatalf("expected stringified fields passthrough, got %q", c.Fields)
		}
	})

	t.Run("fields as array is invalid (object expected)", func(t *testing.T) {
		var c ItemCreate
		err := json.Unmarshal([]byte(`{"title":"X","fields":["a","b"]}`), &c)
		if !errors.Is(err, ErrInvalidFieldsType) {
			t.Fatalf("expected ErrInvalidFieldsType for array fields, got %v", err)
		}
	})

	t.Run("other fields decode normally", func(t *testing.T) {
		var c ItemCreate
		body := `{"title":"new","content":"body","pinned":true,"source":"web","fields":{"x":1},"tags":["a"]}`
		if err := json.Unmarshal([]byte(body), &c); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if c.Title != "new" {
			t.Fatal("title not decoded")
		}
		if c.Content != "body" {
			t.Fatal("content not decoded")
		}
		if !c.Pinned {
			t.Fatal("pinned not decoded")
		}
		if c.Source != "web" {
			t.Fatal("source not decoded")
		}
		if c.Fields != `{"x":1}` {
			t.Fatalf("fields not normalized, got %q", c.Fields)
		}
		if c.Tags != `["a"]` {
			t.Fatalf("tags not normalized, got %q", c.Tags)
		}
	})
}
