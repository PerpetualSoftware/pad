package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/xarmian/pad/internal/events"
	"github.com/xarmian/pad/internal/items"
	"github.com/xarmian/pad/internal/models"
)

// handleListItems lists all items across collections in a workspace.
func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := parseItemListParams(r)
	result, err := s.store.ListItems(workspaceID, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if result == nil {
		result = []models.Item{}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleListCollectionItems lists items within a specific collection.
func (s *Server) handleListCollectionItems(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	params := parseItemListParams(r)
	params.CollectionSlug = collSlug

	result, err := s.store.ListItems(workspaceID, params)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if result == nil {
		result = []models.Item{}
	}

	writeJSON(w, http.StatusOK, result)
}

// handleCreateItem creates a new item in a collection, validating fields against the schema.
func (s *Server) handleCreateItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	var input models.ItemCreate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Title is required")
		return
	}

	// Parse collection schema
	var schema models.CollectionSchema
	if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse collection schema")
		return
	}

	// Parse and validate input fields
	fieldMap := make(map[string]any)
	if input.Fields != "" {
		if err := json.Unmarshal([]byte(input.Fields), &fieldMap); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid fields JSON")
			return
		}
	}

	// Resolve relation fields (slugs/refs → UUIDs) before validation
	if err := s.resolveRelationFields(workspaceID, fieldMap, schema); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if err := items.ValidateFields(fieldMap, schema); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}

	// Marshal validated/defaulted fields back
	validatedFields, err := json.Marshal(fieldMap)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to marshal validated fields")
		return
	}
	input.Fields = string(validatedFields)

	item, err := s.store.CreateItem(workspaceID, coll.ID, input)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "conflict", "An item with this title already exists")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	s.logActivity(workspaceID, item.ID, "created", input.CreatedBy, input.Source)
	s.publishItemEvent(events.ItemCreated, workspaceID, item.ID, item.Title, collSlug, input.CreatedBy, input.Source)

	writeJSON(w, http.StatusCreated, item)
}

// handleGetItem retrieves a single item by slug.
func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	writeJSON(w, http.StatusOK, item)
}

// handleUpdateItem updates an existing item (fields, content, or both).
func (s *Server) handleUpdateItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	var input models.ItemUpdate
	if err := decodeJSON(r, &input); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// If fields are being updated, validate against schema
	if input.Fields != nil {
		coll, err := s.store.GetCollection(item.CollectionID)
		if err != nil || coll == nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to load collection")
			return
		}

		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(coll.Schema), &schema); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse collection schema")
			return
		}

		fieldMap := make(map[string]any)
		if err := json.Unmarshal([]byte(*input.Fields), &fieldMap); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid fields JSON")
			return
		}

		// Resolve relation fields (slugs/refs → UUIDs) before validation
		if err := s.resolveRelationFields(workspaceID, fieldMap, schema); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}

		if err := items.ValidateFields(fieldMap, schema); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		// Auto-populate date fields on status changes
		autoPopulateDates(fieldMap, item.Fields, schema)

		validatedFields, err := json.Marshal(fieldMap)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to marshal validated fields")
			return
		}
		validated := string(validatedFields)
		input.Fields = &validated
	}

	updated, err := s.store.UpdateItem(item.ID, input)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	s.logActivity(workspaceID, updated.ID, "updated", input.LastModifiedBy, input.Source)
	s.publishItemEvent(events.ItemUpdated, workspaceID, updated.ID, updated.Title, updated.CollectionSlug, input.LastModifiedBy, input.Source)

	writeJSON(w, http.StatusOK, updated)
}

// handleDeleteItem archives (soft-deletes) an item.
func (s *Server) handleDeleteItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	if err := s.store.DeleteItem(item.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	s.logActivity(workspaceID, item.ID, "archived", "user", "web")
	s.publishItemEvent(events.ItemArchived, workspaceID, item.ID, item.Title, item.CollectionSlug, "user", "web")

	w.WriteHeader(http.StatusNoContent)
}

