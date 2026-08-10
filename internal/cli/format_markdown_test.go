package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/fatih/color"
)

// forceColor forces ANSI ON, so a renderer that leaks colorized helpers into
// markdown output fails loudly instead of passing on a non-TTY test runner.
func forceColor(t *testing.T) {
	t.Helper()
	prev := color.NoColor
	color.NoColor = false
	t.Cleanup(func() { color.NoColor = prev })
}

func num(n int) *int { return &n }

func TestRenderItemMarkdown_Empty(t *testing.T) {
	var buf bytes.Buffer
	RenderItemMarkdown(&buf, nil)
	if got := buf.String(); got != "No items found.\n" {
		t.Errorf("empty render = %q, want %q", got, "No items found.\n")
	}
}

func TestRenderItemMarkdown_TableShape(t *testing.T) {
	items := []models.Item{{
		CollectionPrefix: "TASK",
		ItemNumber:       num(5),
		Title:            "Wire up the widget",
		Fields:           `{"status":"in-progress","priority":"high"}`,
		CollectionName:   "Tasks",
		CollectionIcon:   "✓",
		UpdatedAt:        time.Now(),
	}}

	var buf bytes.Buffer
	RenderItemMarkdown(&buf, items)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")

	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header, separator, one row):\n%s", len(lines), buf.String())
	}
	if want := "| Ref | Title | Status | Priority | Collection | Updated |"; lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if want := "| --- | --- | --- | --- | --- | --- |"; lines[1] != want {
		t.Errorf("separator = %q, want %q", lines[1], want)
	}
	for _, want := range []string{"TASK-5", "Wire up the widget", "in-progress", "high", "Tasks"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("row %q missing %q", lines[2], want)
		}
	}
	if n := strings.Count(lines[2], "|"); n != 7 {
		t.Errorf("row has %d pipes, want 7 (6 cells): %q", n, lines[2])
	}
}

func TestRenderItemMarkdown_EscapesPipesInTitle(t *testing.T) {
	items := []models.Item{{
		CollectionPrefix: "BUG",
		ItemNumber:       num(9),
		Title:            "crash in a | b parser",
		Fields:           `{}`,
		CollectionName:   "Bugs",
		UpdatedAt:        time.Now(),
	}}

	var buf bytes.Buffer
	RenderItemMarkdown(&buf, items)
	row := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[2]

	if !strings.Contains(row, `a \| b`) {
		t.Errorf("row did not escape the pipe in the title: %q", row)
	}
	// An unescaped pipe would add a seventh cell and break the table.
	if n := strings.Count(row, "|"); n != 8 {
		t.Errorf("row has %d pipes, want 8 (6 cells + 1 escaped): %q", n, row)
	}
}

// A title that already contains a backslash immediately before a pipe is the
// case escaping the pipe alone can't survive: "\" + "|" becomes "\" + "\|",
// which GFM reads as an escaped backslash followed by a LIVE pipe, so the row
// still gains a column. The backslash has to be escaped first.
// Reported by @xarmian in review of PR #1070.
func TestRenderItemMarkdown_EscapesBackslashBeforePipe(t *testing.T) {
	// Built by concatenation so the intent is unambiguous: a, space,
	// backslash, pipe, space, b.
	title := "a " + `\` + "|" + " b"

	items := []models.Item{{
		CollectionPrefix: "BUG",
		ItemNumber:       num(10),
		Title:            title,
		Fields:           `{}`,
		CollectionName:   "Bugs",
		UpdatedAt:        time.Now(),
	}}

	var buf bytes.Buffer
	RenderItemMarkdown(&buf, items)
	row := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[2]

	// Want: escaped backslash (\\) then escaped pipe (\|) — three backslashes
	// then a pipe. GFM renders that back to the literal "a \| b".
	want := "a " + `\\\|` + " b"
	if !strings.Contains(row, want) {
		t.Errorf("backslash not escaped before pipe.\n got: %q\nwant substring: %q", row, want)
	}

	// Structural check: exactly 6 cells, so 7 delimiting pipes. Any live pipe
	// from the title would push this higher.
	delimiters := strings.Count(row, "|") - strings.Count(row, `\|`)
	if delimiters != 7 {
		t.Errorf("row has %d delimiting pipes, want 7 (6 cells): %q", delimiters, row)
	}
}

func TestRenderItemMarkdown_NoANSIEscapes(t *testing.T) {
	forceColor(t)

	items := []models.Item{{
		CollectionPrefix: "TASK",
		ItemNumber:       num(1),
		Title:            "colorful",
		Fields:           `{"status":"done","priority":"critical"}`,
		CollectionName:   "Tasks",
		UpdatedAt:        time.Now(),
		Pinned:           true,
	}}

	var buf bytes.Buffer
	RenderItemMarkdown(&buf, items)

	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("markdown output contains ANSI escapes: %q", buf.String())
	}
}

func TestRenderItemMarkdown_MissingFieldsRenderDash(t *testing.T) {
	items := []models.Item{{
		CollectionPrefix: "IDEA",
		ItemNumber:       num(2),
		Title:            "no status or priority",
		Fields:           `{}`,
		CollectionName:   "Ideas",
		UpdatedAt:        time.Now(),
	}}

	var buf bytes.Buffer
	RenderItemMarkdown(&buf, items)
	row := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[2]

	if strings.Count(row, "—") != 2 {
		t.Errorf("want an em dash for both empty status and priority, got %q", row)
	}
}

func TestRenderItemMarkdown_MarksArchived(t *testing.T) {
	deleted := time.Now()
	items := []models.Item{{
		CollectionPrefix: "TASK",
		ItemNumber:       num(3),
		Title:            "gone",
		Fields:           `{}`,
		CollectionName:   "Tasks",
		UpdatedAt:        time.Now(),
		DeletedAt:        &deleted,
	}}

	var buf bytes.Buffer
	RenderItemMarkdown(&buf, items)
	row := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")[2]

	if !strings.Contains(row, "(archived)") {
		t.Errorf("archived item not marked: %q", row)
	}
}

func TestRenderCollectionMarkdown_Empty(t *testing.T) {
	var buf bytes.Buffer
	RenderCollectionMarkdown(&buf, nil)
	if got := buf.String(); got != "No collections found.\n" {
		t.Errorf("empty render = %q, want %q", got, "No collections found.\n")
	}
}

func TestRenderCollectionMarkdown_TableShape(t *testing.T) {
	colls := []models.Collection{{
		Name:      "Tasks",
		Slug:      "tasks",
		Icon:      "✓",
		ItemCount: 12,
		IsDefault: true,
	}, {
		Name:      "Ideas",
		Slug:      "ideas",
		ItemCount: 3,
	}}

	var buf bytes.Buffer
	RenderCollectionMarkdown(&buf, colls)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")

	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4 (header, separator, two rows):\n%s", len(lines), buf.String())
	}
	if want := "| Name | Slug | Items | Default |"; lines[0] != want {
		t.Errorf("header = %q, want %q", lines[0], want)
	}
	if want := "| --- | --- | --- | --- |"; lines[1] != want {
		t.Errorf("separator = %q, want %q", lines[1], want)
	}
	if !strings.Contains(lines[2], "tasks") || !strings.Contains(lines[2], "12") {
		t.Errorf("first row missing slug/count: %q", lines[2])
	}
	if !strings.Contains(lines[2], "yes") {
		t.Errorf("default collection not marked: %q", lines[2])
	}
	if strings.Contains(lines[3], "yes") {
		t.Errorf("non-default collection wrongly marked: %q", lines[3])
	}
}
