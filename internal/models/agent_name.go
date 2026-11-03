package models

import (
	"strings"
	"unicode"
)

// NormalizeAgentName is the one stored form of a self-declared agent name,
// shared by write attribution, presence (TASK-4040), chat key validation
// (TASK-4039) and the client-side chat target guard (TASK-4186 PR C):
// trimmed, control characters removed, at most MaxAgentNameRunes runes. It
// does not fold case.
func NormalizeAgentName(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, c := range strings.TrimSpace(raw) {
		if unicode.IsControl(c) {
			continue
		}
		if n == MaxAgentNameRunes {
			break
		}
		b.WriteRune(c)
		n++
	}
	return strings.TrimSpace(b.String())
}
