// Package acceptance reads the "Acceptance criteria" section of an item body
// (PLAN-3598 slice (a)). It is the one Go reading of the rule the spec
// template writes and the verify and decompose playbooks describe in prose:
// a heading named "Acceptance criteria", bullet lines of the form
// "- AC-N: text", and skeleton lines (angle-bracket instructions or a bare
// ellipsis) that are placeholders, not criteria.
package acceptance

import (
	"regexp"
	"strings"
)

// Criterion is one "AC-N: text" line.
type Criterion struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Section is what Parse found. Found is false when the body has no
// acceptance-criteria heading; a heading with no real criteria under it is
// Found with an empty Criteria.
type Section struct {
	Found bool `json:"found"`
	// Raw is the section's text between its heading and the next heading of
	// the same or a higher level, trimmed.
	Raw      string      `json:"raw"`
	Criteria []Criterion `json:"criteria"`
	// Placeholders are the IDs of AC lines still holding skeleton text.
	Placeholders []string `json:"placeholders"`
}

var (
	headingRe   = regexp.MustCompile(`^(#{1,6})[ \t]+(.*?)[ \t]*#*[ \t]*$`)
	criterionRe = regexp.MustCompile(`^[ \t]{0,3}[-*+][ \t]+(AC-[0-9]+)[ \t]*:[ \t]*(.*)$`)
	fenceRe     = regexp.MustCompile("^[ \t]{0,3}(```|~~~)")
)

// Parse returns the first acceptance-criteria section in body.
func Parse(body string) Section {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	var out Section
	level := 0
	start := -1
	end := len(lines)
	fence := ""
	for i, line := range lines {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			if fence == "" {
				fence = m[1]
			} else if m[1] == fence {
				fence = ""
			}
			continue
		}
		if fence != "" {
			continue
		}
		m := headingRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if start < 0 {
			if strings.EqualFold(strings.TrimSpace(m[2]), "acceptance criteria") {
				level = len(m[1])
				start = i + 1
			}
			continue
		}
		if len(m[1]) <= level {
			end = i
			break
		}
	}
	if start < 0 {
		return out
	}
	out.Found = true
	section := lines[start:end]
	out.Raw = strings.TrimSpace(strings.Join(section, "\n"))
	out.Criteria = []Criterion{}
	out.Placeholders = []string{}

	var cur *Criterion
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = strings.TrimSpace(cur.Text)
		if isPlaceholder(cur.Text) {
			out.Placeholders = append(out.Placeholders, cur.ID)
		} else {
			out.Criteria = append(out.Criteria, *cur)
		}
		cur = nil
	}
	fence = ""
	for _, line := range section {
		if m := fenceRe.FindStringSubmatch(line); m != nil {
			if fence == "" {
				fence = m[1]
			} else if m[1] == fence {
				fence = ""
			}
			flush()
			continue
		}
		if fence != "" {
			continue
		}
		if m := criterionRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Criterion{ID: m[1], Text: m[2]}
			continue
		}
		// An indented line continues the criterion above it; anything
		// else (a blank line, another bullet, prose) ends it.
		if cur != nil && strings.TrimSpace(line) != "" && (strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t")) {
			cur.Text += " " + strings.TrimSpace(line)
			continue
		}
		flush()
	}
	flush()
	return out
}

// isPlaceholder is the playbooks' rule: unedited skeleton text, either
// angle-bracket instructions or a bare ellipsis, is not a criterion.
func isPlaceholder(text string) bool {
	if text == "" || text == "..." || text == "…" {
		return true
	}
	return strings.HasPrefix(text, "<") && strings.HasSuffix(text, ">")
}
