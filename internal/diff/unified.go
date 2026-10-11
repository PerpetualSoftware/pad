package diff

import (
	"fmt"
	"strings"

	difflib "github.com/sergi/go-diff/diffmatchpatch"
)

// Unified renders a line-based unified diff of oldText → newText with the
// given lines of context (PLAN-3578: what a patch write hands back so the
// caller can verify exactly what changed). Empty when the texts are equal.
// Headers are fixed ("--- before" / "+++ after"); hunk ranges follow the
// unified format (1-based start, count; a zero-count range starts at the
// line before it).
func Unified(oldText, newText string, context int) string {
	if oldText == newText {
		return ""
	}
	a, b, lines := dmp.DiffLinesToChars(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)

	type line struct {
		op   difflib.Operation
		text string
	}
	var all []line
	for _, d := range diffs {
		for _, l := range splitKeepEnd(d.Text) {
			all = append(all, line{d.Type, l})
		}
	}

	var out strings.Builder
	out.WriteString("--- before\n+++ after\n")
	oldNo, newNo := 1, 1
	for i := 0; i < len(all); {
		if all[i].op == difflib.DiffEqual {
			oldNo++
			newNo++
			i++
			continue
		}
		// A change starts at i: back up over up to `context` equal lines.
		start := i
		for k := 0; k < context && start > 0 && all[start-1].op == difflib.DiffEqual; k++ {
			start--
		}
		hOld, hNew := oldNo-(i-start), newNo-(i-start)
		// Extend through changes separated by at most 2*context equal lines.
		end := i
		for end < len(all) {
			if all[end].op != difflib.DiffEqual {
				end++
				continue
			}
			run := end
			for run < len(all) && all[run].op == difflib.DiffEqual {
				run++
			}
			if run < len(all) && run-end <= 2*context {
				end = run
				continue
			}
			end = min(end+context, run)
			break
		}
		var body strings.Builder
		oc, nc := 0, 0
		for _, l := range all[start:end] {
			switch l.op {
			case difflib.DiffEqual:
				body.WriteString(" " + l.text)
				oc++
				nc++
			case difflib.DiffDelete:
				body.WriteString("-" + l.text)
				oc++
			case difflib.DiffInsert:
				body.WriteString("+" + l.text)
				nc++
			}
			if !strings.HasSuffix(l.text, "\n") {
				body.WriteString("\n\\ No newline at end of file\n")
			}
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(hOld, oc), hunkRange(hNew, nc))
		out.WriteString(body.String())
		// Advance the counters over the lines of this hunk that lie past i.
		for _, l := range all[i:end] {
			if l.op != difflib.DiffInsert {
				oldNo++
			}
			if l.op != difflib.DiffDelete {
				newNo++
			}
		}
		i = end
	}
	return out.String()
}

func hunkRange(start, count int) string {
	if count == 0 {
		return fmt.Sprintf("%d,0", start-1)
	}
	if count == 1 {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d,%d", start, count)
}

// splitKeepEnd splits s into lines, each keeping its "\n"; a last line
// without one is kept as is.
func splitKeepEnd(s string) []string {
	var out []string
	for s != "" {
		k := strings.IndexByte(s, '\n')
		if k < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:k+1])
		s = s[k+1:]
	}
	return out
}
