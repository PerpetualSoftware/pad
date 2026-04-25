package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/models"
)

func (s *Server) handleListDocuments(w http.ResponseWriter, r *http.Request) {
	// Legacy documents are not covered by the grants model — block guests.
	if !requireMinRole(w, r, "viewer") {
		return
	}
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
		writeInternalError(w, err)
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
		writeInternalError(w, err)
		return
	}

	// Log activity and publish event
	actor, source := actorFromRequest(r)
	s.logActivity(workspaceID, doc.ID, "created", r)
	s.publishEventWithName(events.DocumentCreated, workspaceID, doc.ID, doc.Title, doc.DocType, actor, actorNameFromRequest(r), source)

	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleGetDocument(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
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
		writeInternalError(w, err)
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
		writeInternalError(w, err)
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
		return auditMeta(map[string]string{"agent": agentName})
	}
	// Merge: insert agent field into existing JSON object
	if strings.HasPrefix(existingMeta, "{") {
		agentJSON, _ := json.Marshal(agentName)
		return fmt.Sprintf(`{"agent":%s,%s`, agentJSON, existingMeta[1:])
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
		IPAddress:   clientIP(r),
		UserAgent:   r.Header.Get("User-Agent"),
	})
}

// logAuditEvent logs a non-workspace audit event (e.g. login, logout).
// Best-effort: errors are silently ignored.
func (s *Server) logAuditEvent(action string, r *http.Request, metadata string) {
	s.logAuditEventForUser(action, r, currentUserID(r), metadata)
}

// logAuditEventForUser logs an audit event with an explicit user ID.
// Use this when the user isn't (yet) in the request context, e.g. after
// a successful login/register/bootstrap where the session was just created.
func (s *Server) logAuditEventForUser(action string, r *http.Request, userID string, metadata string) {
	actor, source := actorFromRequest(r)
	if metadata == "" {
		metadata = "{}"
	}
	_, _ = s.store.CreateActivity(models.Activity{
		Action:    action,
		Actor:     actor,
		Source:    source,
		Metadata:  metadata,
		UserID:    userID,
		IPAddress: clientIP(r),
		UserAgent: r.Header.Get("User-Agent"),
	})
}

// auditMeta safely marshals a map to a JSON string for audit log metadata.
// Falls back to "{}" on marshal error so audit calls never break.
func auditMeta(kv map[string]string) string {
	data, err := json.Marshal(kv)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// logWorkspaceAuditEvent logs a workspace-scoped audit event (e.g. member invited).
// Best-effort: errors are silently ignored.
func (s *Server) logWorkspaceAuditEvent(workspaceID, action string, r *http.Request, metadata string) {
	actor, source := actorFromRequest(r)
	if metadata == "" {
		metadata = "{}"
	}
	_, _ = s.store.CreateActivity(models.Activity{
		WorkspaceID: workspaceID,
		Action:      action,
		Actor:       actor,
		Source:      source,
		Metadata:    metadata,
		UserID:      currentUserID(r),
		IPAddress:   clientIP(r),
		UserAgent:   r.Header.Get("User-Agent"),
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
		newStr := formatChangeValue(key, newVal)
		if !exists {
			changes = append(changes, fmt.Sprintf("%s: → %s", key, newStr))
			continue
		}
		// Compare on the raw decoded values rather than the display strings.
		// formatChangeValue() collapses structured values to fixed labels
		// (e.g. `(1 note)`, `(object)`), so two semantically distinct edits
		// of the same cardinality would otherwise produce equal display
		// strings and the change would be silently dropped from the activity
		// metadata. reflect.DeepEqual is correct for the types `json.Unmarshal`
		// produces here (nil, bool, float64, string, []any, map[string]any).
		if reflect.DeepEqual(oldVal, newVal) {
			continue
		}
		oldStr := formatChangeValue(key, oldVal)
		changes = append(changes, fmt.Sprintf("%s: %s → %s", key, oldStr, newStr))
	}

	// Sort for deterministic output
	sort.Strings(changes)
	return strings.Join(changes, ", ")
}

// formatChangeValue renders a JSON-decoded field value as a human-readable
// string for the activity-feed `changes` summary. Primitives use Go's default
// formatting; structured values (slices, maps) are summarised as a count or
// short label so the activity card never surfaces Go's `[map[k:v ...]]`
// repr to end users (BUG-748).
//
// Known structured fields (`implementation_notes`, `decision_log`) get
// domain-specific phrasing; unknown structured fields fall back to a generic
// `(N items)` / `(object)` label.
func formatChangeValue(field string, val any) string {
	if val == nil {
		return ""
	}
	switch v := val.(type) {
	case []any:
		switch field {
		case models.ItemFieldImplementationNotes:
			if len(v) == 1 {
				return "(1 note)"
			}
			return fmt.Sprintf("(%d notes)", len(v))
		case models.ItemFieldDecisionLog:
			if len(v) == 1 {
				return "(1 entry)"
			}
			return fmt.Sprintf("(%d entries)", len(v))
		}
		if len(v) == 1 {
			return "(1 item)"
		}
		return fmt.Sprintf("(%d items)", len(v))
	case map[string]any:
		return "(object)"
	default:
		return fmt.Sprintf("%v", v)
	}
}
