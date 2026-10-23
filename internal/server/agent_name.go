package server

import (
	"net/http"
	"strings"
	"unicode"
)

// maxAgentNameRunes bounds the self-declared agent name (BUG-3372).
const maxAgentNameRunes = 64

// agentNameFromRequest is the X-Pad-Agent header as stored for the caller's
// writes and presence. This is a self-description, never a verified identity.
func agentNameFromRequest(r *http.Request) string {
	return sanitizeAgentName(r.Header.Get("X-Pad-Agent"))
}

// sanitizeAgentName applies the same hygiene to write attribution and presence:
// trimmed, control characters removed, at most maxAgentNameRunes runes.
func sanitizeAgentName(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, c := range strings.TrimSpace(raw) {
		if unicode.IsControl(c) {
			continue
		}
		if n == maxAgentNameRunes {
			break
		}
		b.WriteRune(c)
		n++
	}
	return strings.TrimSpace(b.String())
}
