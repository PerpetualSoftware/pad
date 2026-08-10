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

func TestRenderItemsGroupedByCollectionMarkdown_Empty(t *testing.T) {
	var buf bytes.Buffer
	renderItemsGroupedByCollectionMarkdown(&buf, nil)
	if got := buf.String(); got != "No items found.\n" {
		t.Errorf("empty render = %q, want %q", got, "No items found.\n")
	}
}
