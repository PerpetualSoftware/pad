package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// handleListComments returns all comments for an item.
func (s *Server) handleListComments(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, itemSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
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
	// Check edit permission (grant-aware for guests)
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
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

	// Log activity first so we can link the comment to the activity record.
	// This prevents duplicate timeline entries (one for the comment, one for the activity).
	// Only set ActivityID on success — comments.activity_id has a FK constraint,
	// and CreateActivity returns an ID even on insert failure.
	if activityID, err := s.logActivityWithMetaReturningID(workspaceID, item.ID, "commented", r, ""); err == nil && activityID != "" {
		input.ActivityID = activityID
	}

	comment, err := s.store.CreateComment(workspaceID, item.ID, currentUserID(r), input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Publish SSE event
	s.publishCommentEvent(events.CommentCreated, workspaceID, item.ID, comment.ID, item.Title, item.CollectionSlug, actor, source)
	s.dispatchWebhook(workspaceID, "comment.created", comment)

	writeJSON(w, http.StatusCreated, comment)
}

// handleDeleteComment removes a comment.
func (s *Server) handleDeleteComment(w http.ResponseWriter, r *http.Request) {
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
	if !s.requireCommentVisible(w, r, workspaceID, comment) {
		return
	}
	// Check edit permission on the comment's item (grant-aware for guests)
	if commentItem, ierr := s.store.GetItem(comment.ItemID); ierr == nil && commentItem != nil {
		if !s.requireEditPermission(w, r, workspaceID, commentItem.ID, commentItem.CollectionID) {
			return
		}
	} else if !requireMinRole(w, r, "editor") {
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

// handleUpdateComment edits a comment's body. Editing is an authorship
// operation — distinct from delete, which any item editor may do — so only
// the comment author (matching user_id) or a platform admin may edit.
// Comments with no recorded user_id (created before TASK-1663, or
// agent/system comments) are admin-only. (PLAN-1662.)
func (s *Server) handleUpdateComment(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	commentID := chi.URLParam(r, "commentID")

	comment, cerr := s.store.GetComment(commentID)
	if cerr != nil || comment == nil || comment.WorkspaceID != workspaceID {
		writeError(w, http.StatusNotFound, "not_found", "Comment not found")
		return
	}
	if !s.requireCommentVisible(w, r, workspaceID, comment) {
		return
	}
	if !s.canEditComment(r, comment) {
		writeError(w, http.StatusForbidden, "forbidden", "Only the comment author or an admin can edit this comment")
		return
	}

	var input struct {
		Body string `json:"body"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body := strings.TrimSpace(input.Body)
	if body == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "body is required (use delete to remove a comment)")
		return
	}

	updated, err := s.store.UpdateComment(commentID, body)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Comment not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	actor, source := actorFromRequest(r)
	var title, collSlug string
	if item, ierr := s.store.GetItem(updated.ItemID); ierr == nil && item != nil {
		title = item.Title
		collSlug = item.CollectionSlug
	}
	s.publishCommentEvent(events.CommentUpdated, workspaceID, updated.ItemID, updated.ID, title, collSlug, actor, source)
	s.dispatchWebhook(workspaceID, "comment.updated", updated)

	writeJSON(w, http.StatusOK, updated)
}

// canEditComment reports whether the requester may edit the given comment:
// the authenticated author (matching user_id) or a platform admin. A comment
// with an empty user_id has no provable author, so only admins can edit it.
func (s *Server) canEditComment(r *http.Request, comment *models.Comment) bool {
	u := currentUser(r)
	if u == nil {
		return false
	}
	if u.Role == "admin" {
		return true
	}
	return comment.UserID != "" && comment.UserID == u.ID
}

// handleCreateReply creates a reply to an existing comment.
func (s *Server) handleCreateReply(w http.ResponseWriter, r *http.Request) {
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
	if !s.requireCommentVisible(w, r, workspaceID, parentComment) {
		return
	}
	// Check edit permission on the parent comment's item (grant-aware for guests)
	if commentItem, ierr := s.store.GetItem(parentComment.ItemID); ierr == nil && commentItem != nil {
		if !s.requireEditPermission(w, r, workspaceID, commentItem.ID, commentItem.CollectionID) {
			return
		}
	} else if !requireMinRole(w, r, "editor") {
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

	comment, err := s.store.CreateComment(workspaceID, parentComment.ItemID, currentUserID(r), input)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Comment replies don't go through logActivity (no "commented" activity
	// row is emitted for replies — see handleCreateComment for the non-reply
	// path that does). Bump last_write_at explicitly so engagement metrics
	// reflect reply authorship too (PLAN-1542 / TASK-1543).
	s.store.TouchUserWrite(r.Context(), currentUserID(r))

	// Resolve the item's collection slug for SSE filtering
	replyCollSlug := ""
	if replyItem, err := s.store.GetItem(parentComment.ItemID); err == nil && replyItem != nil {
		replyCollSlug = replyItem.CollectionSlug
	}
	s.publishCommentEvent(events.CommentCreated, workspaceID, parentComment.ItemID, comment.ID, parentComment.ItemTitle, replyCollSlug, actor, source)

	writeJSON(w, http.StatusCreated, comment)
}

// handleAddReaction adds an emoji reaction to a comment.
func (s *Server) handleAddReaction(w http.ResponseWriter, r *http.Request) {
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
	if !s.requireCommentVisible(w, r, workspaceID, comment) {
		return
	}
	// Check edit permission on the comment's item (grant-aware for guests)
	if commentItem, ierr := s.store.GetItem(comment.ItemID); ierr == nil && commentItem != nil {
		if !s.requireEditPermission(w, r, workspaceID, commentItem.ID, commentItem.CollectionID) {
			return
		}
	} else if !requireMinRole(w, r, "editor") {
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
	if !s.requireCommentVisible(w, r, workspaceID, commentObj) {
		return
	}
	// Check edit permission on the comment's item (grant-aware for guests)
	if commentItem, ierr := s.store.GetItem(commentObj.ItemID); ierr == nil && commentItem != nil {
		if !s.requireEditPermission(w, r, workspaceID, commentItem.ID, commentItem.CollectionID) {
			return
		}
	} else if !requireMinRole(w, r, "editor") {
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

// requireCommentVisible checks that a comment's underlying item is in a visible
// collection. Writes a 404 and returns false if not.
func (s *Server) requireCommentVisible(w http.ResponseWriter, r *http.Request, workspaceID string, comment *models.Comment) bool {
	item, err := s.store.GetItem(comment.ItemID)
	if err != nil || item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Comment not found")
		return false
	}
	return s.requireItemVisible(w, r, workspaceID, item)
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
	// Resolve the item's collection slug so SSE filtering can scope this event
	collSlug := ""
	if item, err := s.store.GetItem(comment.ItemID); err == nil && item != nil {
		collSlug = item.CollectionSlug
	}
	s.events.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: comment.WorkspaceID,
		ItemID:      comment.ItemID,
		Collection:  collSlug,
	})
}
