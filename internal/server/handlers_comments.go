package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

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
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	comments, err := s.store.ListComments(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if comments == nil {
		comments = []models.Comment{}
	}

	// Bulk-load reactions for all comments.
	if len(comments) > 0 {
		commentIDs := make([]string, len(comments))
		for i, c := range comments {
			commentIDs[i] = c.ID
		}
		reactionsMap, err := s.store.ListReactionsByComments(commentIDs)
		if err == nil && reactionsMap != nil {
			for i := range comments {
				if reactions, ok := reactionsMap[comments[i].ID]; ok {
					comments[i].Reactions = reactions
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, comments)
}

// handleCreateComment adds a new comment to an item.
func (s *Server) handleCreateComment(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
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

	// Set author from authenticated user if available
	if u := currentUser(r); u != nil && input.Author == "" {
		input.Author = u.Name
	}

	// Derive actor/source from auth context
	actor, source := actorFromRequest(r)
	if input.CreatedBy == "" {
		input.CreatedBy = actor
	}
	if input.Source == "" {
		input.Source = source
	}

	comment, err := s.store.CreateComment(workspaceID, item.ID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Log activity
	s.logActivity(workspaceID, item.ID, "commented", r)

	// Publish SSE event
	s.publishCommentEvent(events.CommentCreated, workspaceID, item.ID, comment.ID, item.Title, item.CollectionSlug, actor, source)
	s.dispatchWebhook(workspaceID, "comment.created", comment)

	writeJSON(w, http.StatusCreated, comment)
}

// handleDeleteComment removes a comment.
func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	commentID := chi.URLParam(r, "commentID")

	// Verify the comment belongs to this workspace.
	comment, cerr := s.store.GetComment(commentID)
	if cerr != nil || comment == nil || comment.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	if err := s.store.DeleteComment(commentID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Comment not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleCreateReply creates a reply to an existing comment.
func (s *Server) handleCreateReply(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	commentID := chi.URLParam(r, "commentID")
	parentComment, err := s.store.GetComment(commentID)
	if err != nil || parentComment == nil {
		writeError(w, http.StatusNotFound, "not_found", "Parent comment not found")
		return
	}
	if parentComment.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Parent comment not found")
		return
	}

	var input models.CommentCreate
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	if strings.TrimSpace(input.Body) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "body is required")
		return
	}

	// Set author from current user if not provided.
	if input.Author == "" {
		if u := currentUser(r); u != nil {
			input.Author = u.Name
		}
	}

	actor, source := actorFromRequest(r)
	if input.CreatedBy == "" {
		input.CreatedBy = actor
	}
	if input.Source == "" {
		input.Source = source
	}
	input.ParentID = commentID

	comment, err := s.store.CreateComment(workspaceID, parentComment.ItemID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	s.publishCommentEvent(events.CommentCreated, workspaceID, parentComment.ItemID, comment.ID, parentComment.ItemTitle, "", actor, source)

	writeJSON(w, http.StatusCreated, comment)
}

// handleAddReaction adds an emoji reaction to a comment.
func (s *Server) handleAddReaction(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	commentID := chi.URLParam(r, "commentID")

	// Verify the comment belongs to this workspace.
	comment, err := s.store.GetComment(commentID)
	if err != nil || comment == nil || comment.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	var input struct {
		Emoji string `json:"emoji"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", "Invalid JSON body")
		return
	}
	if strings.TrimSpace(input.Emoji) == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "emoji is required")
		return
	}

	actor, _ := actorFromRequest(r)
	userID := currentUserID(r)

	reaction, err := s.store.AddReaction(commentID, userID, actor, input.Emoji)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Fire SSE event for the reaction.
	if parentComment, cerr := s.store.GetComment(commentID); cerr == nil {
		s.publishReactionEvent(events.ReactionAdded, parentComment)
	}

	writeJSON(w, http.StatusCreated, reaction)
}

// handleRemoveReaction removes an emoji reaction from a comment.
func (s *Server) handleRemoveReaction(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	commentID := chi.URLParam(r, "commentID")
	emoji := chi.URLParam(r, "emoji")

	// Verify the comment belongs to this workspace.
	commentObj, cerr := s.store.GetComment(commentID)
	if cerr != nil || commentObj == nil || commentObj.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Comment not found")
		return
	}

	userID := currentUserID(r)

	if err := s.store.RemoveReaction(commentID, userID, emoji); err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Reaction not found")
		return
	}

	// Fire SSE event for the reaction removal.
	if parentComment, cerr := s.store.GetComment(commentID); cerr == nil {
		s.publishReactionEvent(events.ReactionRemoved, parentComment)
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

// publishReactionEvent publishes a real-time event for reaction changes.
func (s *Server) publishReactionEvent(eventType string, comment *models.Comment) {
	if s.events == nil || comment == nil {
		return
	}
	s.events.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: comment.WorkspaceID,
		ItemID:      comment.ItemID,
	})
}
