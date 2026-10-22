package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// notificationExcerptRunes is the longest excerpt GET /me/notifications/{id}
// returns.
const notificationExcerptRunes = 140

// writeNotificationNotFound is the ONLY way the HANDLER of GET
// /me/notifications/{id} says no. A missing id, another user's row and every
// denial of the shared notificationAccess gate (not a member, workspace or item
// gone, not visible, comment tombstoned, no longer assigned) answer these exact
// bytes, so the endpoint cannot be used to learn which of them it was. A
// disabled account (403) and an app token (401) are refused earlier, by the
// middleware, with one answer for every id; a store failure is a 500, an
// operational error and not a denial.
func writeNotificationNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "Notification not found")
}

// handleGetMyNotification answers GET /api/v1/me/notifications/{id}
// (TASK-3939 B2b): what the app shows after a content-free push, which carries
// only an id. It runs the dispatcher's own gate, so the push and its
// enrichment can never disagree about what the recipient may see. The
// recipient's notification SETTINGS are deliberately not consulted: they
// govern whether a push is SENT, never whether an already delivered one may be
// read.
func (s *Server) handleGetMyNotification(w http.ResponseWriter, r *http.Request) {
	if appContextFrom(r) != nil {
		if currentUser(r) == nil {
			writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
			return
		}
		writeNotificationNotFound(w)
		return
	}
	user := currentUser(r)
	if user == nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "Authentication required")
		return
	}
	n, err := s.store.GetNotificationForUser(user.ID, chi.URLParam(r, "id"))
	if errors.Is(err, store.ErrNotificationNotFound) {
		writeNotificationNotFound(w)
		return
	}
	if err != nil {
		writeInternalError(w, err)
		return
	}
	cache := newPushGateCache()
	item, reason, err := s.notificationAccess(cache, *n)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if reason != "" {
		writeNotificationNotFound(w)
		return
	}
	ws, err := s.store.GetWorkspaceByID(n.WorkspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if ws == nil {
		writeNotificationNotFound(w)
		return
	}
	owner := ws.OwnerUsername
	if owner == "" && ws.OwnerID != "" {
		u, err := s.store.GetUser(ws.OwnerID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if u != nil {
			owner = u.Username
		}
	}
	// A deleted actor is a nil user and an empty name; a store error is a 500
	// (TASK-3939 B2b codex r3).
	actor := ""
	if n.ActorUserID != "" {
		u, err := s.store.GetUser(n.ActorUserID)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		if u != nil {
			actor = u.Name
		}
	}
	// The gate loaded the live comment for the comment kinds; nothing is read
	// or shown from it before the gate has passed.
	excerpt := ""
	if cm := cache.comments[n.CommentID]; cm != nil && n.CommentID != "" {
		excerpt = notificationExcerpt(cm.Body)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":         n.ID,
		"kind":       n.Kind,
		"workspace":  map[string]string{"slug": ws.Slug, "name": ws.Name},
		"item":       map[string]string{"ref": item.Ref, "title": item.Title},
		"actor_name": actor,
		"excerpt":    excerpt,
		"path":       "/" + owner + "/" + ws.Slug + "/" + item.CollectionSlug + "/" + item.Slug,
	})
}

// notificationExcerpt collapses runs of whitespace and keeps the first
// notificationExcerptRunes runes.
func notificationExcerpt(body string) string {
	s := strings.Join(strings.Fields(body), " ")
	if r := []rune(s); len(r) > notificationExcerptRunes {
		return string(r[:notificationExcerptRunes])
	}
	return s
}