// handleRestoreItem restores an archived item.
func (s *Server) handleRestoreItem(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")

	// We need to find the item even if deleted (for restore).
	item, err := s.store.ResolveItemIncludeDeleted(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found or not archived")
		return
	}

	restored, err := s.store.RestoreItem(item.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Item not found or not archived")
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	s.logActivity(workspaceID, restored.ID, "restored", "user", "web")
	s.publishItemEvent(events.ItemRestored, workspaceID, restored.ID, restored.Title, restored.CollectionSlug, "user", "web")

	writeJSON(w, http.StatusOK, restored)
}

// publishItemEvent publishes a real-time event for item changes.
func (s *Server) publishItemEvent(eventType, workspaceID, itemID, title, collection, actor, source string) {
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

// handlePhasesProgress returns task completion progress for all non-deleted phases.
func (s *Server) handlePhasesProgress(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	progress, err := s.store.GetAllPhasesProgress(workspaceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// handleGetItemTasks returns tasks linked to a phase item via the phase relation field.
func (s *Server) handleGetItemTasks(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	item, err := s.store.ResolveItem(workspaceID, itemSlug)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	tasks, err := s.store.GetTasksForPhase(item.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	if tasks == nil {
		tasks = []models.Item{}
	}
	writeJSON(w, http.StatusOK, tasks)
}

// resolveRelationFields resolves slugs, PREFIX-NUMBER refs, and other identifiers
// in relation fields to their canonical UUIDs. This allows clients to send
// human-readable identifiers (e.g. --field phase=workspace-onboarding) and have
// them stored as UUIDs that the dashboard and queries expect.
func (s *Server) resolveRelationFields(workspaceID string, fields map[string]any, schema models.CollectionSchema) error {
	for _, def := range schema.Fields {
		if def.Type != "relation" {
			continue
		}
		val, exists := fields[def.Key]
		if !exists || val == nil {
			continue
		}
		strVal, ok := val.(string)
		if !ok || strVal == "" {
			continue
		}
		// Already a UUID — nothing to resolve
		if isUUID(strVal) {
			continue
		}
		// Resolve the identifier (slug, PREFIX-NUMBER, etc.) to an item
		item, err := s.store.ResolveItem(workspaceID, strVal)
		if err != nil {
			return fmt.Errorf("field %q: failed to resolve %q: %w", def.Key, strVal, err)
		}
		if item == nil {
			return fmt.Errorf("field %q: item %q not found", def.Key, strVal)
		}
		fields[def.Key] = item.ID
	}
	return nil
}

// isUUID checks if a string looks like a UUID (8-4-4-4-12 hex).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// autoPopulateDates auto-fills start_date/end_date when status changes to active/completed.
// Only sets dates if the schema defines those date fields and the field is currently empty.
func autoPopulateDates(newFields map[string]any, existingFieldsJSON string, schema models.CollectionSchema) {
	// Check if schema has date fields named start_date and/or end_date
	hasStartDate := false
	hasEndDate := false
	for _, f := range schema.Fields {
		if f.Key == "start_date" && f.Type == "date" {
			hasStartDate = true
		}
		if f.Key == "end_date" && f.Type == "date" {
			hasEndDate = true
		}
	}
	if !hasStartDate && !hasEndDate {
		return
	}

	// Get the new status value
	newStatus, ok := newFields["status"].(string)
	if !ok || newStatus == "" {
		return
	}

	// Check if status actually changed
	var oldFields map[string]any
	if existingFieldsJSON != "" {
		json.Unmarshal([]byte(existingFieldsJSON), &oldFields)
	}
	oldStatus, _ := oldFields["status"].(string)
	if newStatus == oldStatus {
		return
	}

	today := time.Now().Format("2006-01-02")

	// Auto-set start_date when moving to active
	if hasStartDate && newStatus == "active" {
		existing, _ := newFields["start_date"].(string)
		if existing == "" {
			newFields["start_date"] = today
		}
	}

	// Auto-set end_date when moving to completed
	if hasEndDate && (newStatus == "completed" || newStatus == "done") {
		existing, _ := newFields["end_date"].(string)
		if existing == "" {
			newFields["end_date"] = today
		}
	}
}

// parseItemListParams extracts item list parameters from the request query string.
func parseItemListParams(r *http.Request) models.ItemListParams {
	params := models.ItemListParams{
		Sort:     r.URL.Query().Get("sort"),
		GroupBy:  r.URL.Query().Get("group_by"),
		Search:   r.URL.Query().Get("search"),
		ParentID: r.URL.Query().Get("parent_id"),
		Tag:      r.URL.Query().Get("tag"),
	}

	if r.URL.Query().Get("include_archived") == "true" {
		params.IncludeArchived = true
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			params.Offset = o
		}
	}

	// Extract field filters: any query param that isn't a known param is a field filter.
	knownParams := map[string]bool{
		"sort": true, "group_by": true, "search": true, "parent_id": true,
		"tag": true, "include_archived": true, "limit": true, "offset": true,
	}

	fields := make(map[string]string)
	for key, values := range r.URL.Query() {
		if knownParams[key] {
			continue
		}
		if len(values) > 0 {
			fields[key] = values[0]
		}
	}
	if len(fields) > 0 {
		params.Fields = fields
	}

	return params
}
