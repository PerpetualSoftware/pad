package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/xarmian/pad/internal/models"
)

// Color definitions for reuse across the CLI.
var (
	Bold    = color.New(color.Bold)
	Dim     = color.New(color.Faint)
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

// PrintItemTable prints items in a formatted table.
func PrintItemTable(items []models.Item) {
	if len(items) == 0 {
		fmt.Println("No items found.")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	headerColor := color.New(color.Faint)
	fmt.Fprintf(w, "%s\t%s\t%s\t%s\n",
		headerColor.Sprint("TITLE"),
		headerColor.Sprint("COLLECTION"),
		headerColor.Sprint("UPDATED"),
		headerColor.Sprint("BY"),
	)
	for _, item := range items {
		pin := ""
		if item.Pinned {
			pin = color.YellowString("* ")
		}
		ref := ItemRef(item)
		titlePart := item.Title
		if ref != "" {
			titlePart = BoldCyan.Sprint(ref) + "  " + Bold.Sprint(item.Title)
		} else {
			titlePart = Bold.Sprint(item.Title)
		}
		collLabel := item.CollectionName
		if item.CollectionIcon != "" {
			collLabel = item.CollectionIcon + " " + collLabel
		}
		fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\n",
			pin, titlePart,
			collLabel,
			Dim.Sprint(RelativeTime(item.UpdatedAt)),
			Dim.Sprint(item.LastModifiedBy),
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

// PrintJSON prints any value as formatted JSON.
func PrintJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
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
