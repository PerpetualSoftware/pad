package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// reservedSchemaFieldKeys are collection schema field keys that collide
// with the parent/plan extraction at handlers_items.go:584 (create),
// :851 (PATCH), and :2147 (list filter). A schema field keyed exactly
// "parent" or "plan" makes schemaHasField (handlers_items.go:2190) return
// true, which makes those sites silently skip fields-JSON extraction —
// disabling subtask linking for the collection with no error anywhere
// (TASK-1912, stage 1 of the IDEA-1746 consolidation plan).
var reservedSchemaFieldKeys = []string{"parent", "plan"}

// validateNoReservedFieldKeys rejects a schema that newly introduces a
// field keyed "parent" or "plan". prevSchema is nil on collection create
// (nothing to grandfather); on update it's the collection's schema before
// this request, so a reserved key already present there is grandfathered
// in rather than rejected, letting existing workspaces keep working.
//
// Matching is exact and case-sensitive, mirroring schemaHasField (which
// does a plain f.Key == key comparison, no case-folding). Do not make this
// case-insensitive: the web layer's RESERVED_FIELD_KEYS check lowercases
// before comparing, which is stricter than this server-side check — that
// asymmetry is intentional (a client stricter than the server is safe;
// the reverse would mean a field the server rejects could still reach
// schemaHasField's exact-match guard under a different case).
func validateNoReservedFieldKeys(schema models.CollectionSchema, prevSchema *models.CollectionSchema) error {
	for _, f := range schema.Fields {
		for _, reserved := range reservedSchemaFieldKeys {
			if f.Key != reserved {
				continue
			}
			if prevSchema != nil && schemaHasField(*prevSchema, reserved) {
				continue
			}
			return fmt.Errorf("field key %q is reserved and cannot be used in a collection schema", reserved)
		}
	}
	return nil
}

func (s *Server) handleListCollections(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	colls, err := s.store.ListCollections(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if colls == nil {
		colls = []models.Collection{}
	}

	// Filter by collection visibility
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if visibleIDs != nil {
		filtered := make([]models.Collection, 0, len(colls))
		for _, c := range colls {
			if isCollectionVisible(c.ID, visibleIDs) {
				filtered = append(filtered, c)
			}
		}
		colls = filtered
	}

	writeJSON(w, http.StatusOK, colls)
}

func (s *Server) handleCreateCollection(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	var input models.CollectionCreate
	if err := decodeJSON(r, &input); err != nil {
		// IDEA-1488: surface the domain-level error from
		// CollectionCreate.UnmarshalJSON without the "invalid JSON: ..."
		// wrapper from decodeJSON (mirrors handlers_items.go:641
		// precedent).
		if errors.Is(err, models.ErrInvalidSettingsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidSettingsType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Name == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Name is required")
		return
	}

	if input.Schema != "" {
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(input.Schema), &schema); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid schema JSON")
			return
		}
		if err := validateNoReservedFieldKeys(schema, nil); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}

	coll, err := s.store.CreateCollection(workspaceID, input)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "conflict", "A collection with this name already exists")
			return
		}
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, coll)
}

func (s *Server) handleGetCollection(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// Check collection visibility
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	writeJSON(w, http.StatusOK, coll)
}

func (s *Server) handleUpdateCollection(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	var input models.CollectionUpdate
	if err := decodeJSON(r, &input); err != nil {
		// IDEA-1488: surface the domain-level error from
		// CollectionUpdate.UnmarshalJSON without the "invalid JSON: ..."
		// wrapper from decodeJSON (mirrors handlers_items.go:641
		// precedent).
		if errors.Is(err, models.ErrInvalidSettingsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidSettingsType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Schema != nil && *input.Schema != "" {
		var schema models.CollectionSchema
		if err := json.Unmarshal([]byte(*input.Schema), &schema); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "Invalid schema JSON")
			return
		}
		// prevSchema is best-effort: a parse failure on the collection's
		// existing (already-stored) schema falls back to the zero value,
		// which grandfathers nothing — the newer/incoming schema is then
		// held to the stricter no-grandfathering rule rather than risking
		// a false grandfather off of unparsable state.
		var prevSchema models.CollectionSchema
		_ = json.Unmarshal([]byte(coll.Schema), &prevSchema)
		if err := validateNoReservedFieldKeys(schema, &prevSchema); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
	}

	// Extract migrations before updating (they're not stored on the collection)
	migrations := input.Migrations
	input.Migrations = nil

	updated, err := s.store.UpdateCollection(coll.ID, input)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// Apply field value migrations to existing items
	if len(migrations) > 0 {
		if _, err := s.store.MigrateItemFieldValues(coll.ID, migrations); err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Schema updated but migration failed: "+err.Error())
			return
		}
	}

	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteCollection(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "owner") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collSlug := chi.URLParam(r, "collSlug")
	coll, err := s.store.GetCollectionBySlug(workspaceID, collSlug)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	if err := s.store.DeleteCollection(coll.ID); err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Collection not found")
			return
		}
		if strings.Contains(err.Error(), "cannot delete default collection") {
			writeError(w, http.StatusBadRequest, "bad_request", "Cannot delete a default collection")
			return
		}
		writeInternalError(w, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
