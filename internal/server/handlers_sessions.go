package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// sessionsResponse is the body of GET /api/v1/sessions.
//
// Count is redundant with len(Sessions) and is included anyway: the
// primary consumer is a UI that only needs "is anything listening?" to
// decide whether the push button is honest (PLAN-2558 S3), and making
// that the cheapest possible read keeps the common case from having to
// reason about the array at all.
type sessionsResponse struct {
	Sessions []sessionResponse `json:"sessions"`
	Count    int               `json:"count"`
}

// sessionResponse adds current activity without persisting it in session JSON.
type sessionResponse struct {
	LiveSession
	State string `json:"state"`
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

	sessions, err := s.sessionPresence.ListForUser(user.ID)
	if err != nil {
		// The SAME 503 the nil-registry branch above returns, and for the
		// same reason stated there: a server that cannot tell who is
		// listening has no degraded answer to give, and answering 200 with
		// an empty list is a lie in exactly the direction this endpoint
		// exists to prevent. The nil branch covers "never wired"; this one
		// covers "wired but unreachable right now" — a distinction only an
		// out-of-process registry can produce (codex round 1, P1 on
		// BUG-2698), and one the caller must not have to guess at.
		slog.Warn("sessions: presence read failed", "error", err, "user_id", user.ID)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Session presence is not available")
		return
	}
	workers := make([]string, 0, len(sessions))
	for _, sess := range sessions {
		workers = append(workers, sess.WorkerID)
	}
	busy, err := s.sessionPresence.BusyWorkers(user.ID, workers)
	if err != nil {
		slog.Warn("sessions: activity read failed", "error", err, "user_id", user.ID)
	}
	rows := make([]sessionResponse, 0, len(sessions))
	for _, sess := range sessions {
		state := "connected_not_accepting"
		if sess.Armed {
			state = "online"
		}
		if sess.WorkerID != "" && busy[sess.WorkerID] {
			state = "busy"
		}
		sess.WorkerID = models.ShortWorkerID(sess.WorkerID)
		rows = append(rows, sessionResponse{LiveSession: sess, State: state})
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: rows, Count: len(rows)})
}

// handleSessionActivity writes only the credential user's worker activity.
// POST /api/v1/sessions/activity
func (s *Server) handleSessionActivity(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	if s.sessionPresence == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Session presence is not available")
		return
	}
	worker := r.Header.Get("X-Pad-Worker")
	if !models.ValidWorkerID(worker) {
		writeError(w, http.StatusBadRequest, "invalid_worker_id", `X-Pad-Worker must be "wk_" followed by 32 lowercase hex characters`)
		return
	}
	var body struct {
		State string          `json:"state"`
		At    json.RawMessage `json:"at"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "Invalid activity body")
		return
	}
	if body.State != "busy" && body.State != "idle" {
		writeError(w, http.StatusBadRequest, "validation_error", "state must be busy or idle")
		return
	}
	var clientAt *time.Time
	if len(body.At) > 0 {
		var raw string
		if err := json.Unmarshal(body.At, &raw); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "at must be an RFC3339 timestamp")
			return
		}
		parsed, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "at must be an RFC3339 timestamp")
			return
		}
		clientAt = &parsed
	}
	if err := s.sessionPresence.SetActivity(user.ID, worker, body.State == "busy", time.Now().UTC(), clientAt); err != nil {
		slog.Warn("sessions: activity write failed", "error", err, "user_id", user.ID)
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Session presence is not available")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
