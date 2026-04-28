package server

import (
	"net/http"
	"strconv"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleAuditLog returns a filtered audit log. Admin-only.
// Supports filtering by action, user (user ID), workspace, days, and pagination.
func (s *Server) handleAuditLog(w http.ResponseWriter, r *http.Request) {
	user := currentUser(r)
	if user == nil || user.Role != "admin" {
		writeError(w, http.StatusForbidden, "forbidden", "Admin access required")
		return
	}

	// Accept both "user" and "actor" query params for filtering by user ID.
	actorFilter := r.URL.Query().Get("user")
	if actorFilter == "" {
		actorFilter = r.URL.Query().Get("actor")
	}

	params := models.AuditLogParams{
		Action:      r.URL.Query().Get("action"),
		Actor:       actorFilter,
		WorkspaceID: r.URL.Query().Get("workspace"),
		Days:        30, // default
	}

	if d := r.URL.Query().Get("days"); d != "" {
		if days, err := strconv.Atoi(d); err == nil && days > 0 {
			params.Days = days
		}
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if limit, err := strconv.Atoi(l); err == nil && limit > 0 {
			params.Limit = limit
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if offset, err := strconv.Atoi(o); err == nil && offset >= 0 {
			params.Offset = offset
		}
	}

	activities, err := s.store.ListAuditLog(params)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, activities)
}
