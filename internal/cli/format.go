package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/fatih/color"
	"golang.org/x/term"
)

// Color definitions for reuse across the CLI.
var (
	Bold     = color.New(color.Bold)
	Dim      = color.New(color.Faint)
	BoldCyan = color.New(color.Bold, color.FgCyan)
)

// StatusColor returns a *color.Color appropriate for the given status string.
func StatusColor(status string) *color.Color {
	s := strings.ToLower(strings.ReplaceAll(status, "_", "-"))
	switch s {
	case "done", "completed", "fixed", "implemented", "resolved", "accepted":
		return color.New(color.FgGreen)
	case "in-progress", "in_progress", "exploring", "fixing", "building",
		"researching", "planning", "triaged", "in-sprint", "in_sprint", "paused":
		return color.New(color.FgYellow)
	case "open", "new", "draft", "todo", "planned", "proposed", "raw", "ready":
		return color.New(color.FgBlue)
	case "cancelled", "rejected", "wontfix":
		return color.New(color.FgRed)
	case "active", "published":
		return color.New(color.FgCyan)
	case "archived", "disabled":
		return color.New(color.Faint)
	default:
		return color.New(color.Reset)
	}
}

// PriorityColor returns a *color.Color appropriate for the given priority string.
func PriorityColor(priority string) *color.Color {
	switch strings.ToLower(priority) {
	case "critical", "urgent":
		return color.New(color.FgRed, color.Bold)
	case "high":
		return color.New(color.FgYellow)
	case "medium":
		return color.New(color.FgWhite)
	case "low":
		return color.New(color.Faint)
	default:
		return color.New(color.Reset)
	}
}

// ColorizedStatus returns a status icon + status text with appropriate color.
func ColorizedStatus(status string) string {
	icon := statusIconChar(status)
	c := StatusColor(status)
	return c.Sprintf("%s %s", icon, status)
}

// statusIconChar returns just the icon character for a status (no text, no color).
func statusIconChar(status string) string {
	s := strings.ToLower(strings.ReplaceAll(status, "_", "-"))
	switch s {
	case "active", "open":
		return "●"
	case "draft", "raw", "new", "planned", "proposed", "ready":
		return "○"
	case "completed", "done", "fixed", "implemented", "resolved", "accepted":
		return "✓"
	case "archived", "disabled":
		return "⊘"
	case "in-progress", "exploring", "fixing", "building", "researching",
		"planning", "triaged", "in-sprint", "paused":
		return "◐"
	case "cancelled", "rejected", "wontfix":
		return "✗"
	default:
		return "·"
	}
}

// ItemRef returns the reference string (e.g. "TASK-5") for an item, or empty string.
func ItemRef(item models.Item) string {
	if item.CollectionPrefix != "" && item.ItemNumber != nil {
		return fmt.Sprintf("%s-%d", item.CollectionPrefix, *item.ItemNumber)
	}
	return ""
}

// StatusIcon returns a colorized status indicator.
func StatusIcon(status string) string {
	return ColorizedStatus(status)
}

// RelativeTime returns a human-readable relative time string.
func RelativeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		m := int(d.Minutes())
		if m == 1 {
			return "1 min ago"
		}
		return fmt.Sprintf("%d mins ago", m)
	case d < 24*time.Hour:
		h := int(d.Hours())
		if h == 1 {
			return "1 hour ago"
		}
		return fmt.Sprintf("%d hours ago", h)
	case d < 7*24*time.Hour:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "1 day ago"
		}
		return fmt.Sprintf("%d days ago", days)
	default:
		return t.Format("Jan 2, 2006")
	}
}

// sgrPattern matches ANSI SGR (Select Graphic Rendition) escape sequences —
// the color/reset codes fatih/color emits on a TTY. Compiled once and reused
// to measure the visible width of a colorized string. Declared at package
// scope so the compile cost is paid a single time.
var sgrPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

// displayWidth returns the visible column width of s: ANSI SGR escape
// sequences are stripped, then the remaining runes are counted. Every rune is
// approximated as one column — emoji and other wide runes therefore
// under-count. That is a pre-existing limitation of the CLI's table rendering
// (matching the old tabwriter behavior) and is intentionally out of scope; the
// alternative is a new go-runewidth dependency we don't want to add.
func displayWidth(s string) int {
	return utf8.RuneCountInString(sgrPattern.ReplaceAllString(s, ""))
}

// padCell left-aligns s in a column of the given visible width by appending
// spaces. If s is already at least that wide the string is returned unchanged
// (never panics on a negative repeat count).
func padCell(s string, width int) string {
	gap := width - displayWidth(s)
	if gap <= 0 {
		return s
	}
	return s + strings.Repeat(" ", gap)
}

