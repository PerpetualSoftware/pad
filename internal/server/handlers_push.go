package server

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/watchevents"
)

// pushRequest is the body of POST .../items/{itemSlug}/push.
type pushRequest struct {
	Message string `json:"message"`
}

// handlePushToItem publishes a self-addressed watchevents.KindPush
// notification (IDEA-2544 Phase 1) — the "push this to my agent" verb:
// an explicit, user-authored instruction bound to an item, delivered to
// every one of the pushing user's OWN connected monitor sessions via
// GET /api/v1/events/stream. Unlike watch/assignment notifications,
// this has no durable backing (Dave's product call: fire-and-forget is
// acceptable for v1 — no inbox, no "no session connected" warning; the
// bus's replay buffer is the only resilience a push gets).
//
// Self-addressed only: pushing into someone else's session is a consent
// question, not a code question (IDEA-2544 plan), so TargetUserID is
// always set to the CALLER's own ID, never a request-supplied target —
// there is no cross-user push in Phase 1.
//
// POST /api/v1/workspaces/{slug}/items/{itemSlug}/push
func (s *Server) handlePushToItem(w http.ResponseWriter, r *http.Request) {
	if s.watchEvents == nil {
		// Unlike handleCreateWatch (a durable store write that works fine
		// without the bus), a push has NO persistence to fall back on —
		// if there's no bus, the message is unrecoverably lost. Fail
		// loudly here rather than returning 200 for an instruction that
		// silently went nowhere.
		writeError(w, http.StatusServiceUnavailable, "unavailable", "Push is not available")
		return
	}

	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		s.writeItemResolveError(w, r, workspaceID, itemSlug)
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}

	userID := currentUserID(r)
	if userID == "" {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}

	var input pushRequest
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	// Collapse newlines to spaces, matching truncateForSummary's
	// rationale in handlers_comments.go: Notification.Summary is a
	// single-line wire contract (`pad watch --stream --for-session`
	// prints exactly one stdout line per event). Trimmed BEFORE the
	// empty check so a whitespace-only -m ("   ") is rejected the same
	// as a genuinely empty one, rather than publishing a blank
	// instruction.
	message := strings.Join(strings.Fields(input.Message), " ")
	if message == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "message must not be empty")
		return
	}

	actor, _ := actorFromRequest(r)
	actorName := actorNameFromRequest(r)

	s.watchEvents.Publish(watchevents.Notification{
		WorkspaceID:  workspaceID,
		ItemID:       item.ID,
		CollectionID: item.CollectionID,
		ItemRef:      item.Ref,
		Kind:         watchevents.KindPush,
		Actor:        actor,
		ActorName:    actorName,
		Summary:      message,
		TargetUserID: userID,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ref":     item.Ref,
		"pushed":  true,
		"message": message,
	})
}
