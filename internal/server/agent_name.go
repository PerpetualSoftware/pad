package server

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// agentNameFromRequest is the X-Pad-Agent header as stored for the caller's
// writes and presence. This is a self-description, never a verified identity.
func agentNameFromRequest(r *http.Request) string {
	return normalizeAgentName(r.Header.Get("X-Pad-Agent"))
}

// normalizeAgentName is the one stored form of a self-declared agent name,
// shared by write attribution, presence (TASK-4040) and chat key validation
// (TASK-4039 r3): trimmed, control characters removed, at most
// models.MaxAgentNameRunes runes.
func normalizeAgentName(raw string) string {
	if raw == "" {
		return ""
	}
	var b strings.Builder
	n := 0
	for _, c := range strings.TrimSpace(raw) {
		if unicode.IsControl(c) {
			continue
		}
		if n == models.MaxAgentNameRunes {
			break
		}
		b.WriteRune(c)
		n++
	}
	return strings.TrimSpace(b.String())
}