// truncateTitle limits an uncolorized, SGR-free title to maxRunes visible
// columns, appending an ellipsis when it had to cut. Callers strip SGR escapes
// first (see the row-build loop), so every rune counts as exactly one column —
// matching displayWidth — and slicing by runes can never cut mid-escape.
func truncateTitle(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes == 1 {
		return "…"
	}
	return string(runes[:maxRunes-1]) + "…"
}

// itemStatusPriority pulls the "status" and "priority" string values out of an
// item's fields JSON blob. Missing keys, non-string values, or malformed JSON
// yield empty strings.
func itemStatusPriority(fieldsJSON string) (status, priority string) {
	if fieldsJSON == "" || fieldsJSON == "{}" || fieldsJSON == "null" {
		return "", ""
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return "", ""
	}
	if s, ok := fields["status"].(string); ok {
		status = s
	}
	if p, ok := fields["priority"].(string); ok {
		priority = p
	}
	return status, priority
}

// PrintItemTable prints items in a width-aware, ANSI-safe table. It probes the
// terminal width (falling back to 120 columns when stdout is piped or the size
// is unavailable) and delegates layout to renderItemTable.
func PrintItemTable(items []models.Item) {
	maxWidth := 120
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		maxWidth = w
	}
	renderItemTable(os.Stdout, items, maxWidth)
}

// renderItemTable is the testable core of PrintItemTable: it lays the item
// list out into maxWidth columns and writes to w. Columns are
// REF/TITLE · STATUS · PRIORITY · COLLECTION · UPDATED. The four short columns
// are sized to their widest cell; the flexible REF/TITLE column absorbs the
// remaining width, truncating ONLY the title (never the ref) so each row fits.
//
// tabwriter is deliberately not used here: it counts ANSI color escape bytes
// as visible width and so misaligns colorized output on a TTY. All width math
// goes through displayWidth, which strips those escapes.
func renderItemTable(w io.Writer, items []models.Item, maxWidth int) {
	if len(items) == 0 {
		fmt.Fprintln(w, "No items found.")
		return
	}

	headerColor := color.New(color.Faint)

	// Pre-render every cell so we can measure visible widths before laying
	// out. The REF/TITLE cell is assembled later (it depends on the computed
	// title budget), so we stash its raw pieces.
	type row struct {
		pin        string // rendered pin marker ("" or "* " in yellow)
		ref        string // raw ref, e.g. "TASK-5" — never truncated
		title      string // raw title — the only thing we truncate
		archived   bool
		status     string // rendered short-column cells
		priority   string
		collection string
		updated    string
	}

	rows := make([]row, 0, len(items))
	for _, item := range items {
		var r row
		if item.Pinned {
			r.pin = color.YellowString("* ")
		}
		r.ref = ItemRef(item)
		// Strip any SGR escapes a title might carry (we apply our own Bold
		// styling). This keeps displayWidth == rune count for the title, so
		// truncateTitle can slice by runes without ever cutting mid-escape or
		// mis-accounting the width budget.
		r.title = sgrPattern.ReplaceAllString(item.Title, "")
		r.archived = item.DeletedAt != nil

		status, priority := itemStatusPriority(item.Fields)
		if status != "" {
			r.status = ColorizedStatus(status)
		} else {
			r.status = Dim.Sprint("—")
		}
		if priority != "" {
			r.priority = PriorityColor(priority).Sprint(priority)
		} else {
			r.priority = Dim.Sprint("—")
		}

		collLabel := item.CollectionName
		if item.CollectionIcon != "" {
			collLabel = item.CollectionIcon + " " + collLabel
		}
		r.collection = collLabel
		r.updated = Dim.Sprint(RelativeTime(item.UpdatedAt))
		rows = append(rows, r)
	}

	// Short-column widths: max visible width over the header + every row.
	statusW := displayWidth(headerColor.Sprint("STATUS"))
	priorityW := displayWidth(headerColor.Sprint("PRIORITY"))
	collectionW := displayWidth(headerColor.Sprint("COLLECTION"))
	updatedW := displayWidth(headerColor.Sprint("UPDATED"))
	for _, r := range rows {
		statusW = max(statusW, displayWidth(r.status))
		priorityW = max(priorityW, displayWidth(r.priority))
		collectionW = max(collectionW, displayWidth(r.collection))
		updatedW = max(updatedW, displayWidth(r.updated))
	}

	const sep = 2 // spaces between columns; 4 gaps between the 5 columns.
	titleBudget := maxWidth - statusW - priorityW - collectionW - updatedW - sep*4
	// Clamp to a non-negative floor: on a genuinely narrow terminal the four
	// fixed columns alone can exceed maxWidth, driving titleBudget negative.
	// Per DR-1 the ref and the short columns are never truncated, so such a
	// terminal necessarily overflows — we degrade to "ref + as much title as
	// fits (often none)" rather than mangle the ref or slice an ANSI escape.
	// The floor just keeps the width arithmetic well-defined (padCell already
	// tolerates over-wide cells).
	if titleBudget < 0 {
		titleBudget = 0
	}
	gap := strings.Repeat(" ", sep)

	// The final column is never padded (avoids trailing whitespace); every
	// earlier cell is padded to its column width so the columns line up.
	writeRow := func(refCell, status, priority, collection, updated string) {
		fmt.Fprintln(w, strings.Join([]string{
			padCell(refCell, titleBudget),
			padCell(status, statusW),
			padCell(priority, priorityW),
			padCell(collection, collectionW),
			updated,
		}, gap))
	}

	// The flexible-column header obeys the same title budget as the data rows
	// (a no-op at normal widths where "TITLE" easily fits) so the header can't
	// overflow past the data cells in the degraded narrow-terminal regime.
	writeRow(
		headerColor.Sprint(truncateTitle("TITLE", titleBudget)),
		headerColor.Sprint("STATUS"),
		headerColor.Sprint("PRIORITY"),
		headerColor.Sprint("COLLECTION"),
		headerColor.Sprint("UPDATED"),
	)

	for _, r := range rows {
		writeRow(
			buildRefCell(r.pin, r.ref, r.title, r.archived, titleBudget),
			r.status, r.priority, r.collection, r.updated,
		)
	}
}

