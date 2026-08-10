package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// Markdown list rendering (#898).
//
// The `--format markdown` branch of the list commands writes a GitHub-flavored
// markdown table. Two rules separate it from the table renderer:
//
//  1. No ANSI. Markdown output is destined for a file, a PR body, or an agent's
//     context — never a terminal — so the colorized helpers (ColorizedStatus,
//     PriorityColor, Dim) are deliberately NOT reused here. Raw field values go
//     in and the reader's renderer does the styling.
//  2. Every cell is escaped. An unescaped `|` in a title silently adds a column
//     and corrupts the row; a newline ends the row early.
//
// Widths are not computed — markdown renderers lay the table out themselves, so
// the width-aware column math in renderItemTable has no counterpart here.

// escapeMarkdownCell makes an arbitrary string safe inside a table cell: ANSI
// escapes are stripped, pipes are escaped so they can't start a new column, and
// newlines collapse to spaces so they can't end the row.
func escapeMarkdownCell(s string) string {
	s = sgrPattern.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "|", `\|`)
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	return strings.TrimSpace(s)
}

// writeMarkdownRow writes one pipe-delimited row. Cells are expected to be
// pre-escaped.
func writeMarkdownRow(w io.Writer, cells ...string) {
	fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
}

// writeMarkdownSeparator writes the header/body separator for n columns.
func writeMarkdownSeparator(w io.Writer, n int) {
	cells := make([]string, n)
	for i := range cells {
		cells[i] = "---"
	}
	writeMarkdownRow(w, cells...)
}

// PrintItemMarkdown prints items as a markdown table on stdout.
func PrintItemMarkdown(items []models.Item) {
	RenderItemMarkdown(os.Stdout, items)
}

// RenderItemMarkdown is the writer-taking core of PrintItemMarkdown. Columns
// mirror the table renderer's — Ref · Title · Status · Priority · Collection ·
// Updated — so switching format changes the styling, not the information.
// Exported because cmd/pad composes it per collection group.
func RenderItemMarkdown(w io.Writer, items []models.Item) {
	if len(items) == 0 {
		fmt.Fprintln(w, "No items found.")
		return
	}

	writeMarkdownRow(w, "Ref", "Title", "Status", "Priority", "Collection", "Updated")
	writeMarkdownSeparator(w, 6)

	for _, item := range items {
		// The ref is the item's handle for every other command, so fall back to
		// the slug rather than emitting an unusable empty first cell.
		ref := ItemRef(item)
		if ref == "" {
			ref = item.Slug
		}
		if item.Pinned {
			// The table marks pins with a yellow "*"; colour is unavailable
			// here, so the marker has to be a glyph.
			ref = "📌 " + ref
		}

		title := item.Title
		if item.DeletedAt != nil {
			title += " (archived)"
		}

		status, priority := itemStatusPriority(item.Fields)
		if status == "" {
			status = "—"
		}
		if priority == "" {
			priority = "—"
		}

		collection := item.CollectionName
		if item.CollectionIcon != "" {
			collection = item.CollectionIcon + " " + collection
		}

		writeMarkdownRow(w,
			escapeMarkdownCell(ref),
			escapeMarkdownCell(title),
			escapeMarkdownCell(status),
			escapeMarkdownCell(priority),
			escapeMarkdownCell(collection),
			escapeMarkdownCell(RelativeTime(item.UpdatedAt)),
		)
	}
}

// PrintCollectionMarkdown prints collections as a markdown table on stdout.
func PrintCollectionMarkdown(collections []models.Collection) {
	RenderCollectionMarkdown(os.Stdout, collections)
}

// RenderCollectionMarkdown is the writer-taking core of
// PrintCollectionMarkdown. Columns mirror PrintCollectionTable's.
func RenderCollectionMarkdown(w io.Writer, collections []models.Collection) {
	if len(collections) == 0 {
		fmt.Fprintln(w, "No collections found.")
		return
	}

	writeMarkdownRow(w, "Name", "Slug", "Items", "Default")
	writeMarkdownSeparator(w, 4)

	for _, col := range collections {
		name := col.Name
		if col.Icon != "" {
			name = col.Icon + " " + name
		}
		def := ""
		if col.IsDefault {
			def = "yes"
		}
		writeMarkdownRow(w,
			escapeMarkdownCell(name),
			escapeMarkdownCell(col.Slug),
			strconv.Itoa(col.ItemCount),
			def,
		)
	}
}
