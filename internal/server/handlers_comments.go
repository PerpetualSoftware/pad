package server

import (
	"database/sql"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
)

// handleListComments returns all comments for an item.
func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.GetItemBySlug(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	comments, err := s.store.ListComments(item.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if comments == nil {
		comments = []models.Comment{}
	}

	writeJSON(w, http.StatusOK, comments)
}

// handleCreateComment adds a new comment to an item.
func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.GetItemBySlug(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	var input models.CommentCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Body == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "body is required")
		return
	}

	comment, err := s.store.CreateComment(workspaceID, item.ID, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Log activity
	s.logActivity(workspaceID, item.ID, "commented", comment.CreatedBy, comment.Source)

	// Publish SSE event
	s.publishCommentEvent(events.CommentCreated, workspaceID, item.ID, comment.ID, item.Title, item.CollectionSlug, comment.CreatedBy, comment.Source)

	writeJSON(w, http.StatusCreated, comment)
}

// handleDeleteComment removes a comment.
func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	_, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	commentID := chi.URLParam(r, "commentID")
	if err := s.store.DeleteComment(commentID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Comment not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// publishCommentEvent publishes a real-time event for comment changes.
func (s *Server) publishCommentEvent(eventType, workspaceID, itemID, commentID, title, collection, actor, source string) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: workspaceID,
		ItemID:      itemID,
		Collection:  collection,
		Title:       title,
		Actor:       actor,
		Source:      source,
	})
}
