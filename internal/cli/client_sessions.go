package cli

// Client access to the caller's live event-stream sessions (PLAN-2613
// S2). GET /api/v1/sessions is self-scoped — it returns only the
// authenticated user's own connections (handlers_sessions.go) — so `pad
// session status` can report how many of THIS user's sessions the server
// currently sees, and how many of those are armed.

// LiveSession mirrors server.LiveSession's JSON shape (the wire contract
// of GET /api/v1/sessions). Only the fields `pad session status` reads
// are declared; extra fields in the response are ignored.
type LiveSession struct {
	ID          string `json:"id"`
	Label       string `json:"label,omitempty"`
	PID         int    `json:"pid,omitempty"`
	Armed       bool   `json:"armed"`
	ConnectedAt string `json:"connected_at"`
}

// SessionsResponse is the body of GET /api/v1/sessions.
type SessionsResponse struct {
	Sessions []LiveSession `json:"sessions"`
	Count    int           `json:"count"`
}

// ListSessions returns the caller's currently-connected event-stream
// sessions as the server sees them. This is authoritative for delivery
// (the server's armed bit is the sole authority — PLAN-2613 D3/S1),
// unlike the LOCAL arm-state file, which only reflects a session's
// declared intent on this machine.
func (c *Client) ListSessions() (*SessionsResponse, error) {
	var resp SessionsResponse
	if err := c.get("/sessions", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