// buildRefCell assembles the flexible REF/TITLE cell within a visible budget.
// Only the title is truncated — the pin marker, ref, and "(archived)" suffix
// are always shown in full — so a narrow terminal degrades to ref + as much
// title as fits (possibly none) rather than ever hiding or slicing the ref.
// Truncation happens on the raw title before colorizing, so an ANSI escape is
// never cut mid-sequence.
func buildRefCell(pin, ref, title string, archived bool, budget int) string {
	// Visible width consumed by everything except the title text.
	fixed := displayWidth(pin)
	refPrefix := ""
	if ref != "" {
		refPrefix = BoldCyan.Sprint(ref) + "  "
		fixed += displayWidth(ref) + 2
	}
	archivedSuffix := ""
	if archived {
		archivedSuffix = "  " + Dim.Sprint("(archived)")
		fixed += 2 + len("(archived)")
	}

	shown := truncateTitle(title, budget-fixed)

	cell := pin + refPrefix
	if shown != "" {
		cell += Bold.Sprint(shown)
	}
	return cell + archivedSuffix
}

// PrintItemTitles prints just item titles.
func PrintItemTitles(items []models.Item) {
	for _, item := range items {
		fmt.Println(item.Title)
	}
}

// FormatFieldSummary returns a formatted summary of item fields.
// Example output: "status: open | priority: high | category: platform"
func FormatFieldSummary(fieldsJSON string) string {
	if fieldsJSON == "" || fieldsJSON == "{}" || fieldsJSON == "null" {
		return ""
	}

	var fields map[string]any
	if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
		return ""
	}

	if len(fields) == 0 {
		return ""
	}

	// Sort keys for consistent output
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		v := fields[k]
		str := fmt.Sprintf("%v", v)
		if str == "" || str == "<nil>" {
			continue
		}
		// Colorize well-known fields
		switch k {
		case "status":
			parts = append(parts, fmt.Sprintf("%s: %s", k, StatusColor(str).Sprint(str)))
		case "priority":
			parts = append(parts, fmt.Sprintf("%s: %s", k, PriorityColor(str).Sprint(str)))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s", k, str))
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " | ")
}

// PrintJSON prints any value as indented (human-readable) JSON.
func PrintJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// PrintJSONCompact prints any value as compact (no-indent) JSON — a single
// line plus trailing newline. Preferred for machine-consumed payloads where
// pretty-print whitespace is pure overhead (e.g. `pad bootstrap`, whose
// canonical consumer is the /pad agent skill). Human inspection has the
// --format markdown variant.
func PrintJSONCompact(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(v)
}

