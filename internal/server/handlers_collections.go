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

// reservedSchemaFieldKeys are collection schema field keys a schema may not
// declare. Two unrelated groups, for two unrelated reasons — see below.
//
// parent / plan collide
// with the parent/plan extraction at handlers_items.go:584 (create),
// :851 (PATCH), and :2147 (list filter). A schema field keyed exactly
// "parent" or "plan" makes schemaHasField (handlers_items.go:2190) return
// true, which makes those sites silently skip fields-JSON extraction —
// disabling subtask linking for the collection with no error anywhere
// (TASK-1912, stage 1 of the IDEA-1746 consolidation plan).
// The system-metadata keys (implementation_notes, decision_log, github_pr,
// convention) are reserved here for a DIFFERENT reason, added with BUG-2674.
// They are written by Pad itself and deliberately live outside every schema —
// MigrateFields now carries them across a move by identity rather than
// schema-matching them. A schema that declares one of those keys re-creates
// the collision the carry-through exists to avoid: the migrated array arrives
// at ValidateFieldsDetailed, which now DOES see the key (it iterates
// schema.Fields), and a `text` FieldDef rejects an array — turning a move that
// previously destroyed the notes into one that fails outright. Forbidding the
// declaration is the honest fix; coercing or silently skipping validation for
// a key the schema genuinely declares would be guessing at which meaning the
// author wanted.
var reservedSchemaFieldKeys = append([]string{"parent", "plan"}, models.ReservedItemFieldKeys()...)

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

// validateCollectionTraits parses and validates an inbound traits blob.
// An empty blob is legal — most collections declare nothing. TASK-2657.
func validateCollectionTraits(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	traits, err := models.ParseCollectionTraits(raw)
	if err != nil {
		return err
	}
	return traits.Validate()
}

