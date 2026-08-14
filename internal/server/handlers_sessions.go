package server

import "net/http"

// sessionsResponse is the body of GET /api/v1/sessions.
//
// Count is redundant with len(Sessions) and is included anyway: the
// primary consumer is a UI that only needs "is anything listening?" to
// decide whether the push button is honest (PLAN-2558 S3), and making
// that the cheapest possible read keeps the common case from having to
// reason about the array at all.
type sessionsResponse struct {
	Sessions []LiveSession `json:"sessions"`
	Count    int           `json:"count"`
}

// handleListSessions returns the caller's OWN live event-stream
// connections (PLAN-2558 S1). It is the read side of the presence
// registry described in session_presence.go.
//
// SELF-SCOPED, WITH NO ADMIN VIEW. There is deliberately no
// ?user_id= parameter and no admin bypass, even though most list
// endpoints in this server have one. Who has an agent session open, and
// when, is a presence signal about a person rather than a fact about
// workspace content — the same reasoning that made `pad push`
// self-addressed only (handlers_push.go: "pushing into someone else's
// session is a consent question, not a code question"). If a
// cross-user view is ever wanted, it is a product decision with a
// consent design attached, not a query parameter.
//
// GET /api/v1/sessions
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if s.sessionPresence == nil {
		// Mirrors handleWatchEventsStream's own nil-bus stance: a server
		// built without the presence registry doesn't have a degraded
		// answer to give, and answering 200 with an empty list would be
		// a LIE in exactly the direction this feature exists to prevent
		// (the UI would report "nobody is listening" when it simply
		// cannot tell).
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Session presence is not available")
		return
	}

	user := currentUser(r)
	if user == nil {
		// Same stance as the stream this endpoint reports on: there is
		// no coherent "my sessions" for a workspace-scoped token or the
		// fresh-install no-auth window. Resolved user identity or 401.
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	// Codex round 2, P2. writeJSON sets no Cache-Control and the
	// jsonContentType middleware only sets Content-Type, so without
	// this the response is heuristically cacheable. Two reasons that's
	// wrong here, beyond matching the house pattern for per-user
	// sensitive responses (handlers_attachments.go:585):
	//
	//  1. Cross-context staleness. This body is scoped to ONE user; a
	//     shared cache serving it to another is a presence leak about a
	//     person, which is the same boundary this endpoint's missing
	//     admin view exists to hold.
	//  2. It is a liveness answer with a short shelf life (see
	//     LiveSession's staleness note). A cached "1 session connected"
	//     is precisely the confident-but-wrong answer the whole slice
	//     exists to prevent.
	w.Header().Set("Cache-Control", "private, no-store")

	sessions := s.sessionPresence.ListForUser(user.ID)
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: sessions, Count: len(sessions)})
}
