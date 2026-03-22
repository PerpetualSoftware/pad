package server

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := models.DocumentListParams{
		Type:   r.URL.Query().Get("type"),
		Status: r.URL.Query().Get("status"),
		Tag:    r.URL.Query().Get("tag"),
		Query:  r.URL.Query().Get("q"),
		Sort:   r.URL.Query().Get("sort"),
		Order:  r.URL.Query().Get("order"),
	}

	if pinnedStr := r.URL.Query().Get("pinned"); pinnedStr != "" {
		pinned := pinnedStr == "true"
		params.Pinned = &pinned
	}

	docs, err := s.store.ListDocuments(workspaceID, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if docs == nil {
		docs = []models.Document{}
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) handleCreateDocument(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var input models.DocumentCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Title is required")
		return
	}
	if input.DocType != "" && !models.IsValidDocType(input.DocType) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid doc_type")
		return
	}
	if input.Status != "" && !models.IsValidStatus(input.Status) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid status")
		return
	}

	doc, err := s.store.CreateDocument(workspaceID, input)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "conflict", "A document with this title already exists in this workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	// Log activity and publish event
	s.logActivity(workspaceID, doc.ID, "created", input.CreatedBy, input.Source)
	s.publishEvent(events.DocumentCreated, workspaceID, doc.ID, doc.Title, doc.DocType, input.CreatedBy, input.Source)

	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleUpdateDocument(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	var input models.DocumentUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.DocType != nil && !models.IsValidDocType(*input.DocType) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid doc_type")
		return
	}
	if input.Status != nil && !models.IsValidStatus(*input.Status) {
		writeError(w, http.StatusBadRequest, "bad_request", "Invalid status")
		return
	}

	updated, err := s.store.UpdateDocument(doc.ID, input)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "conflict", "A document with this title already exists in this workspace")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Document not found")
		return
	}

	// Only log activity for meaningful changes (not every auto-save keystroke).
	// Content-only updates from the web editor are too frequent to log individually.
	isContentOnly := input.Content != nil &&
		input.Title == nil && input.DocType == nil && input.Status == nil &&
		input.Tags == nil && input.Pinned == nil && input.SortOrder == nil
	isWebAutoSave := isContentOnly && input.Source == "web"

	if !isWebAutoSave {
		s.logActivity(updated.WorkspaceID, updated.ID, "updated", input.LastModifiedBy, input.Source)
	}
	s.publishEvent(events.DocumentUpdated, updated.WorkspaceID, updated.ID, updated.Title, updated.DocType, input.LastModifiedBy, input.Source)

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	if err := s.store.DeleteDocument(doc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	s.logActivity(doc.WorkspaceID, doc.ID, "archived", "user", "web")
	s.publishEvent(events.DocumentArchived, doc.WorkspaceID, doc.ID, doc.Title, doc.DocType, "user", "web")

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestoreDocument(w http.ResponseWriter, r *http.Request) {
	// Restore needs special handling — doc is soft-deleted so getWorkspaceDocument won't find it.
	// Verify workspace exists, then restore by ID.
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	docID := chi.URLParam(r, "docID")
	doc, err := s.store.RestoreDocument(docID)
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "Document not found or not archived")
		return
	}

	// Verify the restored doc belongs to this workspace
	if doc.WorkspaceID != workspaceID {
		// Re-delete it since it shouldn't have been restored under this workspace
		_ = s.store.DeleteDocument(docID)
		writeError(w, http.StatusNotFound, "not_found", "Document not found or not archived")
		return
	}

	s.logActivity(doc.WorkspaceID, doc.ID, "restored", "user", "web")
	s.publishEvent(events.DocumentRestored, doc.WorkspaceID, doc.ID, doc.Title, doc.DocType, "user", "web")

	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleQuickSave(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var input models.QuickSave
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Title is required")
		return
	}

	// Defaults
	if input.CreatedBy == "" {
		input.CreatedBy = "user"
	}
	if input.Source == "" {
		input.Source = "web"
	}

	// Check if doc exists to determine activity action
	existing, _ := s.store.GetDocumentByTitle(workspaceID, input.Title)
	action := "created"
	if existing != nil {
		action = "updated"
	}

	doc, err := s.store.QuickSave(workspaceID, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	s.logActivity(workspaceID, doc.ID, action, input.CreatedBy, input.Source)
	eventType := events.DocumentUpdated
	if action == "created" {
		eventType = events.DocumentCreated
	}
	s.publishEvent(eventType, workspaceID, doc.ID, doc.Title, doc.DocType, input.CreatedBy, input.Source)

	status := http.StatusOK
	if action == "created" {
		status = http.StatusCreated
	}
	writeJSON(w, status, doc)
}

func (s *Server) handleBulkRead(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []string `json:"ids"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	docs, err := s.store.BulkRead(input.IDs)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if docs == nil {
		docs = []models.Document{}
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) handleGetBacklinks(w http.ResponseWriter, r *http.Request) {
	workspaceID, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	backlinks, err := s.store.GetBacklinks(workspaceID, doc.Title)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if backlinks == nil {
		backlinks = []models.Document{}
	}

	// Filter out the document itself
	var filtered []models.Document
	for _, bl := range backlinks {
		if bl.ID != doc.ID {
			filtered = append(filtered, bl)
		}
	}
	if filtered == nil {
		filtered = []models.Document{}
	}

	writeJSON(w, http.StatusOK, filtered)
}

func (s *Server) handleGetLinks(w http.ResponseWriter, r *http.Request) {
	workspaceID, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}
	linkedDocs, err := s.store.GetLinks(workspaceID, doc.Content)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if linkedDocs == nil {
		linkedDocs = []models.Document{}
	}

	writeJSON(w, http.StatusOK, linkedDocs)
}

func (s *Server) handleGetContext(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var types []string
	if t := r.URL.Query().Get("type"); t != "" {
		types = strings.Split(t, ",")
	}

	includeContent := true
	if r.URL.Query().Get("include_content") == "false" {
		includeContent = false
	}

	docs, err := s.store.GetContext(workspaceID, types, includeContent)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if docs == nil {
		docs = []models.Document{}
	}

	writeJSON(w, http.StatusOK, docs)
}

// publishEvent publishes a real-time event if the event bus is available (best-effort).
func (s *Server) publishEvent(eventType, workspaceID, documentID, title, docType, actor, source string) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: workspaceID,
		DocumentID:  documentID,
		Title:       title,
		DocType:     docType,
		Actor:       actor,
		Source:      source,
	})
}

// logActivity is a helper that logs activity, ignoring errors (best-effort).
func (s *Server) logActivity(workspaceID, documentID, action, actor, source string) {
	if actor == "" {
		actor = "user"
	}
	if source == "" {
		source = "web"
	}
	_ = s.store.CreateActivity(models.Activity{
		WorkspaceID: workspaceID,
		DocumentID:  documentID,
		Action:      action,
		Actor:       actor,
		Source:      source,
	})
}
