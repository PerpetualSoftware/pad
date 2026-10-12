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
	atxRe       = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)
	setextRe    = regexp.MustCompile(`^ {0,3}(=+|-+)[ \t]*$`)
	bulletRe    = regexp.MustCompile(`^ {0,3}[-*+](?:[ \t]|$)`)
	itemRe      = regexp.MustCompile(`^( *)[-*+]( +)(.*)$`)
	criterionRe = regexp.MustCompile(`^(AC-[0-9]+)[ \t]*:[ \t]*(.*)$`)
	fenceOpenRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")
	closeHashRe = regexp.MustCompile(`(?:^|[ \t]+)#+$`)
)

// heading is one ATX or setext heading: it starts at line `at` and its body
// starts at line `body`.
type heading struct {
	at, body, level int
	text            string
}

// scan marks the lines inside fenced code blocks (fence lines included) and
// lists the headings outside them, following CommonMark's rules for both: a
// fence closes only on a run of its own character at least as long as the
// opener with nothing after it, and a setext underline makes a heading only
// of a paragraph line, never of a list item, a blank line, or an indented
// line (which continues a list item, so the underline is a thematic break).
func scan(lines []string) (fenced []bool, heads []heading) {
	fenced = make([]bool, len(lines))
	fence := ""
	for i, line := range lines {
		if fence != "" {
			fenced[i] = true
			t := strings.TrimRight(strings.TrimLeft(line, " "), " \t")
			if len(line)-len(strings.TrimLeft(line, " ")) <= 3 && len(t) >= len(fence) && strings.Trim(t, fence[:1]) == "" {
				fence = ""
			}
			continue
		}
		if m := fenceOpenRe.FindStringSubmatch(line); m != nil && !(m[1][0] == '`' && strings.Contains(m[2], "`")) {
			fence = m[1]
			fenced[i] = true
			continue
		}
		if m := atxRe.FindStringSubmatch(line); m != nil {
			text := closeHashRe.ReplaceAllString(m[2], "")
			heads = append(heads, heading{at: i, body: i + 1, level: len(m[1]), text: strings.TrimSpace(text)})
			continue
		}
		if m := setextRe.FindStringSubmatch(line); m != nil && i > 0 && !fenced[i-1] {
			prev := lines[i-1]
			if strings.TrimSpace(prev) != "" && !atxRe.MatchString(prev) && !bulletRe.MatchString(prev) && !setextRe.MatchString(prev) && !strings.HasPrefix(prev, "  ") && !strings.HasPrefix(prev, "\t") {
				level := 2
				if m[1][0] == '=' {
					level = 1
				}
				heads = append(heads, heading{at: i - 1, body: i + 1, level: level, text: strings.TrimSpace(prev)})
			}
		}
	}
	return fenced, heads
}

// Parse returns the first acceptance-criteria section in body.
func Parse(body string) Section {
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	fenced, heads := scan(lines)
	var out Section
	start, end, level := -1, len(lines), 0
	for _, h := range heads {
		if start < 0 {
			if strings.EqualFold(h.text, "acceptance criteria") {
				start, level = h.body, h.level
			}
			continue
		}
		if h.level <= level {
			end = h.at
			break
		}
	}
	if start < 0 {
		return out
	}
	out.Found = true
	out.Raw = strings.TrimSpace(strings.Join(lines[start:end], "\n"))
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
	// itemCol is the content column of the open top-level list item, or -1.
	// A bullet indented to at least that column is nested inside the item
	// (CommonMark's rule), so an AC-shaped example under a criterion joins
	// its text; a bullet short of it, indented up to three spaces, starts a
	// new top-level item, which is a criterion when it reads "AC-N: text".
	itemCol := -1
	for i := start; i < end; i++ {
		line := strings.ReplaceAll(lines[i], "\t", "    ")
		if fenced[i] {
			flush()
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if m := itemRe.FindStringSubmatch(line); m != nil {
			if itemCol >= 0 && indent >= itemCol {
				if cur != nil {
					cur.Text += " " + strings.TrimSpace(line)
				}
				continue
			}
			if indent <= 3 {
				flush()
				gap := len(m[2])
				if gap > 4 {
					gap = 1
				}
				itemCol = indent + 1 + gap
				if c := criterionRe.FindStringSubmatch(m[3]); c != nil {
					cur = &Criterion{ID: c[1], Text: c[2]}
				}
				continue
			}
		}
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		// An indented line continues the criterion above it; an unindented
		// one is prose that ends both the criterion and the list item.
		if indent >= 2 && cur != nil {
			cur.Text += " " + strings.TrimSpace(line)
			continue
		}
		flush()
		if indent == 0 {
			itemCol = -1
		}
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
