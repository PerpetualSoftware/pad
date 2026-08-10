package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func mdNum(n int) *int { return &n }

// `pad item list` with no collection argument groups by collection. The
// markdown form has to keep that grouping — a flat table would lose
// information the table format shows (#898).
func TestRenderItemsGroupedByCollectionMarkdown_GroupsWithHeadings(t *testing.T) {
	items := []models.Item{
		{
			CollectionSlug: "tasks", CollectionName: "Tasks", CollectionIcon: "✓",
			CollectionPrefix: "TASK", ItemNumber: mdNum(1),
			Title: "first task", Fields: `{"status":"ready"}`, UpdatedAt: time.Now(),
		},
		{
			CollectionSlug: "ideas", CollectionName: "Ideas", CollectionIcon: "💡",
			CollectionPrefix: "IDEA", ItemNumber: mdNum(7),
			Title: "an idea", Fields: `{"status":"new"}`, UpdatedAt: time.Now(),
		},
		{
			CollectionSlug: "tasks", CollectionName: "Tasks", CollectionIcon: "✓",
			CollectionPrefix: "TASK", ItemNumber: mdNum(2),
			Title: "second task", Fields: `{"status":"done"}`, UpdatedAt: time.Now(),
		},
	}

	var buf bytes.Buffer
	renderItemsGroupedByCollectionMarkdown(&buf, items)
	out := buf.String()

	if !strings.Contains(out, "## ✓ Tasks (2)") {
		t.Errorf("missing Tasks heading with count:\n%s", out)
	}
	if !strings.Contains(out, "## 💡 Ideas (1)") {
		t.Errorf("missing Ideas heading with count:\n%s", out)
	}
	// One table per group, so one header row per group.
	if n := strings.Count(out, "| Ref | Title |"); n != 2 {
		t.Errorf("got %d table headers, want 2 (one per group):\n%s", n, out)
	}
	// First-seen collection order is preserved (tasks before ideas).
	if strings.Index(out, "Tasks (2)") > strings.Index(out, "Ideas (1)") {
		t.Errorf("group order not preserved:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("markdown output contains ANSI escapes:\n%q", out)
	}
}

// The group heading interpolates the collection icon and name into "## …".
// A newline in either injects document structure, which breaks the same
// guarantee the per-cell escaping provides inside the table.
// Reported by @xarmian in review of PR #1070.
func TestRenderItemsGroupedByCollectionMarkdown_SanitizesHeading(t *testing.T) {
	items := []models.Item{{
		CollectionSlug: "tasks",
		CollectionName: "Tasks\n## Injected Heading",
		CollectionIcon: "✓",
		// A stray SGR sequence must not survive into the heading either.
		CollectionPrefix: "TASK", ItemNumber: mdNum(1),
		Title: "t", Fields: `{"status":"ready"}`, UpdatedAt: time.Now(),
	}}

	var buf bytes.Buffer
	renderItemsGroupedByCollectionMarkdown(&buf, items)
	out := buf.String()

	headings := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "## ") {
			headings++
		}
	}
	if headings != 1 {
		t.Errorf("got %d headings, want 1 — newline in the collection name injected structure:\n%s", headings, out)
	}
	if strings.Contains(out, "\n## Injected Heading") {
		t.Errorf("injected heading survived:\n%s", out)
	}
}

func TestRenderItemsGroupedByCollectionMarkdown_StripsANSIFromHeading(t *testing.T) {
	items := []models.Item{{
		CollectionSlug:   "tasks",
		CollectionName:   "\x1b[1;31mTasks\x1b[0m",
		CollectionPrefix: "TASK", ItemNumber: mdNum(1),
		Title: "t", Fields: `{}`, UpdatedAt: time.Now(),
	}}

	var buf bytes.Buffer
	renderItemsGroupedByCollectionMarkdown(&buf, items)

	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("ANSI escape survived into the heading: %q", buf.String())
	}
}

func TestRenderItemsGroupedByCollectionMarkdown_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderItemsGroupedByCollectionMarkdown(&buf, nil)
	if got := buf.String(); got != "No items found.\n" {
		t.Errorf("empty render = %q, want %q", got, "No items found.\n")
	}
}
