package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/spf13/cobra"
)

// --- renderer unit tests for the two non-tabular surfaces ---

// A comment body is authored as markdown, so it must be emitted verbatim.
// Escaping it would turn its lists, links, and code fences into literal text —
// the opposite of what --format markdown is for.
func TestRenderCommentsMarkdown_BodyIsVerbatim(t *testing.T) {
	comments := []models.Comment{{
		Author:    "Dave",
		CreatedBy: "dave@example.com",
		Source:    "cli",
		CreatedAt: time.Now(),
		Body:      "- a list item\n- another\n\n`code | with a pipe`",
	}}

	var buf bytes.Buffer
	renderCommentsMarkdown(&buf, comments)
	out := buf.String()

	if !strings.Contains(out, "- a list item\n- another") {
		t.Errorf("list markup did not survive verbatim:\n%s", out)
	}
	if strings.Contains(out, `\|`) {
		t.Errorf("body was table-escaped; it should be verbatim:\n%s", out)
	}
	if !strings.Contains(out, "**Dave (dave@example.com)**") {
		t.Errorf("attribution line missing or unformatted:\n%s", out)
	}
}

// The attribution line IS constructed by us, so it gets sanitized.
func TestRenderCommentsMarkdown_SanitizesAttribution(t *testing.T) {
	comments := []models.Comment{{
		Author:    "Dave\n# Injected",
		CreatedBy: "dave@example.com",
		Source:    "\x1b[2Kcli",
		CreatedAt: time.Now(),
		Body:      "body",
	}}

	var buf bytes.Buffer
	renderCommentsMarkdown(&buf, comments)
	out := buf.String()

	if strings.Contains(out, "\n# Injected") {
		t.Errorf("newline in author injected a heading:\n%s", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("escape byte survived in attribution: %q", out)
	}
}

func TestRenderCommentsMarkdown_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderCommentsMarkdown(&buf, nil)
	if got := buf.String(); got != "No comments.\n" {
		t.Errorf("empty render = %q, want %q", got, "No comments.\n")
	}
}

func TestRenderDepsMarkdown_SectionsAndEmptyState(t *testing.T) {
	var buf bytes.Buffer
	renderDepsMarkdown(&buf, "TASK-5 the item",
		[]models.ItemLink{{TargetTitle: "blocks this"}},
		nil)
	out := buf.String()

	for _, want := range []string{
		"# Dependencies for TASK-5 the item",
		"## Blocks",
		"- blocks this",
		"## Blocked by",
		"_none_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("deps markdown missing %q:\n%s", want, out)
		}
	}
}

func TestRenderDepsMarkdown_SanitizesTitles(t *testing.T) {
	var buf bytes.Buffer
	renderDepsMarkdown(&buf, "label", []models.ItemLink{{TargetTitle: "evil\n## Injected"}}, nil)

	headings := 0
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.HasPrefix(line, "## ") {
			headings++
		}
	}
	// Exactly the two section headings we emit.
	if headings != 2 {
		t.Errorf("got %d headings, want 2 — a link title injected structure:\n%s", headings, buf.String())
	}
}

// --- end-to-end routing, one case per surface ---

// jsonHandler serves canned JSON per URL-path suffix so each command's client
// call is satisfied without knowing the full route.
func jsonHandler(t *testing.T, bySuffix map[string]any) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for suffix, payload := range bySuffix {
			if strings.HasSuffix(r.URL.Path, suffix) {
				if raw, ok := payload.(string); ok {
					_, _ = w.Write([]byte(raw))
					return
				}
				_ = json.NewEncoder(w).Encode(payload)
				return
			}
		}
		http.NotFound(w, r)
	})
}