// PrintItemMeta prints item metadata header with colors.
func PrintItemMeta(item *models.Item) {
	label := color.New(color.Faint)
	// Item ref + Title
	ref := ItemRef(*item)
	if ref != "" {
		fmt.Printf("%s  %s\n", BoldCyan.Sprint(ref), Bold.Sprint(item.Title))
	} else {
		fmt.Printf("%s\n", Bold.Sprint(item.Title))
	}

	if item.DeletedAt != nil {
		fmt.Printf("%s %s\n", label.Sprint("Archived:  "),
			color.New(color.FgYellow, color.Bold).Sprintf("⚠ yes (soft-deleted %s) — restore before editing", RelativeTime(*item.DeletedAt)))
	}

	if item.CollectionName != "" {
		collLabel := item.CollectionName
		if item.CollectionIcon != "" {
			collLabel = item.CollectionIcon + " " + collLabel
		}
		fmt.Printf("%s %s\n", label.Sprint("Collection:"), collLabel)
	}
	// Parent link
	if item.ParentRef != "" {
		ref := item.ParentRef
		title := item.ParentTitle
		parentStr := ref
		if title != "" {
			parentStr = ref + " " + title
		}
		fmt.Printf("%s %s\n", label.Sprint("Parent:    "), parentStr)
	}
	// Assignment: user + role
	if item.AssignedUserName != "" || item.AgentRoleName != "" {
		assignStr := ""
		if item.AssignedUserName != "" && item.AgentRoleName != "" {
			roleLabel := item.AgentRoleName
			if item.AgentRoleIcon != "" {
				roleLabel = item.AgentRoleIcon + " " + roleLabel
			}
			assignStr = fmt.Sprintf("%s (%s)", item.AssignedUserName, roleLabel)
		} else if item.AssignedUserName != "" {
			assignStr = item.AssignedUserName
		} else {
			roleLabel := item.AgentRoleName
			if item.AgentRoleIcon != "" {
				roleLabel = item.AgentRoleIcon + " " + roleLabel
			}
			assignStr = roleLabel
		}
		fmt.Printf("%s %s\n", label.Sprint("Assigned:  "), assignStr)
	}

	tags := item.Tags
	if tags == "[]" || tags == "" || tags == "null" {
		tags = Dim.Sprint("(none)")
	}
	fmt.Printf("%s %s\n", label.Sprint("Tags:      "), tags)
	if item.Pinned {
		fmt.Printf("%s %s\n", label.Sprint("Pinned:    "), color.YellowString("★ yes"))
	}
	fmt.Printf("%s %s by %s via %s\n",
		label.Sprint("Updated:   "),
		Dim.Sprint(RelativeTime(item.UpdatedAt)),
		Dim.Sprint(item.LastModifiedBy),
		Dim.Sprint(item.Source),
	)
	fmt.Println(Dim.Sprint("───────────────────────────────────────────"))
}

// PrintCollectionTable prints collections in a formatted table.
func PrintCollectionTable(collections []models.Collection) {
	if len(collections) == 0 {
		fmt.Println("No collections found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "NAME\tSLUG\tITEMS\tDEFAULT\n")
	for _, col := range collections {
		icon := col.Icon
		if icon == "" {
			icon = " "
		}
		def := ""
		if col.IsDefault {
			def = "yes"
		}
		fmt.Fprintf(w, "%s %s\t%s\t%d\t%s\n",
			icon, col.Name,
			col.Slug,
			col.ItemCount,
			def,
		)
	}
	w.Flush()
}

// PrintLinkTable prints item links in a formatted table.
func PrintLinkTable(links []models.ItemLink) {
	if len(links) == 0 {
		fmt.Println("No links found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "TYPE\tSOURCE\tTARGET\tCREATED\n")
	for _, link := range links {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			link.LinkType,
			link.SourceTitle,
			link.TargetTitle,
			RelativeTime(link.CreatedAt),
		)
	}
	w.Flush()
}

// PrintActivityTable prints activity entries in a table.
func PrintActivityTable(activities []models.Activity) {
	if len(activities) == 0 {
		fmt.Println("No recent activity.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "ACTION\tACTOR\tSOURCE\tWHEN\n")
	for _, a := range activities {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
			a.Action,
			a.Actor,
			a.Source,
			RelativeTime(a.CreatedAt),
		)
	}
	w.Flush()
}

// PrintCommentTable prints comments in a formatted table.
func PrintCommentTable(comments []models.Comment) {
	if len(comments) == 0 {
		fmt.Println("No comments.")
		return
	}

	for i, c := range comments {
		badge := c.CreatedBy
		if c.Author != "" && c.Author != c.CreatedBy {
			badge = c.Author + " (" + c.CreatedBy + ")"
		}
		fmt.Printf("💬 %s  •  %s via %s\n", badge, RelativeTime(c.CreatedAt), c.Source)
		fmt.Println(c.Body)
		if i < len(comments)-1 {
			fmt.Println()
		}
	}
}
