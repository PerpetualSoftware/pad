package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xarmian/pad/internal/models"
)

// StatusIcon returns a status indicator.
func StatusIcon(status string) string {
	switch status {
	case "active":
		return "● Active"
	case "draft":
		return "○ Draft"
	case "completed":
		return "✓ Done"
	case "archived":
		return "⊘ Archived"
	case "open":
		return "● Open"
	case "in_progress":
		return "◐ In Progress"
	case "done":
		return "✓ Done"
	default:
		return status
	}
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

// PrintItemTable prints items in a formatted table.
func PrintItemTable(items []models.Item) {
	if len(items) == 0 {
		fmt.Println("No items found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "TITLE\tCOLLECTION\tUPDATED\tBY\n")
	for _, item := range items {
		pin := ""
		if item.Pinned {
			pin = "* "
		}
		collLabel := item.CollectionName
		if item.CollectionIcon != "" {
			collLabel = item.CollectionIcon + " " + collLabel
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\n",
			pin, item.Title,
			collLabel,
			RelativeTime(item.UpdatedAt),
			item.LastModifiedBy,
		)
	}
	w.Flush()
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
		parts = append(parts, fmt.Sprintf("%s: %s", k, str))
	}

	if len(parts) == 0 {
		return ""
	}

	return strings.Join(parts, " | ")
}

// PrintJSON prints any value as formatted JSON.
func PrintJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// PrintItemMeta prints item metadata header.
func PrintItemMeta(item *models.Item) {
	fmt.Printf("Title:      %s\n", item.Title)
	fmt.Printf("Slug:       %s\n", item.Slug)
	if item.CollectionName != "" {
		collLabel := item.CollectionName
		if item.CollectionIcon != "" {
			collLabel = item.CollectionIcon + " " + collLabel
		}
		fmt.Printf("Collection: %s\n", collLabel)
	}
	tags := item.Tags
	if tags == "[]" || tags == "" || tags == "null" {
		tags = "(none)"
	}
	fmt.Printf("Tags:       %s\n", tags)
	if item.Pinned {
		fmt.Printf("Pinned:     yes\n")
	}
	fmt.Printf("Updated:    %s by %s via %s\n", RelativeTime(item.UpdatedAt), item.LastModifiedBy, item.Source)
	fmt.Println("---")
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

func stripHTMLTags(s string) string {
	var result strings.Builder
	inTag := false
	for _, r := range s {
		if r == '<' {
			inTag = true
			continue
		}
		if r == '>' {
			inTag = false
			continue
		}
		if !inTag {
			result.WriteRune(r)
		}
	}
	return result.String()
}