// checkTraitConflicts refuses a declaration that would make a workspace-level
// resolution ambiguous: two collections declaring the same artifact_kind, or
// two declaring invocation_field.
//
// SPEC-0 L6 — conflicts fail loud, never silent merges. Without this the
// resolvers still have to pick one, and they pick by collection order, so
// which collection receives an imported artifact or answers `/pad <slug>`
// would depend on sort_order and creation time. That is a coin flip wearing a
// rule's clothes. Codex round 7.
//
// BEST-EFFORT, NOT AN INVARIANT — say so plainly rather than let the name
// imply more than it delivers (Codex round 8). This reads and then writes
// without holding a lock across both, so two concurrent owner-level writes can
// both pass and mint a duplicate. Workspace IMPORT bypasses it entirely by
// design, and a rename that frees a canonical slug can produce a duplicate
// with no write to this path at all.
//
// The database-level version — a partial unique index on the extracted trait,
// the shape migration 054 already uses for invocation_slug — is deliberately
// NOT added in phase 0: existing deployments can already hold duplicates (the
// rename-then-reseed path produces one), so creating such an index would fail
// the migration on exactly the databases that most need fixing. That wants a
// de-duplication pass first, which is its own unit.
//
// So this gate closes the common case — a user or agent declaring a duplicate
// through the API — and the resolvers keep their documented order-dependent
// behaviour for duplicates arriving any other way, because refusing to resolve
// at read time would break a workspace rather than a request.
//
// excludeCollID is the collection being updated, so a collection never
// conflicts with itself.
func (s *Server) checkTraitConflicts(workspaceID, raw, excludeCollID string) error {
	traits, err := models.ParseCollectionTraits(raw)
	if err != nil || traits.IsZero() {
		return nil
	}
	existing, err := s.store.ListTraitedCollections(workspaceID)
	if err != nil {
		return err
	}
	for _, other := range existing {
		if other.ID == excludeCollID {
			continue
		}
		if traits.ArtifactKind != nil && other.Traits.ArtifactKind != nil &&
			other.Traits.ArtifactKind.Kind == traits.ArtifactKind.Kind {
			return fmt.Errorf("collection %q already declares artifact_kind %q; two collections declaring one kind would make imports and exports depend on collection order", other.Slug, traits.ArtifactKind.Kind)
		}
		if traits.InvocationField != "" && other.Traits.InvocationField != "" {
			return fmt.Errorf("collection %q already declares invocation_field; two invocation-routing collections would make playbook resolution depend on collection order", other.Slug)
		}
	}
	return nil
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
		if errors.Is(err, models.ErrInvalidTraitsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidTraitsType.Error())
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

	// Kernel traits are declarations that switch on kernel behavior, so a
	// malformed one must be refused rather than stored: a stored blob that
	// fails to parse degrades to "declares nothing", which is silently the
	// wrong behavior instead of a loud error (SPEC-0 L6). TASK-2657.
	if err := validateCollectionTraits(input.Traits); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.checkTraitConflicts(workspaceID, input.Traits, ""); err != nil {
		writeError(w, http.StatusConflict, "conflict", err.Error())
		return
	}

	coll, err := s.store.CreateCollection(workspaceID, input)
	if err != nil {
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "conflict", uniqueCollectionConflictMessage(err))
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
		if errors.Is(err, models.ErrInvalidTraitsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidTraitsType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Same fail-loud gate as create: a malformed declaration is refused, not
	// stored as a blob that silently parses to "declares nothing". A nil
	// Traits (every pre-TASK-2657 client) skips this and leaves the stored
	// declarations untouched.
	if input.Traits != nil {
		if err := validateCollectionTraits(*input.Traits); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", err.Error())
			return
		}
		if err := s.checkTraitConflicts(workspaceID, *input.Traits, coll.ID); err != nil {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}
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
	// items_changed is set from whether a field MIGRATION WAS REQUESTED, NOT
	// how many rows actually changed (Codex round 7 P1): an item-grant
	// subscriber receives this flag, so keying it off the affected-row count
	// would let a subscriber whose OWN items were unaffected infer that HIDDEN
	// items matched the migrated value — an info leak. "Migration requested"
	// leaks nothing about hidden item values. Captured before the store call
	// (input is passed by value, but read it here for clarity).
	migrationRequested := len(input.Migrations) > 0

	updated, err := s.store.UpdateCollection(coll.ID, input)
	if err != nil {
		// BUG-2265: an optimistic-concurrency loss → structured 409, same
		// wire shape as the item path, BEFORE the generic internal-error path.
		if conflict, ok := asCollectionUpdateConflictError(err); ok {
			writeCollectionUpdateConflictError(w, coll.Slug, conflict)
			return
		}
		// A unique violation reaching here is a RACE past checkTraitConflicts
		// and the slug pre-check — that gate reads then writes without holding
		// a lock across both, which is why TASK-2710 made the constraint the
		// real guarantee. This path recognised NEITHER driver's text and
		// answered 500; it now matches create.
		if isUniqueViolation(err) {
			writeError(w, http.StatusConflict, "conflict", uniqueCollectionConflictMessage(err))
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
	// action (Codex P2). No actor/source: an item-grant subscriber receives
	// this event, so it must not leak the owner's identity/source (Codex P1).
	//
	// items_changed (Codex round 6/7 P1): a requested field migration mutates
	// item `fields` JSON and advances item `seq`. Rather than a SEPARATE
	// items_bulk_updated event (which carries op/count for items an item-grant
	// subscriber can't see, and isn't rename-routed), fold a SANITIZED bool onto
	// this already item-grant-delivered, old-slug-routed event. The client
	// triggers a /items-changes deltaSync (server-filtered to the caller's
	// grants) + refetches an open item, so open views reconcile the migrated
	// field JSON — closing the clobber where a stale full-fields item update
	// would UNDO the migration.
	//
	// The event carries the STABLE collection ID (Codex round 7 P1): slugs are
	// mutable and reusable, and events replay, so a stale rename event could
	// otherwise pass a subscriber's slug-based match for a DIFFERENT collection
	// now owning the old slug. Clients match by collection_id, not slug (the
	// slug(s) stay only for the rename-navigation URL).
	newSlug := ""
	if updated.Slug != coll.Slug {
		newSlug = updated.Slug
	}
	s.publishCollectionEvent(events.CollectionUpdated, workspaceID, updated.ID, coll.Slug, newSlug, migrationRequested)

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
// (BUG-2265). collectionID is the STABLE identity clients match on; Collection
// carries the (old) slug so the SSE visibility filter routes it to workspace
// clients who can see the collection; newSlug is set only on a rename;
// itemsChanged is set when a field migration was requested (a SANITIZED
// reconcile bool). Deliberately SANITIZED — no Actor / ActorName / Source, no
// per-item data — because this event is delivered to item-grant subscribers,
// who must not learn the owner's identity/source or
// anything about items they can't see (Codex P1). Clients only need the
// slug(s) + itemsChanged to refresh their snapshot / re-target / reconcile.
func (s *Server) publishCollectionEvent(eventType, workspaceID, collectionID, collectionSlug, newSlug string, itemsChanged bool) {
	if s.events == nil {
		return
	}
	s.events.Publish(events.Event{
		Type:         eventType,
		WorkspaceID:  workspaceID,
		CollectionID: collectionID,
		Collection:   collectionSlug,
		NewSlug:      newSlug,
		ItemsChanged: itemsChanged,
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

	// Optimistic-concurrency token (BUG-2265 Codex round 8): a caller that
	// resolved the collection by stable id passes the updated_at it read so the
	// delete 409s if a concurrent RENAME re-owned this slug with a DIFFERENT
	// collection (or the collection changed) — never archiving the wrong one.
	// Validated at the boundary (clean 400 on a malformed value).
	expectedUpdatedAt := r.URL.Query().Get("expected_updated_at")
	if expectedUpdatedAt != "" {
		if _, perr := time.Parse(time.RFC3339, expectedUpdatedAt); perr != nil {
			writeError(w, http.StatusBadRequest, "bad_request",
				"expected_updated_at must be an RFC3339 timestamp")
			return
		}
	}

	if err := s.store.DeleteCollection(coll.ID, expectedUpdatedAt); err != nil {
		if conflict, ok := asCollectionUpdateConflictError(err); ok {
			writeCollectionUpdateConflictError(w, coll.Slug, conflict)
			return
		}
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

// isUniqueViolation recognises a unique-index violation from EITHER driver.
//
// The collection handlers used to check only SQLite's "UNIQUE constraint", so
// the identical race answered 409 on SQLite and 500 on Postgres — and the
// update path checked neither. Every item handler already tests both strings;
// this is that test, named, so the next handler needing it does not invent a
// third spelling (codex round 1, P2).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint") || strings.Contains(msg, "duplicate key")
}

// uniqueCollectionConflictMessage distinguishes the indexes a collection write
// can violate, because "a collection with this name already exists" is
// actively misleading for a TASK-2710 trait conflict: the NAME is fine, the
// DECLARATION is taken, and a user told to rename would rename forever.
func uniqueCollectionConflictMessage(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "idx_collections_artifact_kind_per_workspace"):
		return "Another collection in this workspace already declares this artifact kind"
	case strings.Contains(msg, "idx_collections_invocation_field_per_workspace"):
		return "Another collection in this workspace already handles invocation routing"
	default:
		return "A collection with this name already exists"
	}
}
