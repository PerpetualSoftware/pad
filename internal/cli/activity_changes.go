package cli

import (
	"strings"
	"unicode/utf8"
)

// FormatChangesForDisplay is the Go twin of formatChangesForDisplay in
// web/src/lib/utils/activityChanges.ts (BUG-2789). It sanitises an activity
// row's `changes` metadata string for a surface where a HUMAN reads it: the
// CLI's `pad project activity` and `pad workspace audit-log`. The rules, and
// the reasoning for each, live on the TypeScript function; this file restates
// only what a port has to get right.
//
// It drops every segment that is not a change: no colon, an apparent field
// key that is empty, contains whitespace or is longer than 64 characters, a
// suppressed field (implementation_notes and decision_log, BUG-2628's ruling:
// those have their own timeline cards, and a legacy row freezes the whole
// notes array into this string), or a value with no arrow. Survivors are
// trimmed and rejoined with "; ".
//
// FIDELITY TO THE JS, which is the reference. Three places where the obvious
// Go spelling disagrees with it, each pinned by the shared fixture
// (web/src/lib/utils/activityChanges.fixture.json), which BOTH test suites
// assert:
//   - whitespace is ECMAScript's (jsIsSpace), not unicode.IsSpace: the two
//     disagree in both directions, on U+0085 and U+FEFF;
//   - trimming uses the same set, for the same reason;
//   - "longer than 64" counts UTF-16 code units, which is what JS `length`
//     counts, not bytes or runes.
//
// The wire surfaces (REST, --format json, bootstrap, MCP) return the metadata
// verbatim by design; this is a display rule only.
func FormatChangesForDisplay(changes string) string {
	if changes == "" {
		return ""
	}
	var kept []string
	for _, part := range strings.Split(changes, ";") {
		trimmed := strings.TrimFunc(part, jsIsSpace)
		colon := strings.IndexByte(trimmed, ':')
		if colon == -1 {
			continue
		}
		field := strings.TrimFunc(trimmed[:colon], jsIsSpace)
		if !looksLikeFieldKey(field) || suppressedChangeFields[field] {
			continue
		}
		if !strings.Contains(trimmed[colon+1:], "→") {
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "; ")
}

var suppressedChangeFields = map[string]bool{"implementation_notes": true, "decision_log": true}

const maxFieldKeyLength = 64

func looksLikeFieldKey(field string) bool {
	n := utf16Len(field)
	return n > 0 && n <= maxFieldKeyLength && !strings.ContainsFunc(field, jsIsSpace)
}

// utf16Len is the length JavaScript reports for s.
func utf16Len(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// jsIsSpace is ECMAScript's WhiteSpace ∪ LineTerminator: the set `\s` and
// String.prototype.trim use. It differs from unicode.IsSpace in both
// directions: it INCLUDES U+FEFF, and it EXCLUDES U+0085.
func jsIsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