func TestMarkdownRoutingAcrossSurfaces(t *testing.T) {
	now := time.Now()
	five := 5

	cases := []struct {
		name    string
		routes  map[string]any
		cmd     func() *cobra.Command
		args    []string
		want    []string
		notWant []string
	}{
		{
			name: "item starred",
			routes: map[string]any{"/starred": []models.Item{{
				CollectionPrefix: "TASK", ItemNumber: &five, Title: "starred one",
				CollectionName: "Tasks", Fields: `{"status":"ready"}`, UpdatedAt: now,
			}}},
			cmd:     starredCmd,
			want:    []string{"| Ref | Title | Status | Priority | Collection | Updated |", "| TASK-5 |"},
			notWant: []string{"PRIORITY"},
		},
		{
			name: "item list scoped to one collection",
			routes: map[string]any{"/items": []models.Item{{
				CollectionPrefix: "TASK", ItemNumber: &five, Title: "scoped one",
				CollectionSlug: "tasks", CollectionName: "Tasks",
				Fields: `{"status":"ready"}`, UpdatedAt: now,
			}}},
			cmd:  listCmd,
			args: []string{"tasks"},
			want: []string{"| Ref | Title | Status | Priority | Collection | Updated |", "| TASK-5 |"},
			// Scoped listing is a single table, so no grouping heading.
			notWant: []string{"## ", "PRIORITY"},
		},
		{
			name: "item comments",
			routes: map[string]any{
				"/comments": []models.Comment{{
					Author: "Dave", CreatedBy: "dave@example.com", Source: "cli",
					CreatedAt: now, Body: "the body",
				}},
				"/items/TASK-5": models.Item{Slug: "task-5"},
			},
			cmd:  commentsCmd,
			args: []string{"TASK-5"},
			want: []string{"**Dave (dave@example.com)**", "the body"},
		},
		{
			name: "project activity",
			routes: map[string]any{"/activity": []models.Activity{{
				Action: "updated", Actor: "user", ActorName: "Claude",
				ItemRef: "TASK-5", ItemTitle: "a task", CreatedAt: now,
				Metadata: `{"changes":"status: ready → done"}`,
			}}},
			cmd:  activityCmd,
			want: []string{"| When | Actor | Action | Item | Changes |", "TASK-5 a task", "status: ready → done"},
		},
		{
			name: "role list",
			routes: map[string]any{"/agent-roles": []models.AgentRole{{
				Slug: "implementer", Name: "Implementer", Icon: "🔧",
				Description: "writes code", Tools: "edit", ItemCount: 3,
			}}},
			cmd:     roleListCmd,
			want:    []string{"| Slug | Name | Description | Tools | Items |", "| implementer | 🔧 Implementer | writes code | edit | 3 |"},
			notWant: []string{"SLUG\t", "DESCRIPTION"},
		},
		{
			name: "attachment list",
			routes: map[string]any{"/attachments": `{"attachments":[` +
				`{"id":"0123456789abcdef","mime_type":"image/png","size_bytes":2048,` +
				`"filename":"shot.png","item_title":"a task","created_at":"2026-08-11T10:00:00Z"}],` +
				`"total":1,"limit":50,"offset":0}`},
			cmd:  attachmentListCmd,
			want: []string{"| ID | MIME | Size | Filename | Item | Created |", "| 01234567 | image/png |", "shot.png", "_1 of 1 (limit 50, offset 0)_"},
		},
		{
			name: "library list",
			routes: map[string]any{
				"/convention-library": `{"categories":[{"name":"git","description":"version control",` +
					`"conventions":[{"title":"Conventional commits","trigger":"on-commit",` +
					`"enforcement":"must","surfaces":["cli","web"]}]}]}`,
				"/playbook-library": `{"categories":[{"name":"agent-workflows","description":"agent flows",` +
					`"playbooks":[{"title":"Ship tasks","trigger":"manual","scope":"all",` +
					`"invocation_slug":"ship","summary":"work a list of tasks"}]}]}`,
			},
			cmd: libraryCmd,
			want: []string{
				"# Conventions", "## git", "| Title | Trigger | Enforcement | Surfaces |",
				"| Conventional commits | on-commit | must | cli, web |",
				"# Playbooks", "## agent-workflows", "| Title | Trigger | Scope | Invocation | Summary |",
				"/pad ship",
			},
			notWant: []string{"=== CONVENTIONS ===", "────"},
		},
		{
			name: "workspace members",
			routes: map[string]any{"/members": `{"members":[{"user_name":"Dave","user_email":"dave@example.com","role":"owner"}],` +
				`"invitations":[{"email":"new@example.com","role":"editor","code":"ABC123"}]}`},
			cmd:  membersCmd,
			want: []string{"| Name | Email | Role |", "| Dave | dave@example.com | owner |", "## Pending invitations", "| new@example.com | editor | ABC123 |"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupFormatRoutingTest(t, jsonHandler(t, tc.routes))
			formatFlag = "markdown"

			cmd := tc.cmd()
			cmd.SetArgs(tc.args)

			var execErr error
			out := captureStdout(t, func() { execErr = cmd.Execute() })
			if execErr != nil {
				t.Fatalf("execute: %v\noutput:\n%s", execErr, out)
			}

			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("output missing %q:\n%s", want, out)
				}
			}
			for _, notWant := range tc.notWant {
				if strings.Contains(out, notWant) {
					t.Errorf("output should not contain %q (table path leaked?):\n%s", notWant, out)
				}
			}
			if strings.ContainsRune(out, 0x1b) {
				t.Errorf("ANSI escape leaked into markdown output: %q", out)
			}
		})
	}
}
