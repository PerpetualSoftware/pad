package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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
	if !requireMinRole(w, r, "editor") {
		return
	}
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
	actor, source := actorFromRequest(r)
	s.logActivity(workspaceID, doc.ID, "created", r)
	s.publishEventWithName(events.DocumentCreated, workspaceID, doc.ID, doc.Title, doc.DocType, actor, actorNameFromRequest(r), source)

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
	if !requireMinRole(w, r, "editor") {
		return
	}
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

	actor, source := actorFromRequest(r)
	if !isWebAutoSave {
		s.logActivity(updated.WorkspaceID, updated.ID, "updated", r)
	}
	s.publishEventWithName(events.DocumentUpdated, updated.WorkspaceID, updated.ID, updated.Title, updated.DocType, actor, actorNameFromRequest(r), source)

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteDocument(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	_, doc, ok := s.getWorkspaceDocument(w, r)
	if !ok {
		return
	}

	if err := s.store.DeleteDocument(doc.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	actor, source := actorFromRequest(r)
	s.logActivity(doc.WorkspaceID, doc.ID, "archived", r)
	s.publishEventWithName(events.DocumentArchived, doc.WorkspaceID, doc.ID, doc.Title, doc.DocType, actor, actorNameFromRequest(r), source)

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRestoreDocument(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
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

	actor, source := actorFromRequest(r)
	s.logActivity(doc.WorkspaceID, doc.ID, "restored", r)
	s.publishEventWithName(events.DocumentRestored, doc.WorkspaceID, doc.ID, doc.Title, doc.DocType, actor, actorNameFromRequest(r), source)

	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleQuickSave(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
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

	// Derive actor/source from auth context
	actor, source := actorFromRequest(r)
	if input.CreatedBy == "" {
		input.CreatedBy = actor
	}
	if input.Source == "" {
		input.Source = source
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

	s.logActivity(workspaceID, doc.ID, action, r)
	eventType := events.DocumentUpdated
	if action == "created" {
		eventType = events.DocumentCreated
	}
	s.publishEventWithName(eventType, workspaceID, doc.ID, doc.Title, doc.DocType, actor, actorNameFromRequest(r), source)

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
	s.publishEventWithName(eventType, workspaceID, documentID, title, docType, actor, "", source)
}

// publishEventWithName publishes a real-time event with actor name.
func (s *Server) publishEventWithName(eventType, workspaceID, documentID, title, docType, actor, actorName, source string) {
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
		ActorName:   actorName,
		Source:      source,
	})
}

// actorNameFromRequest returns the authenticated user's display name, or empty string.
func actorNameFromRequest(r *http.Request) string {
	if u := currentUser(r); u != nil {
		return u.Name
	}
	return ""
}

// actorFromRequest derives actor and source from the request's auth context.
// Actor is "user" or "agent" (determined by X-Pad-Agent header).
// Source is "web" (cookie session), "cli" (Bearer token), or falls back to "web".
func actorFromRequest(r *http.Request) (actor, source string) {
	actor = "user"
	source = "web"

	// If an agent name header is present, mark as agent
	if r.Header.Get("X-Pad-Agent") != "" {
		actor = "agent"
	}

	// Determine source from auth method
	if auth := r.Header.Get("Authorization"); auth != "" {
		source = "cli"
	}

	return actor, source
}

// agentMeta returns metadata JSON with the agent name if X-Pad-Agent is set,
// merged with any existing metadata. Returns empty string if no agent.
func agentMeta(r *http.Request, existingMeta string) string {
	agentName := r.Header.Get("X-Pad-Agent")
	if agentName == "" {
		return existingMeta
	}
	if existingMeta == "" || existingMeta == "{}" {
		return fmt.Sprintf(`{"agent":"%s"}`, agentName)
	}
	// Merge: insert agent field into existing JSON
	if strings.HasPrefix(existingMeta, "{") {
		return fmt.Sprintf(`{"agent":"%s",%s`, agentName, existingMeta[1:])
	}
	return existingMeta
}

// logActivity is a helper that logs activity, ignoring errors (best-effort).
func (s *Server) logActivity(workspaceID, documentID, action string, r *http.Request) {
	s.logActivityWithMeta(workspaceID, documentID, action, r, "")
}

// logActivityWithMeta logs activity with optional JSON metadata.
// For "updated" actions, uses debouncing to coalesce rapid successive saves
// (e.g., autosave) into a single activity entry within a cooldown window.
func (s *Server) logActivityWithMeta(workspaceID, documentID, action string, r *http.Request, metadata string) {
	_, _ = s.logActivityWithMetaReturningID(workspaceID, documentID, action, r, metadata)
}

// logActivityWithMetaReturningID is like logActivityWithMeta but returns the activity ID.
// The ID is either newly created or the coalesced existing activity's ID (for debounced updates).
func (s *Server) logActivityWithMetaReturningID(workspaceID, documentID, action string, r *http.Request, metadata string) (string, error) {
	actor, source := actorFromRequest(r)
	metadata = agentMeta(r, metadata)
	return s.store.CreateActivityDebounced(models.Activity{
		WorkspaceID: workspaceID,
		DocumentID:  documentID,
		Action:      action,
		Actor:       actor,
		Source:      source,
		Metadata:    metadata,
		UserID:      currentUserID(r),
	})
}

// diffFields compares old and new field JSON strings and returns a human-readable
// summary of changes (e.g. "status: open → done, priority: medium → high").
// valueOrEmpty returns the string or "(none)" if empty.
func valueOrEmpty(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// appendChange adds a change description to existing metadata JSON.
func appendChange(meta, change string) string {
	if meta == "" {
		return fmt.Sprintf(`{"changes":%q}`, change)
	}
	// Parse existing changes and append
	var m map[string]string
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		return fmt.Sprintf(`{"changes":%q}`, change)
	}
	if existing, ok := m["changes"]; ok {
		m["changes"] = existing + "; " + change
	} else {
		m["changes"] = change
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func diffFields(oldFields, newFields string) string {
	var oldMap, newMap map[string]any
	if err := json.Unmarshal([]byte(oldFields), &oldMap); err != nil {
		return ""
	}
	if err := json.Unmarshal([]byte(newFields), &newMap); err != nil {
		return ""
	}

	var changes []string
	for key, newVal := range newMap {
		oldVal, exists := oldMap[key]
		newStr := fmt.Sprintf("%v", newVal)
		if !exists {
			changes = append(changes, fmt.Sprintf("%s: → %s", key, newStr))
		} else {
			oldStr := fmt.Sprintf("%v", oldVal)
			if oldStr != newStr {
				changes = append(changes, fmt.Sprintf("%s: %s → %s", key, oldStr, newStr))
			}
		}
	}

	// Sort for deterministic output
	sort.Strings(changes)
	return strings.Join(changes, ", ")
}
