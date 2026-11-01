package server

import (
	"net/http"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// agentNameFromRequest is the X-Pad-Agent header as stored for the caller's
// writes and presence. This is a self-description, never a verified identity.
func agentNameFromRequest(r *http.Request) string {
	return normalizeAgentName(r.Header.Get("X-Pad-Agent"))
}

// normalizeAgentName delegates to models.NormalizeAgentName, the single source
// of the stored form of a self-declared agent name (TASK-4186 PR C moved it so
// the CLI and MCP chat guard compares exactly as the server does).
func normalizeAgentName(raw string) string {
	return models.NormalizeAgentName(raw)
}
