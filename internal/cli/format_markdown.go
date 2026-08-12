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

// SanitizeMarkdownText makes an arbitrary string safe to interpolate into
// markdown OUTSIDE a table cell — a heading, say. ANSI escapes are stripped and
// newlines collapse to spaces, so a value carrying a line break can't inject
// document structure. Pipes are left alone: they're only special inside a table.
func SanitizeMarkdownText(s string) string {
	s = stripANSI(s)
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
	return strings.TrimSpace(s)
}

// escapeMarkdownCell makes an arbitrary string safe INSIDE a table cell: it
// sanitizes as above, then escapes backslashes and pipes.
//
// Order matters. Escaping the pipe alone is not enough: a title containing a
// backslash immediately before a pipe ("…\|…") would become "…\\|…", which GFM
// reads as an escaped backslash followed by a LIVE pipe, so the row still gains
// a column. Escaping backslashes FIRST turns it into "…\\\|…" — an escaped
// backslash then an escaped pipe — which renders back to the literal text.
// Doing it the other way round would double-escape the backslashes we just
// introduced. (Caught by @xarmian reviewing PR #1070.)
func escapeMarkdownCell(s string) string {
	s = SanitizeMarkdownText(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
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

// RenderMarkdownTable writes headers, a separator, and one row per entry as a
// GitHub-flavored markdown table. Every cell is escaped, so callers pass raw
// values and never pre-format. Rows shorter than the header are padded with
// empty cells and longer ones are truncated, so a ragged row can't shift the
// column count and corrupt the table.
//
// This is the shared spine for the list commands' markdown output; a command
// only has to name its columns and map its rows.
func RenderMarkdownTable(w io.Writer, headers []string, rows [][]string) {
	if len(headers) == 0 {
		return
	}

	escaped := make([]string, len(headers))
	for i, h := range headers {
		escaped[i] = escapeMarkdownCell(h)
	}
	writeMarkdownRow(w, escaped...)
	writeMarkdownSeparator(w, len(headers))

	for _, row := range rows {
		cells := make([]string, len(headers))
		for i := range cells {
			if i < len(row) {
				cells[i] = escapeMarkdownCell(row[i])
			}
		}
		writeMarkdownRow(w, cells...)
	}
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
