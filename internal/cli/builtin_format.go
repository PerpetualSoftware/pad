package cli

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// FormatLineDiff renders old -> new as lines prefixed "-", "+" or " ",
// keeping at most context unchanged lines around each change and eliding the
// rest with "…" (TASK-3462 U3c). Empty when the two are equal.
func FormatLineDiff(oldText, newText string, context int) string {
	if oldText == newText {
		return ""
	}
	dmp := diffmatchpatch.New()
	a, b, lines := dmp.DiffLinesToChars(oldText, newText)
	diffs := dmp.DiffCharsToLines(dmp.DiffMain(a, b, false), lines)

	type line struct {
		op   diffmatchpatch.Operation
		text string
	}
	var all []line
	for _, d := range diffs {
		text := strings.TrimSuffix(d.Text, "\n")
		for _, l := range strings.Split(text, "\n") {
			all = append(all, line{d.Type, l})
		}
	}
	keep := make([]bool, len(all))
	for i, l := range all {
		if l.op == diffmatchpatch.DiffEqual {
			continue
		}
		for j := i - context; j <= i+context; j++ {
			if j >= 0 && j < len(all) {
				keep[j] = true
			}
		}
	}
	var out strings.Builder
	elided := false
	for i, l := range all {
		if !keep[i] {
			if !elided {
				out.WriteString("  …\n")
				elided = true
			}
			continue
		}
		elided = false
		switch l.op {
		case diffmatchpatch.DiffInsert:
			out.WriteString("+")
		case diffmatchpatch.DiffDelete:
			out.WriteString("-")
		default:
			out.WriteString(" ")
		}
		out.WriteString(l.text)
		out.WriteString("\n")
	}
	return out.String()
}

// BuiltinFieldChange is one setting an accepted update would replace.
type BuiltinFieldChange struct {
	Key     string
	Current any
	Library any
	HasCur  bool
	HasLib  bool
}

// BuiltinFieldChanges lists the settings taking the library's text would
// replace: every key the library writes whose value differs from the item's,
// and every key the seed had that the library dropped (the update removes
// those). status is never touched by an update, so it is never listed. Values
// compare as canonical JSON, so number spelling and key order do not count.
func BuiltinFieldChanges(current, library, seed map[string]any) []BuiltinFieldChange {
	keys := map[string]bool{}
	for k := range library {
		keys[k] = true
	}
	for k := range seed {
		if _, kept := library[k]; !kept {
			keys[k] = true
		}
	}
	delete(keys, "status")
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	canon := func(v any) string {
		b, err := json.Marshal(models.CanonicalJSONNumbers(v))
		if err != nil {
			return ""
		}
		return string(b)
	}
	var out []BuiltinFieldChange
	for _, k := range sorted {
		cur, hasCur := current[k]
		lib, hasLib := library[k]
		if !hasCur && !hasLib {
			continue
		}
		if hasCur && hasLib && canon(cur) == canon(lib) {
			continue
		}
		out = append(out, BuiltinFieldChange{Key: k, Current: cur, Library: lib, HasCur: hasCur, HasLib: hasLib})
	}
	return out
}

// ShowFieldValue renders a setting's value for a terminal: a string as
// itself, anything else as JSON.
func ShowFieldValue(v any, present bool) string {
	if !present {
		return "(none)"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "?"
	}
	return string(b)
}
