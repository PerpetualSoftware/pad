package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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
	if !s.requireCollectionFullyVisible(w, r, workspaceID, coll) {
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

	// BUG-2265: validate the optimistic-concurrency token's format at the
	// boundary so a malformed value is a clean 400 rather than surfacing from
	// the store as a generic 500. The store re-parses (guaranteed to succeed)
	// and does the actual under-lock comparison. Mirrors handlers_items.go's
	// handleUpdateItem boundary check.
	if input.ExpectedUpdatedAt != "" {
		if _, perr := time.Parse(time.RFC3339, input.ExpectedUpdatedAt); perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"expected_updated_at must be an RFC3339 timestamp")
			return
		}
	}

	if input.Schema != nil {
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

	// Field-value migrations (select-option renames) are applied ATOMICALLY
	// inside UpdateCollection's transaction (BUG-2265 Codex P1) — a migration
	// failure rolls back the schema change AND the concurrency-token advance,
	// so nothing is committed and the caller's retry works cleanly. Leave them
	// on the input rather than extracting + running them as a separate,
	// non-atomic write.
	updated, migratedItems, err := s.store.UpdateCollection(coll.ID, input)
	if err != nil {
		// BUG-2265: an optimistic-concurrency loss → structured 409, same
		// wire shape as the item path, BEFORE the generic internal-error path.
		if conflict, ok := asCollectionUpdateConflictError(err); ok {
			writeCollectionUpdateConflictError(w, coll.Slug, conflict)
			return
		}
		writeInternalError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// BUG-2265 (part 3): broadcast a collection.updated event so sibling
	// ItemDetails / collection pages in this workspace refresh their own
	// independent Collection snapshot proactively — shrinking the window in
	// which another client would send a stale expected_updated_at and 409.
	// Published AFTER the fully-atomic update+migration committed, so a failed
	// migration (which rolled everything back) does NOT emit a spurious refresh.
	//
	// ALWAYS routed by the OLD slug (coll.Slug) — the slug sibling tabs still
	// address. On a rename the event carries the NEW slug so those tabs can
	// re-target instead of silently hitting the dead old slug on their next
	// action (Codex P2). No actor/source: an item-grant guest receives this
	// event, so it must not leak the owner's identity/source (Codex P1).
	newSlug := ""
	if updated.Slug != coll.Slug {
		newSlug = updated.Slug
	}
	s.publishCollectionEvent(events.CollectionUpdated, workspaceID, coll.Slug, newSlug)

	// BUG-2265 (Codex P1): a field-value migration mutated item `fields` JSON
	// and advanced item `seq`. collection_updated only refreshes collection
	// METADATA — open item views would keep stale field JSON under the new
	// schema and a later full-fields item update could UNDO the migration. When
	// the migration actually touched ≥1 item, ALSO emit the existing bulk
	// item-mutation signal (items_bulk_updated) so open views reconcile the
	// migrated rows via /items-changes. Routed by the collection so the SSE
	// visibility filter handles it like any collection-scoped item event.
	if migratedItems > 0 {
		s.publishBulkItemsEvent(workspaceID, "migrate", updated.Slug, int(migratedItems), "", "", "", 0)
	}

	writeJSON(w, http.StatusOK, updated)
}

// asCollectionUpdateConflictError reports whether err is (or wraps) a
// store.CollectionUpdateConflictError and returns it. Mirrors
// asUpdateConflictError for the item path (BUG-2265).
func asCollectionUpdateConflictError(err error) (*store.CollectionUpdateConflictError, bool) {
	var conflict *store.CollectionUpdateConflictError
	if errors.As(err, &conflict) {
		return conflict, true
	}
	return nil, false
}

// writeCollectionUpdateConflictError emits the shared update_conflict envelope
// (HTTP 409) for a collection optimistic-concurrency loss. `ref` is the
// collection slug. Reuses writeUpdateConflictEnvelope so the wire shape is
// byte-for-byte identical to the item path's 409 (BUG-2265).
func writeCollectionUpdateConflictError(w http.ResponseWriter, ref string, conflict *store.CollectionUpdateConflictError) {
	writeUpdateConflictEnvelope(w, ref, conflict.ExpectedUpdatedAt, conflict.ActualUpdatedAt)
}

// publishCollectionEvent publishes a real-time collection-level change
// (BUG-2265). Collection carries the (old) slug so the SSE visibility filter
// routes it to workspace clients who can see the collection; newSlug is set
// only on a rename. Deliberately SANITIZED — no Actor / ActorName / Source —
// because this event is delivered to item-grant-only subscribers, who must
// not learn the owner's identity or edit source (Codex P1). Clients only need
// the slug(s) to refresh their snapshot / re-target.
func (s *Server) publishCollectionEvent(eventType, workspaceID, collectionSlug, newSlug string) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Type:        eventType,
		WorkspaceID: workspaceID,
		Collection:  collectionSlug,
		NewSlug:     newSlug,
	})
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
	if !s.requireCollectionFullyVisible(w, r, workspaceID, coll) {
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
