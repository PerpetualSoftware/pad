package bodypatch

import (
	"fmt"
	"strconv"
	"strings"
)

// BodyLines is a numbered slice of an item's body (PLAN-3578), for targeting
// and quoting a patch. Line numbers are 1-based over the body as stored,
// split on "\n". They are for reading only: a patch anchors on text.
type BodyLines struct {
	Ref          string     `json:"ref"`
	Seq          int64      `json:"seq"`
	ContentState string     `json:"content_state,omitempty"`
	Start        int        `json:"start"`
	End          int        `json:"end"`
	TotalLines   int        `json:"total_lines"`
	Lines        []BodyLine `json:"lines"`
}

// BodyLine is one numbered line.
type BodyLine struct {
	N    int    `json:"n"`
	Text string `json:"text"`
}

// ParseLineRange reads "A-B", "A-" (to the end) or "A". A range past the
// body's end is clipped by SliceBodyLines, not refused here.
func ParseLineRange(s string) (start, end int, err error) {
	bad := fmt.Errorf("lines wants A-B, A- or A (1-based), got %q", s)
	a, b, isRange := strings.Cut(strings.TrimSpace(s), "-")
	start, err = strconv.Atoi(a)
	if err != nil || start < 1 {
		return 0, 0, bad
	}
	switch {
	case !isRange:
		return start, start, nil
	case b == "":
		return start, 0, nil // 0: to the end
	}
	end, err = strconv.Atoi(b)
	if err != nil || end < start {
		return 0, 0, bad
	}
	return start, end, nil
}

// SliceBodyLines numbers content's lines start..end (end 0 = to the end),
// clipped to the body. A body that ends in "\n" has no empty last line.
func SliceBodyLines(content string, start, end int) (lines []BodyLine, from, to, total int) {
	all := strings.Split(strings.TrimSuffix(content, "\n"), "\n")
	if content == "" {
		all = nil
	}
	total = len(all)
	if end == 0 || end > total {
		end = total
	}
	for n := start; n <= end; n++ {
		lines = append(lines, BodyLine{N: n, Text: all[n-1]})
	}
	if len(lines) == 0 {
		lines = []BodyLine{}
	}
	return lines, start, end, total
}
