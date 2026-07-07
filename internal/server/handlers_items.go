package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/items"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// errStaleCollabSnapshot signals an UnderItemLock-wrapped
// collab-snapshot validation rejected the PATCH because the
// caller's op_log_cursor was incompatible with the current op-log
// (cursor>0 + empty op-log, or cursor<MIN). The handler unwraps
// this via errors.Is and returns 409 Conflict. Per Codex round 13
// [P1] of TASK-1319.
var errStaleCollabSnapshot = errors.New("collab-snapshot: cursor incompatible with op-log")

// handleListItems lists all items across collections in a workspace.
func (s *Server) handleListItems(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := parseItemListParams(r)
	if err := s.resolveParentFilter(r, workspaceID, &params); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Apply collection visibility filter
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	params.CollectionIDs = visibleIDs

	// Apply item-level filtering for users with item grants (guests or
	// restricted members) so item grants don't leak entire collections.
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if len(grantedItemIDs) > 0 {
		params.CollectionIDs = fullCollIDs
		params.ItemIDs = grantedItemIDs
	}

	result, err := s.store.ListItems(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if result == nil {
		result = []models.Item{}
	}
	s.enrichItemsWithParent(workspaceID, result, visibleIDs)

	writeJSON(w, http.StatusOK, result)
}

// handleListTags returns the distinct tags used across the workspace's items
// with per-tag item counts, ordered by count desc then tag asc. Respects the
// same collection-visibility and item-grant filters as handleListItems so tag
// counts never include items the caller can't see.
func (s *Server) handleListTags(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	collIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var itemIDs []string
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if len(grantedItemIDs) > 0 {
		collIDs = fullCollIDs
		itemIDs = grantedItemIDs
	}

	tags, err := s.store.ListWorkspaceTags(workspaceID, collIDs, itemIDs)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if tags == nil {
		tags = []models.TagCount{}
	}

	writeJSON(w, http.StatusOK, tags)
}

// itemsIndexResponse wraps the skinny-projection items list with bookkeeping
// the local-first read model (PLAN-1343) needs:
//   - total: the row count so the client can size its in-memory array.
//   - cursor: the workspace-scoped monotonic `seq` cursor (TASK-1353).
//     Holds MAX(seq) across the requested scope as a decimal-encoded
//     string so it forward-compatibly tolerates future encoding changes
//     (opaque on the wire — clients MUST NOT parse it as an integer).
//     When the requested scope returns zero rows, the cursor falls back
//     to the workspace's current MAX(seq) so /items-changes?since=cursor
//     starts from the right floor on the next poll instead of replaying
//     every prior mutation. Empty workspaces return "0".
type itemsIndexResponse struct {
	Items  []models.Item `json:"items"`
	Total  int           `json:"total"`
	Cursor string        `json:"cursor"`
}

// handleListItemsIndex returns the skinny-projection of every item in a
// workspace — same row set as handleListItems but without the rich-text
// `content` body. It exists to bootstrap the local-first read model
// (PLAN-1343 Phase 1): the client hydrates its in-memory index + IndexedDB
// cache from one request, then renders every collection page from local
// state without re-fetching.
//
// Filters: optional ?collection=<slug>. No pagination — callers want the
// full set. Auth: same as handleListItems (collection visibility + item
// grants).
func (s *Server) handleListItemsIndex(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	params := store.ItemIndexParams{
		CollectionSlug: r.URL.Query().Get("collection"),
	}
	if r.URL.Query().Get("include_archived") == "true" {
		params.IncludeArchived = true
	}

	// Collection visibility filter — same shape as handleListItems.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	params.CollectionIDs = visibleIDs

	// Item-level grants for guests / restricted members.
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if len(grantedItemIDs) > 0 {
		params.CollectionIDs = fullCollIDs
		params.ItemIDs = grantedItemIDs
	}

	// Snapshot the workspace's MAX(seq) BEFORE the list query so the
	// fallback cursor cannot leapfrog a concurrent insert that
	// committed between the list query and a post-list MaxItemSeq
	// read. Per Codex review of TASK-1353 round 1 [P1]:
	//
	//   List → empty.
	//   Concurrent INSERT lands with seq = M+1 (visible to a future
	//     /items-changes call).
	//   MaxItemSeq → M+1.
	//   Cursor = M+1, response = [].
	//   Client polls /items-changes?since=M+1 → seq > M+1 returns
	//     nothing. The new row is lost.
	//
	// Capturing M BEFORE the list eliminates that window: any insert
	// after the snapshot has seq > M (workspace counter is strictly
	// monotonic per TASK-1352), so /items-changes?since=M will return
	// it. Rows the list DOES see may have seq > M (a concurrent insert
	// the list query happened to observe); MAX(rows.seq) bumps the
	// cursor for that case so the client never re-fetches what was
	// already in the response.
	wsMaxBefore, err := s.store.MaxItemSeq(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	result, err := s.store.ListItemsIndex(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if result == nil {
		result = []models.Item{}
	}
	s.enrichItemsWithParent(workspaceID, result, visibleIDs)

	// Cursor: max(pre-list workspace floor, MAX(returned-rows.seq)).
	// See the long comment above MaxItemSeq for why the floor is read
	// BEFORE ListItemsIndex. Empty workspace + empty result → "0".
	cursorSeq := wsMaxBefore
	for _, it := range result {
		if it.Seq > cursorSeq {
			cursorSeq = it.Seq
		}
	}
	cursor := strconv.FormatInt(cursorSeq, 10)

	writeJSON(w, http.StatusOK, itemsIndexResponse{
		Items:  result,
		Total:  len(result),
		Cursor: cursor,
	})
}

// itemChangeRow embeds the standard skinny Item shape and adds a
// `deleted: bool` flag so clients can distinguish upserts from
// tombstones in a single response. The boolean is a derived view of
// the underlying `deleted_at` so old clients that only key off
// `deleted_at` still work — both fields are populated.
type itemChangeRow struct {
	models.Item
	Deleted bool `json:"deleted"`
	// MovedOut marks a row the caller can no longer see because the item
	// moved into a collection outside their visibility (BUG-1675). Unlike
	// `deleted` (a soft-delete the item still exists under), a moved-out
	// row carries only id + seq — no title/fields/destination — and the
	// client evicts it from its local cache. The item is alive for
	// callers who can see the destination; it's just gone from THIS
	// caller's view.
	MovedOut bool `json:"moved_out,omitempty"`
}

// itemsChangesResponse wraps a delta-fetch result.
//
//   - changes: rows where `seq > since`, in ascending seq order. Each
//     row includes `deleted` (true when the row is soft-deleted) so
//     clients can apply / remove without a second roundtrip.
//   - cursor: the largest seq in the response, decimal-encoded.
//     When the response is empty, the server returns the caller's
//     `since` unchanged so the client doesn't lose position. Treat
//     the value as opaque (re-pass as `?since=<cursor>` on the next
//     poll).
type itemsChangesResponse struct {
	Changes []itemChangeRow `json:"changes"`
	Cursor  string          `json:"cursor"`
}

// handleListItemsChanges is the delta-fetch sibling of
// handleListItemsIndex. Returns rows that have mutated since the
// caller's `?since=<seq>` cursor, including tombstones, so a
// local-first read-model client can resume a workspace without
// re-fetching the entire index (PLAN-1343 / TASK-1354).
//
// Query params:
//   - since: exclusive seq lower bound (`seq > since`). Defaults to 0
//     (full delta == full index modulo ordering). Decimal string;
//     invalid values are 400.
//   - limit: cap on returned rows. Defaults to
//     store.DefaultItemChangesLimit (5000), clamped to
//     store.MaxItemChangesLimit (50000). Invalid values are 400.
//
// Auth: same collection-visibility + item-grant filter as
// /items-index. Soft-deleted rows propagate so they can be removed
// from the client's local index.
func (s *Server) handleListItemsChanges(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	sinceStr := q.Get("since")
	var since int64
	if sinceStr != "" {
		v, err := strconv.ParseInt(sinceStr, 10, 64)
		if err != nil || v < 0 {
			writeError(w, http.StatusBadRequest, "invalid_cursor", "`since` must be a non-negative integer")
			return
		}
		since = v
	}

	var limit int
	if limitStr := q.Get("limit"); limitStr != "" {
		v, err := strconv.Atoi(limitStr)
		if err != nil || v <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_limit", "`limit` must be a positive integer")
			return
		}
		limit = v
	}

	// Collection visibility filter — same shape as handleListItemsIndex.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	params := store.ItemChangesParams{
		CollectionIDs: visibleIDs,
		Since:         since,
		Limit:         limit,
	}

	// Item-level grants for guests / restricted members. Delta sync
	// uses the include-deleted-items variant so a tombstone on a
	// granted item still flows through — without it the grant ID
	// disappears as soon as the item is soft-deleted, and the
	// client never learns the row went away (Codex review of
	// TASK-1354 round 1 [P1]).
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilterIncludeDeletedItems(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if len(grantedItemIDs) > 0 {
		params.CollectionIDs = fullCollIDs
		params.ItemIDs = grantedItemIDs
	}

	rows, err := s.store.ListItemsChangesSince(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Enrich with parent metadata so the local cache rows match the
	// /items-index payload shape. Soft-deleted parents are skipped by
	// the underlying GetItem (deleted_at IS NULL), so we don't leak
	// parent title / ref after the parent itself has been archived.
	s.enrichItemsWithParent(workspaceID, rows, visibleIDs)

	changes := make([]itemChangeRow, 0, len(rows))
	for _, it := range rows {
		changes = append(changes, itemChangeRow{
			Item:    it,
			Deleted: it.DeletedAt != nil,
		})
	}

	// Moved-out tombstones (BUG-1675). Only restricted callers
	// (visibleIDs != nil) can lose visibility on a move; full members
	// (nil = all-access) never do, so skip the extra query for them.
	// The main delta filters by current collection, so an item that
	// moved into a collection this caller can't see vanishes with no
	// eviction signal — append a minimal id+seq tombstone for those.
	if visibleIDs != nil {
		// Collections the caller browses at the collection level (the
		// from-collection a move must originate in). With item grants
		// the main query swaps to fullCollIDs; mirror that here.
		collLevelVisibleIDs := visibleIDs
		if len(grantedItemIDs) > 0 {
			collLevelVisibleIDs = fullCollIDs
		}
		movedOut, mErr := s.store.ListMovedOutSince(workspaceID, since, limit, collLevelVisibleIDs, grantedItemIDs)
		if mErr != nil {
			writeInternalError(w, mErr)
			return
		}
		for _, m := range movedOut {
			changes = append(changes, itemChangeRow{
				Item:     models.Item{ID: m.ID, Seq: m.Seq},
				MovedOut: true,
			})
		}
	}

	// Merge main + moved-out rows into one seq-ascending stream, then
	// cap at the effective limit so pagination stays gap-free: the
	// cursor is the last kept row's seq and any row past it is dropped
	// to be re-fetched on the next poll (seq is strictly monotonic per
	// workspace, so no overlap, no gap).
	sort.Slice(changes, func(i, j int) bool { return changes[i].Seq < changes[j].Seq })
	effLimit := limit
	if effLimit <= 0 {
		effLimit = store.DefaultItemChangesLimit
	}
	if effLimit > store.MaxItemChangesLimit {
		effLimit = store.MaxItemChangesLimit
	}
	if len(changes) > effLimit {
		changes = changes[:effLimit]
	}

	// Cursor: MAX(seq) in the (capped) response. When empty, return
	// `since` unchanged so the client doesn't regress its position.
	cursorSeq := since
	for _, c := range changes {
		if c.Seq > cursorSeq {
			cursorSeq = c.Seq
		}
	}

	writeJSON(w, http.StatusOK, itemsChangesResponse{
		Changes: changes,
		Cursor:  strconv.FormatInt(cursorSeq, 10),
	})
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
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// Gate check: is this collection visible to the user?
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	params := parseItemListParams(r)
	params.CollectionSlug = collSlug

	// For users with item-level grants, if this collection's visibility comes
	// from item-level grants (not a full collection grant), restrict to only
	// the granted items. Applies to both guests and restricted members.
	lcFullCollIDs, lcGrantedItemIDs, lcGrantErr := s.guestResourceFilter(r, workspaceID)
	if lcGrantErr != nil {
		writeInternalError(w, lcGrantErr)
		return
	}
	if len(lcGrantedItemIDs) > 0 {
		hasFullCollectionGrant := false
		for _, id := range lcFullCollIDs {
			if id == coll.ID {
				hasFullCollectionGrant = true
				break
			}
		}
		// Also check member_collection_access for restricted members
		if !hasFullCollectionGrant && workspaceRole(r) != "guest" {
			memberColls, _ := s.store.GetMemberCollectionAccess(workspaceID, currentUserID(r))
			for _, id := range memberColls {
				if id == coll.ID {
					hasFullCollectionGrant = true
					break
				}
			}
		}
		if !hasFullCollectionGrant {
			params.ItemIDs = lcGrantedItemIDs
		}
	}

	var collSchema models.CollectionSchema
	if coll.Schema != "" {
		_ = json.Unmarshal([]byte(coll.Schema), &collSchema)
	}
	if err := s.resolveParentFilter(r, workspaceID, &params, collSchema); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	result, err := s.store.ListItems(workspaceID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if result == nil {
		result = []models.Item{}
	}
	s.enrichItemsWithParent(workspaceID, result, visibleIDs)

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
		writeInternalError(w, err)
		return
	}
	if coll == nil {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	// Check edit permission (grant-aware for guests)
	if !s.requireEditPermission(w, r, workspaceID, "", coll.ID) {
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

	var input models.ItemCreate
	if err := decodeJSON(r, &input); err != nil {
		// IDEA-1488 R2 codex P2: surface the domain-level errors from
		// ItemCreate.UnmarshalJSON without the "invalid JSON: ..."
		// wrapper from decodeJSON. PATCH and the view/collection
		// handlers already do this; the POST path was missed in the
		// original IDEA-1488 work. Mirrors handlers_items.go:641's
		// PATCH-side handling.
		if errors.Is(err, models.ErrInvalidFieldsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidFieldsType.Error())
			return
		}
		if errors.Is(err, models.ErrInvalidTagsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidTagsType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	if input.Title == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "Title is required")
		return
	}

	// Enforce item count limit (workspace-scoped)
	if !s.enforcePlanLimit(w, workspaceID, "items_per_workspace") {
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

	// Extract parent from fields — it's managed via item_links, not stored in fields JSON.
	// Accepts both "parent" and "plan" as the field key.
	// Skip this if the schema actually defines a field with that key.
	var parentValue string
	for _, key := range []string{"parent", "plan"} {
		if schemaHasField(schema, key) {
			continue
		}
		if pv, ok := fieldMap[key]; ok && pv != nil {
			if pvStr, ok := pv.(string); ok && pvStr != "" {
				var resolvedParent *models.Item
				if !isUUID(pvStr) {
					resolvedParent, err = s.store.ResolveItem(workspaceID, pvStr)
					if err != nil || resolvedParent == nil {
						writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("parent %q not found", pvStr))
						return
					}
					parentValue = resolvedParent.ID
				} else {
					resolvedParent, _ = s.store.GetItem(pvStr)
					if resolvedParent == nil {
						writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("parent %q not found", pvStr))
						return
					}
					parentValue = resolvedParent.ID
				}
				// Ensure parent item belongs to this workspace and is visible
				if resolvedParent.WorkspaceID != workspaceID {
					writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("parent %q not found", pvStr))
					return
				}
				if !s.requireItemVisible(w, r, workspaceID, resolvedParent) {
					return
				}
			}
			delete(fieldMap, key)
		}
	}

	item, cerr := s.createItemChecked(r, workspaceID, coll, schema, input, fieldMap, parentValue)
	if cerr != nil {
		writeError(w, cerr.status, cerr.code, cerr.message)
		return
	}

	createVisIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if err := s.enrichItemForResponse(item, createVisIDs); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, item)
}

// itemCreateError carries an HTTP-status hint out of createItemChecked so
// callers (handleCreateItem and the artifact-import handler) can map the
// failure to a response without re-classifying it. Mirrors the
// importStatusError pattern in handlers_import_bundle.go.
type itemCreateError struct {
	status  int
	code    string
	message string
}

func (e *itemCreateError) Error() string { return e.message }

// createItemChecked is the shared item-create core: schema-field validation →
// workspace-unique-field precheck → persist → optional parent link → activity
// log + SSE event + webhook dispatch. Both handleCreateItem and the
// artifact-import handler call it so neither path can drop straight to
// store.CreateItem and skip validation, the uniqueness precheck, or the
// create side effects.
//
// The caller owns everything upstream of the validate step: resolving the
// collection, enforcing edit permission + collection visibility, decoding the
// request body, enforcing plan limits, and resolving + visibility-gating any
// parent (passed in as parentValue, an already-resolved item ID or "").
// fieldMap is the parsed-but-not-yet-validated structured fields; this helper
// validates it against schema, marshals the validated/defaulted result back
// into input.Fields, and stamps input.Source from the request auth context
// when the caller left it blank.
//
// Returns the created item (Ref/Slug populated by the store) or an
// *itemCreateError with a status hint.
func (s *Server) createItemChecked(r *http.Request, workspaceID string, coll *models.Collection, schema models.CollectionSchema, input models.ItemCreate, fieldMap map[string]any, parentValue string) (*models.Item, *itemCreateError) {
	if err := items.ValidateFields(fieldMap, schema); err != nil {
		return nil, &itemCreateError{http.StatusBadRequest, "validation_error", err.Error()}
	}

	if err := s.checkUniqueFields(workspaceID, coll.ID, "", schema, fieldMap); err != nil {
		return nil, &itemCreateError{http.StatusConflict, "conflict", err.Error()}
	}

	// Marshal validated/defaulted fields back
	validatedFields, err := json.Marshal(fieldMap)
	if err != nil {
		return nil, &itemCreateError{http.StatusInternalServerError, "internal_error", "Failed to marshal validated fields"}
	}
	input.Fields = string(validatedFields)

	// Persist the source from the request's auth context so the item row
	// reflects which client created it. Without this, items created via
	// the CLI would persist as 'web' (the column default) since the CLI's
	// ItemCreate body has no Source field set, and downstream signals like
	// the dashboard's has_agent_activity flag (TASK-862) would never flip
	// on.
	// If a client explicitly sent a source in the body (e.g. an agent
	// marking itself as 'skill'), respect it.
	if input.Source == "" {
		_, src := actorFromRequest(r)
		input.Source = src
	}

	item, err := s.store.CreateItem(workspaceID, coll.ID, input)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
			// Could be the slug-per-workspace constraint OR the playbook
			// invocation_slug partial unique index (TASK-1378). Keep the
			// message generic so it covers both — the application-layer
			// pre-check (checkUniqueFields) catches the common case with a
			// targeted error message; only a true concurrent race lands here.
			return nil, &itemCreateError{http.StatusConflict, "conflict", "An item conflicts with an existing record (duplicate slug, title, or invocation slug)"}
		}
		slog.Error("createItemChecked: store.CreateItem failed", "error", err)
		return nil, &itemCreateError{http.StatusInternalServerError, "internal_error", "An internal error occurred"}
	}

	// Create parent link if specified
	if parentValue != "" {
		actor, _ := actorFromRequest(r)
		if _, err := s.store.SetParentLink(workspaceID, item.ID, parentValue, actor); err != nil {
			return nil, &itemCreateError{http.StatusInternalServerError, "internal_error", fmt.Sprintf("item created but parent link failed: %v", err)}
		}
	}

	actor, source := actorFromRequest(r)
	s.logActivity(workspaceID, item.ID, "created", r)
	s.publishItemEventWithName(events.ItemCreated, workspaceID, item.ID, item.Title, coll.Slug, actor, actorNameFromRequest(r), source, item.Seq)
	s.dispatchWebhook(workspaceID, "item.created", item)

	return item, nil
}

// writeItemResolveError distinguishes a soft-deleted (archived) item from a
// genuinely-missing one for mutation handlers. An archived item still appears
// in include-archived list results and can be fetched read-only via GET, so a
// bare "Item not found" here misled callers into thinking the data was lost or
// corrupt (BUG-1791). Visibility is enforced exactly as the active path, so an
// archived item is never revealed to a caller who could not otherwise see it.
func (s *Server) writeItemResolveError(w http.ResponseWriter, r *http.Request, workspaceID, ref string) {
	if archived, err := s.store.ResolveItemIncludeDeleted(workspaceID, ref); err == nil && archived != nil && archived.DeletedAt != nil {
		if visible, verr := s.checkItemVisible(workspaceID, archived, currentUser(r), workspaceRole(r), isBearerAuth(r)); verr == nil && visible {
			writeError(w, http.StatusConflict, "archived",
				fmt.Sprintf("%q is archived. Fetch it read-only with GET, or restore it before editing.", ref))
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "Item not found")
}

// handleGetItem retrieves a single item by slug.
func (s *Server) handleGetItem(w http.ResponseWriter, r *http.Request) {
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

	enrichVisIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if err := s.enrichItemForResponse(item, enrichVisIDs); err != nil {
		writeInternalError(w, err)
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

	var input models.ItemUpdate
	if err := decodeJSON(r, &input); err != nil {
		// Surface the domain-level errors from ItemUpdate.UnmarshalJSON
		// (BUG-1144) without the "invalid JSON: ..." wrapper from
		// decodeJSON, so callers see a clean message naming the field.
		if errors.Is(err, models.ErrInvalidFieldsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidFieldsType.Error())
			return
		}
		if errors.Is(err, models.ErrInvalidTagsType) {
			writeError(w, http.StatusBadRequest, "bad_request", models.ErrInvalidTagsType.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// IDEA-1494: precheck closure populated below when the patch
	// includes a fields update AND --force is NOT set. Threads into
	// the three UpdateItem call sites that follow. nil = no guard.
	var openChildrenPrecheck func(tx *sql.Tx, existing *models.Item) error

	// IDEA-1494 R4 P3: parent-link change is captured here and
	// applied AFTER the main UpdateItemWithPreCheck succeeds, so a
	// guard rejection on the status field doesn't leave a committed
	// link change behind. Hoisted out of the `if input.Fields != nil`
	// block so the post-write step can read them.
	var parentValue string
	var parentProvided bool

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

		// Extract parent from fields — it's managed via item_links, not stored in fields JSON.
		// Accepts both "parent" and "plan" as the field key.
		// Skip this if the schema actually defines a field with that key.
		// (parentValue / parentProvided are declared at outer scope so
		// the deferred link-write block below can read them — see
		// IDEA-1494 R4 P3.)
		for _, key := range []string{"parent", "plan"} {
			if schemaHasField(schema, key) {
				continue
			}
			if pv, ok := fieldMap[key]; ok {
				parentProvided = true
				if pv != nil {
					if pvStr, ok := pv.(string); ok && pvStr != "" {
						var resolvedParent *models.Item
						if !isUUID(pvStr) {
							resolvedParent, err = s.store.ResolveItem(workspaceID, pvStr)
							if err != nil || resolvedParent == nil {
								writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("parent %q not found", pvStr))
								return
							}
							parentValue = resolvedParent.ID
						} else {
							resolvedParent, _ = s.store.GetItem(pvStr)
							if resolvedParent == nil {
								writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("parent %q not found", pvStr))
								return
							}
							parentValue = resolvedParent.ID
						}
						// Ensure parent item belongs to this workspace and is visible
						if resolvedParent.WorkspaceID != workspaceID {
							writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("parent %q not found", pvStr))
							return
						}
						if !s.requireItemVisible(w, r, workspaceID, resolvedParent) {
							return
						}
					}
				}
				delete(fieldMap, key)
			}
		}

		if err := items.ValidateFields(fieldMap, schema); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", err.Error())
			return
		}

		if err := s.checkUniqueFields(workspaceID, item.CollectionID, item.ID, schema, fieldMap); err != nil {
			writeError(w, http.StatusConflict, "conflict", err.Error())
			return
		}

		// Auto-populate date fields on status changes
		autoPopulateDates(fieldMap, item.Fields, schema)

		// IDEA-1494 open-children guard. The actual check runs INSIDE
		// the store transaction (see Store.UpdateItemWithPreCheck) so
		// the children-list query and the parent's status write share
		// a snapshot — closes the TOCTOU window flagged in Codex
		// round 2 (P2). Here we just stage the inputs and the
		// visibility filters; the closure runs later under the
		// workspace seq lock + parent-children advisory lock.
		if !input.Force {
			var settings models.CollectionSettings
			if coll.Settings != "" {
				_ = json.Unmarshal([]byte(coll.Settings), &settings)
			}
			// Pre-compute visibility once. The guard's invariant check
			// considers ALL children (data integrity); the visibility
			// filter only affects which children appear in the 409
			// payload — hidden ones are surfaced as a count.
			//
			// Fail CLOSED on visibility-lookup error (Codex round-3 P1):
			// a swallowed error here would leave visIDs==nil, which
			// openChildrenGuardChildVisible treats as "no restriction"
			// — leaking metadata for hidden children. Surfacing the
			// internal error blocks the update entirely rather than
			// risk an information leak.
			visIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
			if visErr != nil {
				writeInternalError(w, visErr)
				return
			}
			guestFull, guestGranted, gerr := s.guestResourceFilter(r, workspaceID)
			if gerr != nil {
				writeInternalError(w, gerr)
				return
			}
			gctx := openChildrenGuardContext{
				r:                    r,
				workspaceID:          workspaceID,
				itemID:               item.ID,
				parentSchema:         schema,
				parentSettings:       settings,
				newFieldMap:          fieldMap,
				visibleCollectionIDs: visIDs,
				guestFullCollIDs:     guestFull,
				guestGrantedItemIDs:  guestGranted,
			}
			openChildrenPrecheck = func(tx *sql.Tx, existing *models.Item) error {
				// Codex round-3 P2: classify the transition against
				// the in-tx snapshot of the parent's fields, not the
				// pre-tx capture. A concurrent writer can change the
				// done-field value between handler-side read and lock
				// acquisition; using `existing.Fields` (loaded by
				// UpdateItemWithPreCheck after the tx began) avoids
				// both false-fire and false-skip classifications.
				txCtx := gctx
				txCtx.currentFieldsJS = existing.Fields
				details, derr := s.runOpenChildrenGuard(tx, txCtx)
				if derr != nil {
					return derr
				}
				if details != nil {
					return &openChildrenGuardError{details: details}
				}
				return nil
			}
		}

		validatedFields, err := json.Marshal(fieldMap)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal_error", "Failed to marshal validated fields")
			return
		}
		validated := string(validatedFields)
		input.Fields = &validated

		// IDEA-1494 R4 P3: parent-link mutations are DEFERRED until
		// after the field write succeeds. Pre-fix this block ran the
		// link update INLINE here — before the precheck even fired —
		// so a PATCH that combined a parent change with a status flip
		// could end up with the link committed AND the field write
		// rejected by the guard. Caller saw 409 but the parent had
		// already moved.
		//
		// Now we record the desired link change and execute it ONLY
		// when the main UpdateItemWithPreCheck call below succeeds.
		// A guard rejection short-circuits with the link untouched.
		// Documented choice: "reorder, don't tx-wrap" — wrapping the
		// link mutation in the same store tx as UpdateItem would
		// require threading a tx through SetParentLink (which has
		// its own tx already, and is also called from the
		// handler_item_links path). Reordering is the smaller surgery
		// and preserves atomicity in the failure direction — caller
		// only sees both writes on success, or neither (with a clear
		// 409) on rejection.
	}

	// Designated-applier routing (PLAN-1248 TASK-1257). When the
	// PATCH body sets the content field AND an active collab room
	// exists for this item, route the new markdown through a
	// connected browser tab instead of writing items.content
	// directly. The chosen tab does editor.commands.setContent,
	// which the y-tiptap binding translates into Y.Doc updates that
	// propagate via the regular sync path; items.content gets
	// refreshed via the next 5s idle flush (TASK-1261).
	//
	// Without this hop, the connected browsers' Y.Doc state would
	// silently overwrite items.content on the next flush and the
	// CLI / API caller's update would be lost.
	//
	// Field-only PATCHes (input.Content == nil) skip this branch
	// entirely; they continue straight to UpdateItem unchanged.
	// fullWriteHandled is set when applyContentViaCollab's directWrite
	// callback ran the FULL UpdateItem (content + title + fields +
	// everything) inside the per-item lock. In that case we must not
	// re-run UpdateItem below — we'd duplicate the write and could
	// produce two version-history rows. Instead we re-fetch the
	// post-write snapshot for the response. Per Codex review round 9.
	var fullWriteHandled bool
	var fullWriteUpdated *models.Item
	// `?source=collab-snapshot` opts out of the applier-routing path so
	// a connected collab tab can flush its Y.Doc-derived markdown to
	// items.content WITHOUT looping back through ApplyExternalContent
	// (which would ask this same tab to apply, ack, and then strip
	// input.Content — leaving items.content unchanged). The flag is
	// trustworthy because the caller already has edit access (else the
	// PATCH would 401/403); the bypass just skips a defensive
	// re-routing that's only useful for EXTERNAL content updates.
	// Per TASK-1260 / PLAN-1248.
	collabSnapshot := r.URL.Query().Get("source") == "collab-snapshot"

	// Stamp the per-version-row attribution so the Versions panel /
	// `pad history` views can distinguish auto-flush snapshots from
	// the user's explicit edits. Without this, every collab-driven
	// 5s flush would land as Source="web" (UpdateItem's default
	// when input.Source is empty — see store/items.go's UpdateItem),
	// triggering the per-(actor, source) throttle and silently
	// suppressing follow-up snapshots from a long editing session.
	//
	// IMPORTANT: this uses VersionSource (not Source) so the
	// attribution lands on the version row only and does NOT mutate
	// `items.source`. Mutating items.source would silently flip a
	// CLI/MCP-created item out of `WorkspaceHasAgentActivity`'s
	// `source IN ('cli', 'mcp')` filter just because the user
	// happened to open the editor and trigger a 5s auto-flush. Per
	// Codex round 3 of TASK-1267 [P2].
	//
	// **Force-override** when the query param is set, even if the
	// body sent a different VersionSource. Otherwise a buggy or
	// malicious client could send `?source=collab-snapshot` (which
	// triggers the applier-bypass) with `"version_source":"cli"`
	// in the body, sneaking a stale browser snapshot through the
	// op-log GC watermark check (TASK-1309 round 5 [P1] keys on
	// VersionSource to gate watermark advancement). The query
	// param is the trustworthy server-side signal; the body
	// VersionSource is client-attacker-controlled. Per Codex round
	// 6 of TASK-1309 [P2].
	if collabSnapshot {
		input.VersionSource = "collab-snapshot"
	}

	// Server-side gate: reject collab-snapshot PATCHes whose
	// op_log_cursor is below MIN(item_yjs_updates.id). Such a
	// cursor proves the flushing tab's Y.Doc was built on op-log
	// rows that no longer exist (PruneAndApply, schema rebuild,
	// dormant GC, or post-force_refresh races). The markdown the
	// PATCH carries is, by definition, stale relative to the
	// canonical content; accepting it would overwrite items.content
	// from a known-bad source. The client-side guard
	// (`forceRefreshInFlight`) covers the common case but an
	// already-in-flight fetch can race a force_refresh and reach
	// this handler with stale content. Per Codex round 10 [P1].
	// Collab-snapshot path (TASK-1319). Run validation + UpdateItem
	// under the per-item collab setup lock so concurrent prunes
	// (PruneAndApply, schema rebuild, dormant GC) cannot land
	// between the cursor check and the items.content write — that
	// race would let a stale snapshot overwrite canonical content
	// the prune just installed. Per Codex round 13 [P1].
	if collabSnapshot && input.Content != nil && s.collab != nil {
		err := s.collab.UnderItemLock(item.ID, func() error {
			if input.OpLogCursor != nil {
				minID, hasMin, merr := s.store.MinOpLogID(item.ID)
				if merr != nil {
					return merr
				}
				// Reject any incompatible cursor:
				//   - cursor > 0 against an empty op-log (entire
				//     log pruned). Flushing tab's Y.Doc was built
				//     on rows that no longer exist.
				//   - cursor < MIN against a non-empty op-log
				//     (includes cursor=0 + non-empty: a Y.Doc
				//     populated by replay binaries whose previous
				//     session never received the post-replay
				//     cursor frame). Per round 12 [P1].
				var stale bool
				switch {
				case !hasMin && *input.OpLogCursor > 0:
					stale = true
				case hasMin && *input.OpLogCursor < minID:
					stale = true
				}
				if stale {
					slog.Info("collab-snapshot: rejecting PATCH; cursor incompatible with op-log",
						"item_id", item.ID,
						"cursor", *input.OpLogCursor,
						"min_id", minID,
						"has_min", hasMin,
					)
					return errStaleCollabSnapshot
				}
			}
			updated, uerr := s.store.UpdateItemWithPreCheck(item.ID, input, openChildrenPrecheck)
			if uerr != nil {
				return uerr
			}
			fullWriteHandled = true
			fullWriteUpdated = updated
			return nil
		})
		if err != nil {
			if details, ok := asOpenChildrenGuardError(err); ok {
				writeOpenChildrenError(w, itemRefOrSlug(*item), details)
				return
			}
			if errors.Is(err, errStaleCollabSnapshot) {
				writeError(w, http.StatusConflict, "stale_collab_snapshot",
					"This editor's view is out of sync with the server; please reload.")
				return
			}
			// Mirror the main UpdateItem path: map UNIQUE constraint /
			// duplicate key races (e.g. concurrent edits both racing the
			// invocation_slug partial unique index) to 409 conflict so
			// they don't surface as misleading 500s.
			if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
				writeError(w, http.StatusConflict, "conflict", "An item conflicts with an existing record (duplicate slug, title, or invocation slug)")
				return
			}
			writeInternalError(w, err)
			return
		}
	} else if collabSnapshot && input.Content != nil && input.OpLogCursor != nil {
		// Fallback for the collab-disabled build (s.collab == nil): no
		// concurrent prunes happen, so the un-locked check is
		// race-free here. The cursor gate semantics are unchanged.
		minID, hasMin, merr := s.store.MinOpLogID(item.ID)
		if merr != nil {
			writeInternalError(w, merr)
			return
		}
		var stale bool
		switch {
		case !hasMin && *input.OpLogCursor > 0:
			stale = true
		case hasMin && *input.OpLogCursor < minID:
			stale = true
		}
		if stale {
			slog.Info("collab-snapshot: rejecting PATCH; cursor incompatible with op-log",
				"item_id", item.ID,
				"cursor", *input.OpLogCursor,
				"min_id", minID,
				"has_min", hasMin,
			)
			writeError(w, http.StatusConflict, "stale_collab_snapshot",
				"This editor's view is out of sync with the server; please reload.")
			return
		}
	}

	if input.Content != nil && s.collab != nil && !collabSnapshot {
		// applyContentViaCollab calls directWrite ONLY on the no-
		// room/no-applier paths (where pruning the op-log is safe
		// and we need to land items.content under the per-item
		// lock). The callback owns the full UpdateItem so a mixed
		// content + title + fields PATCH stays atomic — Round 8's
		// content-only split lost atomicity and broke
		// Store.UpdateItem's content-versioning peek at Title.
		// Per Codex review round 9.
		err := s.applyContentViaCollab(r, item.ID, *input.Content, func() error {
			updated, uerr := s.store.UpdateItemWithPreCheck(item.ID, input, openChildrenPrecheck)
			if uerr != nil {
				return uerr
			}
			fullWriteHandled = true
			fullWriteUpdated = updated
			return nil
		})
		if err == nil {
			// Either the applier path acked (content propagated via
			// Y.Doc; UpdateItem still needs to run for other fields)
			// or directWrite ran the full UpdateItem inside the
			// lock (fullWriteHandled tracks which).
			input.Content = nil
		} else if details, ok := asOpenChildrenGuardError(err); ok {
			// IDEA-1494 R2: the open-children guard fired inside the
			// directWrite callback. Don't let applyContentViaCollab's
			// "any error falls through" policy retry the write —
			// rejection is final.
			writeOpenChildrenError(w, itemRefOrSlug(*item), details)
			return
		}
		// Any other error path (e.g. ErrAllAppliersTimedOut, retry
		// exhaustion) falls through to direct write — graceful
		// degradation. The helper logs the specifics so operators
		// see degraded paths.
	}

	var updated *models.Item
	if fullWriteHandled {
		updated = fullWriteUpdated
	} else {
		updated, err = s.store.UpdateItemWithPreCheck(item.ID, input, openChildrenPrecheck)
	}
	if err != nil {
		// IDEA-1494 R2: surface the open-children guard rejection as
		// the structured 409 BEFORE the UNIQUE-constraint / generic
		// internal-error paths can swallow it. Same shape the
		// in-handler write produced pre-R2, but now correctly fires
		// when the guard ran inside the store tx.
		if details, ok := asOpenChildrenGuardError(err); ok {
			writeOpenChildrenError(w, itemRefOrSlug(*item), details)
			return
		}
		// Map UNIQUE constraint races (e.g. concurrent updates that both
		// pass checkUniqueFields and then both hit the partial unique
		// index on invocation_slug) to 409 conflict, matching the create
		// path. Without this, a benign race surfaces as a misleading 500.
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
			writeError(w, http.StatusConflict, "conflict", "An item conflicts with an existing record (duplicate slug, title, or invocation slug)")
			return
		}
		writeInternalError(w, err)
		return
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	// IDEA-1494 R4 P3: deferred parent-link mutation. Runs ONLY now
	// that the main UpdateItemWithPreCheck succeeded (the precheck
	// passed AND the field write committed). A guard rejection above
	// `return`s before reaching here, so the link is untouched on
	// the failure path.
	//
	// Failure of the link write itself after a successful field
	// write is still possible (e.g. SetParentLink discovers a cycle
	// the cycle-check missed, or a DB error). We surface that as a
	// 500 with the field write already committed — same partial-
	// success window the pre-IDEA-1494 code had in the OTHER
	// direction, and not made worse by the reorder. A future tx-
	// wrap fix could close this entirely; out of scope for round 4.
	if parentProvided {
		if parentValue != "" {
			actor, _ := actorFromRequest(r)
			if _, err := s.store.SetParentLink(workspaceID, item.ID, parentValue, actor); err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", fmt.Sprintf("failed to update parent link: %v", err))
				return
			}
		} else {
			if err := s.store.ClearParentLink(item.ID); err != nil {
				writeError(w, http.StatusInternalServerError, "internal_error", fmt.Sprintf("failed to clear parent link: %v", err))
				return
			}
		}
	}

	// Build rich metadata describing what changed
	var meta string
	if changes := diffFields(item.Fields, updated.Fields); changes != "" {
		meta = fmt.Sprintf(`{"changes":%q}`, changes)
	}
	if input.Title != nil && *input.Title != item.Title {
		if meta == "" {
			titleChange := fmt.Sprintf("title: %s → %s", item.Title, *input.Title)
			meta = fmt.Sprintf(`{"changes":%q}`, titleChange)
		}
	}
	// Track role and assignment changes
	if updated.AgentRoleSlug != item.AgentRoleSlug {
		roleChange := fmt.Sprintf("role: %s → %s", valueOrEmpty(item.AgentRoleName), valueOrEmpty(updated.AgentRoleName))
		meta = appendChange(meta, roleChange)
	}
	if updated.AssignedUserName != item.AssignedUserName {
		assignChange := fmt.Sprintf("assigned: %s → %s", valueOrEmpty(item.AssignedUserName), valueOrEmpty(updated.AssignedUserName))
		meta = appendChange(meta, assignChange)
	}
	actor, source := actorFromRequest(r)
	activityID, _ := s.logActivityWithMetaReturningID(workspaceID, updated.ID, "updated", r, meta)
	s.publishItemEventWithName(events.ItemUpdated, workspaceID, updated.ID, updated.Title, updated.CollectionSlug, actor, actorNameFromRequest(r), source, updated.Seq)
	s.dispatchWebhook(workspaceID, "item.updated", updated)

	// If a comment was attached to this update (e.g. explaining a status change),
	// create a comment linked to the activity entry.
	if input.Comment != nil && strings.TrimSpace(*input.Comment) != "" {
		commentInput := models.CommentCreate{
			Body:       strings.TrimSpace(*input.Comment),
			ActivityID: activityID,
		}
		if u := currentUser(r); u != nil {
			commentInput.Author = u.Name
		}
		commentInput.CreatedBy = actor
		commentInput.Source = source
		comment, cerr := s.store.CreateComment(workspaceID, updated.ID, currentUserID(r), commentInput)
		if cerr != nil {
			slog.Warn("failed to create comment on item update", "item_id", updated.ID, "error", cerr)
		}
		if cerr == nil && comment != nil {
			s.publishCommentEvent(events.CommentCreated, workspaceID, updated.ID, comment.ID, updated.Title, updated.CollectionSlug, actor, source)
			s.dispatchWebhook(workspaceID, "item.updated_with_comment", map[string]interface{}{
				"item":    updated,
				"comment": comment,
				"changes": meta,
			})
		}
	}

	updateVisIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if err := s.enrichItemForResponse(updated, updateVisIDs); err != nil {
		writeInternalError(w, err)
		return
	}

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

	if err := s.store.DeleteItem(item.ID); err != nil {
		writeInternalError(w, err)
		return
	}

	// Fetch the post-delete row so the SSE event carries the new
	// `seq` (DeleteItem bumps it). Falls back to 0 if the row is
	// missing (race with hard-delete or a Postgres timing quirk);
	// downstream SSE consumers backfill via /items-changes on a 0.
	var deleteSeq int64
	if d, derr := s.store.GetItemIncludeDeleted(item.ID); derr == nil && d != nil {
		deleteSeq = d.Seq
	}

	actor, source := actorFromRequest(r)
	s.logActivity(workspaceID, item.ID, "archived", r)
	s.publishItemEventWithName(events.ItemArchived, workspaceID, item.ID, item.Title, item.CollectionSlug, actor, actorNameFromRequest(r), source, deleteSeq)
	s.dispatchWebhook(workspaceID, "item.deleted", item)

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
		writeInternalError(w, err)
		return
	}
	if item == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found or not archived")
		return
	}
	if !s.requireItemVisible(w, r, workspaceID, item) {
		return
	}
	// Check edit permission (grant-aware for guests)
	if !s.requireEditPermission(w, r, workspaceID, item.ID, item.CollectionID) {
		return
	}

	restored, err := s.store.RestoreItem(item.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			writeError(w, http.StatusNotFound, "not_found", "Item not found or not archived")
			return
		}
		// If restore flips the partial unique index back into play and a
		// replacement playbook has already claimed the archived item's
		// invocation_slug, RestoreItem returns a UNIQUE constraint
		// violation. Map it to 409, matching the create/update paths.
		if strings.Contains(err.Error(), "UNIQUE constraint") || strings.Contains(err.Error(), "duplicate key") {
			writeError(w, http.StatusConflict, "conflict", "Cannot restore: another item has claimed this slug or invocation slug")
			return
		}
		writeInternalError(w, err)
		return
	}

	actor, source := actorFromRequest(r)
	s.logActivity(workspaceID, restored.ID, "restored", r)
	s.publishItemEventWithName(events.ItemRestored, workspaceID, restored.ID, restored.Title, restored.CollectionSlug, actor, actorNameFromRequest(r), source, restored.Seq)

	restoreVisIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if err := s.enrichItemForResponse(restored, restoreVisIDs); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, restored)
}

// handleMoveItem moves an item to a different collection with field migration.
func (s *Server) handleMoveItem(w http.ResponseWriter, r *http.Request) {
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

	var input struct {
		TargetCollection string         `json:"target_collection"`
		FieldOverrides   map[string]any `json:"field_overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "Invalid JSON body")
		return
	}
	if input.TargetCollection == "" {
		writeError(w, http.StatusBadRequest, "missing_field", "target_collection is required")
		return
	}

	// Get target collection, verify it's visible, and check edit permission
	targetColl, err := s.store.GetCollectionBySlug(workspaceID, input.TargetCollection)
	if err != nil || targetColl == nil {
		writeError(w, http.StatusBadRequest, "invalid_collection", "Target collection not found")
		return
	}
	targetVisibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	if !isCollectionVisible(targetColl.ID, targetVisibleIDs) {
		writeError(w, http.StatusBadRequest, "invalid_collection", "Target collection not found")
		return
	}
	// Require edit permission on the target collection (not just visibility)
	if !s.requireEditPermission(w, r, workspaceID, "", targetColl.ID) {
		return
	}

	// Don't move to the same collection
	if targetColl.ID == item.CollectionID {
		writeError(w, http.StatusBadRequest, "same_collection", "Item is already in this collection")
		return
	}

	// Get source collection for schema
	sourceColl, err := s.store.GetCollection(item.CollectionID)
	if err != nil || sourceColl == nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to get source collection")
		return
	}

	// Parse schemas
	var sourceSchema, targetSchema models.CollectionSchema
	if err := json.Unmarshal([]byte(sourceColl.Schema), &sourceSchema); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse source schema")
		return
	}
	if err := json.Unmarshal([]byte(targetColl.Schema), &targetSchema); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to parse target schema")
		return
	}

	// Parse current fields
	var currentFields map[string]any
	if err := json.Unmarshal([]byte(item.Fields), &currentFields); err != nil {
		currentFields = make(map[string]any)
	}

	// Migrate fields
	result := items.MigrateFields(currentFields, sourceSchema.Fields, targetSchema.Fields)

	// Apply overrides
	for k, v := range input.FieldOverrides {
		result.Fields[k] = v
	}

	// Check for required field errors (after overrides)
	if len(result.Errors) > 0 {
		writeError(w, http.StatusBadRequest, "missing_required_fields",
			fmt.Sprintf("Required fields missing: %s", strings.Join(result.Errors, ", ")))
		return
	}

	// Serialize migrated fields
	fieldsJSON, err := json.Marshal(result.Fields)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", "Failed to serialize fields")
		return
	}

	// IDEA-1494 R3 P1: same open-children guard as the regular update
	// path. `pad item move ... --field status=completed` would
	// otherwise bypass it. We classify the proposed transition
	// against the DESTINATION collection's schema (conservative —
	// honors the schema the item is moving INTO, so a target-schema
	// terminal value can't be smuggled in via a source whose schema
	// doesn't recognize the value as terminal). Visibility filter is
	// pre-computed and fails closed on lookup error, mirroring the
	// update path.
	//
	// Force override on the move path lives on the URL query
	// (?force=true). Move's request body shape is already fixed
	// ({target_collection, field_overrides}) and adding a body
	// field would require coordinating with the CLI/MCP move
	// callers; the query param sidesteps that without changing the
	// JSON contract. Same semantics as `pad item update --force`.
	moveForce := r.URL.Query().Get("force") == "true"

	var movePrecheck func(tx *sql.Tx, existing *models.Item) error
	if !moveForce {
		var destSettings models.CollectionSettings
		if targetColl.Settings != "" {
			_ = json.Unmarshal([]byte(targetColl.Settings), &destSettings)
		}
		visIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
		if visErr != nil {
			writeInternalError(w, visErr)
			return
		}
		guestFull, guestGranted, gerr := s.guestResourceFilter(r, workspaceID)
		if gerr != nil {
			writeInternalError(w, gerr)
			return
		}
		mgctx := openChildrenGuardContext{
			r:                    r,
			workspaceID:          workspaceID,
			itemID:               item.ID,
			parentSchema:         targetSchema,
			parentSettings:       destSettings,
			newFieldMap:          result.Fields,
			visibleCollectionIDs: visIDs,
			guestFullCollIDs:     guestFull,
			guestGrantedItemIDs:  guestGranted,
		}
		movePrecheck = func(tx *sql.Tx, existing *models.Item) error {
			txCtx := mgctx
			txCtx.currentFieldsJS = existing.Fields
			details, derr := s.runOpenChildrenGuard(tx, txCtx)
			if derr != nil {
				return derr
			}
			if details != nil {
				return &openChildrenGuardError{details: details}
			}
			return nil
		}
	}

	// Move the item
	moved, err := s.store.MoveItemWithPreCheck(item.ID, targetColl.ID, string(fieldsJSON), movePrecheck)
	if err != nil {
		if details, ok := asOpenChildrenGuardError(err); ok {
			writeOpenChildrenError(w, itemRefOrSlug(*item), details)
			return
		}
		writeInternalError(w, err)
		return
	}

	// Log activity with metadata about the move (audit trail). The
	// moved-out tombstone signal (BUG-1675) is recorded durably in
	// item_collection_moves inside the move tx, not derived from this
	// best-effort row.
	actor, source := actorFromRequest(r)
	moveMeta := auditMeta(map[string]string{"from_collection": sourceColl.Slug, "to_collection": targetColl.Slug})
	s.logActivityWithMeta(workspaceID, moved.ID, "moved", r, moveMeta)

	// Publish events for both old and new collections
	s.publishItemEventWithName(events.ItemUpdated, workspaceID, moved.ID, moved.Title, targetColl.Slug, actor, actorNameFromRequest(r), source, moved.Seq)
	s.dispatchWebhook(workspaceID, "item.moved", moved)

	moveVisIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	if err := s.enrichItemForResponse(moved, moveVisIDs); err != nil {
		writeInternalError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, moved)
}

// publishItemEventWithName publishes a real-time event for item changes with actor name.
// `seq` is the item's workspace-scoped monotonic mutation cursor at the
// time of the event (PLAN-1343 / TASK-1358), so SSE consumers can
// reason about ordering and contiguity. Callers that don't have the
// item's current seq handy may pass 0; downstream consumers fall back
// to a /items-changes backfill in that case.
func (s *Server) publishItemEventWithName(eventType, workspaceID, itemID, title, collection, actor, actorName, source string, seq int64) {
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
		ActorName:   actorName,
		Source:      source,
		Seq:         seq,
	})
}

// handleCollectionCheckboxProgress returns per-item markdown-checkbox
// progress for items in a single collection — the bookkeeping that
// powers the list/board/table progress badges for non-plans
// collections. Pairs with /items-index (TASK-1349 / PLAN-1343 Phase 1):
// the index endpoint omits content to keep the wire payload small,
// and this endpoint computes the checkbox counts server-side via
// LENGTH/REPLACE arithmetic so the client doesn't need content at
// all to render the progress UI.
//
// Visibility: enforces the same collection-visibility + item-grant
// rules as handleListItems / handleListItemsIndex. Items the caller
// can't see contribute zero rows to the response.
func (s *Server) handleCollectionCheckboxProgress(w http.ResponseWriter, r *http.Request) {
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

	// Collection-visibility gate.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if !isCollectionVisible(coll.ID, visibleIDs) {
		writeError(w, http.StatusNotFound, "not_found", "Collection not found")
		return
	}

	includeArchived := r.URL.Query().Get("include_archived") == "true"
	progress, err := s.store.CollectionCheckboxProgress(workspaceID, coll.ID, includeArchived)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	// Item-grant filtering: if the caller has item-level grants (guest
	// / restricted member) and the collection isn't fully granted, drop
	// rows for items outside their grant set. This mirrors the same
	// filter handleListItems / handleListItemsIndex apply via
	// guestResourceFilter — without it, a guest could enumerate the
	// existence of items they can't otherwise see by reading their
	// checkbox progress.
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if len(grantedItemIDs) > 0 {
		hasFullCollectionGrant := false
		for _, id := range fullCollIDs {
			if id == coll.ID {
				hasFullCollectionGrant = true
				break
			}
		}
		if !hasFullCollectionGrant && workspaceRole(r) != "guest" {
			memberColls, _ := s.store.GetMemberCollectionAccess(workspaceID, currentUserID(r))
			for _, id := range memberColls {
				if id == coll.ID {
					hasFullCollectionGrant = true
					break
				}
			}
		}
		if !hasFullCollectionGrant {
			granted := make(map[string]struct{}, len(grantedItemIDs))
			for _, id := range grantedItemIDs {
				granted[id] = struct{}{}
			}
			filtered := progress[:0]
			for _, p := range progress {
				if _, ok := granted[p.ItemID]; ok {
					filtered = append(filtered, p)
				}
			}
			progress = filtered
		}
	}

	if progress == nil {
		progress = []store.ItemCheckboxProgress{}
	}
	writeJSON(w, http.StatusOK, progress)
}

// collectionChildrenProgress is the shared implementation for child-item
// completion progress across any collection. It enforces the same
// visibility/guest-grant filtering that the plans-specific path did,
// generalised to an arbitrary collectionSlug.
//
// The caller is responsible for verifying that the collection exists and is
// visible before calling this helper — the helper does NOT write HTTP
// responses.
//
// N+1 note: the restricted-visibility recompute path issues one GetChildItems
// query per parent item. For large collections (e.g. tasks with hundreds of
// items and a guest caller) this can be expensive. The plans endpoint shared
// the same trade-off; it was tolerable for small plan sets. Fixing this
// requires a single-query recompute (e.g. a bulk join limited to visible
// child collections) — tracked as a known limitation, deferred.
func (s *Server) collectionChildrenProgress(
	r *http.Request,
	workspaceID string,
	collectionSlug string,
	collectionID string,
	visibleIDs []string,
	fullCollIDs []string,
	grantedItemIDs []string,
	includeArchived bool,
) ([]store.ItemProgress, error) {
	allProgress, err := s.store.GetAllItemProgress(workspaceID, collectionSlug, includeArchived)
	if err != nil {
		return nil, err
	}

	// For guests with item-level grants, filter the parent items themselves
	// so callers without a grant to a specific parent don't see its row.
	if len(grantedItemIDs) > 0 {
		grantedSet := make(map[string]bool, len(grantedItemIDs))
		for _, id := range grantedItemIDs {
			grantedSet[id] = true
		}
		fullSet := make(map[string]bool, len(fullCollIDs))
		for _, id := range fullCollIDs {
			fullSet[id] = true
		}
		// Check if this collection has a full grant (bypass item-level filter).
		if !fullSet[collectionID] {
			filtered := allProgress[:0]
			for _, p := range allProgress {
				if grantedSet[p.ItemID] {
					filtered = append(filtered, p)
				}
			}
			allProgress = filtered
		}
	}

	// Build a done-context map once so each child is evaluated against its
	// own collection's configured done field, not a global default.
	// ListCollectionsMinimal avoids the per-collection COUNT queries that
	// ListCollections would run — we only need schema + settings.
	wsCollections, _ := s.store.ListCollectionsMinimal(workspaceID)
	ctxMap := buildDoneContextMap(wsCollections)

	// Recompute each parent's progress using only visible children so hidden
	// child counts don't leak through the aggregate totals.
	for i, p := range allProgress {
		children, cerr := s.store.GetChildItems(p.ItemID)
		if cerr != nil {
			continue
		}
		total, done := 0, 0
		for _, child := range children {
			if !isCollectionVisible(child.CollectionID, visibleIDs) {
				continue
			}
			if !s.isItemVisibleToGuest(r, workspaceID, &child, fullCollIDs, grantedItemIDs) {
				continue
			}
			total++
			if isItemDone(child.Fields, child.CollectionID, ctxMap) {
				done++
			}
		}
		allProgress[i].Total = total
		allProgress[i].Done = done
	}
	return allProgress, nil
}

// handlePlansProgress returns child item completion progress for all
// non-deleted plans. Kept as a backward-compat endpoint at /plans-progress;
// delegates to collectionChildrenProgress for the shared visibility logic.
func (s *Server) handlePlansProgress(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}

	// Check that the plans collection is visible to this user.
	plansColl, _ := s.store.GetCollectionBySlug(workspaceID, "plans")
	if visibleIDs != nil {
		if plansColl == nil || !isCollectionVisible(plansColl.ID, visibleIDs) {
			writeJSON(w, http.StatusOK, []interface{}{})
			return
		}
	}

	if visibleIDs != nil {
		collID := ""
		if plansColl != nil {
			collID = plansColl.ID
		}
		// /plans-progress does not support include_archived — plans are
		// always queried without archived parents (false).
		progress, err := s.collectionChildrenProgress(r, workspaceID, "plans", collID, visibleIDs, fullCollIDs, grantedItemIDs, false)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, progress)
		return
	}

	progress, err := s.store.GetAllItemProgress(workspaceID, "plans", false)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// handleCollectionChildrenProgress returns child-item completion progress for
// all items in an arbitrary collection. It preserves the same
// visibility/guest-grant filtering semantics as handlePlansProgress so
// restricted callers can't enumerate hidden child-item counts.
// Supports ?include_archived=true to include soft-deleted parent rows,
// matching the collection page's archived-items toggle.
//
// Route: GET /workspaces/{ws}/collections/{collSlug}/child-progress
func (s *Server) handleCollectionChildrenProgress(w http.ResponseWriter, r *http.Request) {
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

	// Collection-visibility gate: a restricted user without visibility into
	// this collection must receive the same empty/denied shape they'd get
	// from any other guarded endpoint — not child counts for a hidden
	// collection.
	visibleIDs, err := s.visibleCollectionIDs(r, workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if visibleIDs != nil && !isCollectionVisible(coll.ID, visibleIDs) {
		writeJSON(w, http.StatusOK, []interface{}{})
		return
	}

	includeArchived := r.URL.Query().Get("include_archived") == "true"

	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}

	if visibleIDs != nil {
		progress, err := s.collectionChildrenProgress(r, workspaceID, collSlug, coll.ID, visibleIDs, fullCollIDs, grantedItemIDs, includeArchived)
		if err != nil {
			writeInternalError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, progress)
		return
	}

	progress, err := s.store.GetAllItemProgress(workspaceID, collSlug, includeArchived)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

// handleGetItemChildren returns all child items linked to a parent item.
// This is the generalized version — children can come from any collection.
func (s *Server) handleGetItemChildren(w http.ResponseWriter, r *http.Request) {
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

	children, err := s.store.GetChildItems(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if children == nil {
		children = []models.Item{}
	}

	// Filter children by collection visibility and item-level grants
	visibleIDs, visErr := s.visibleCollectionIDs(r, workspaceID)
	if visErr != nil {
		writeInternalError(w, visErr)
		return
	}
	fullCollIDs, grantedItemIDs, grantErr := s.guestResourceFilter(r, workspaceID)
	if grantErr != nil {
		writeInternalError(w, grantErr)
		return
	}
	if visibleIDs != nil {
		filtered := children[:0]
		for _, child := range children {
			if !isCollectionVisible(child.CollectionID, visibleIDs) {
				continue
			}
			if !s.isItemVisibleToGuest(r, workspaceID, &child, fullCollIDs, grantedItemIDs) {
				continue
			}
			filtered = append(filtered, child)
		}
		children = filtered
	}

	s.enrichItemsWithParent(workspaceID, children, visibleIDs)
	if visibleIDs != nil {
		// Visibility-aware has_children: only count visible grandchildren
		for i := range children {
			grandchildren, _ := s.store.GetChildItems(children[i].ID)
			children[i].HasChildren = false
			for _, gc := range grandchildren {
				if !isCollectionVisible(gc.CollectionID, visibleIDs) {
					continue
				}
				if !s.isItemVisibleToGuest(r, workspaceID, &gc, fullCollIDs, grantedItemIDs) {
					continue
				}
				children[i].HasChildren = true
				break
			}
		}
	} else {
		s.store.PopulateHasChildren(children)
	}
	writeJSON(w, http.StatusOK, children)
}

// handleGetItemProgress returns completion progress for an item's children.
// Response: {"total": N, "done": N, "percentage": N}
func (s *Server) handleGetItemProgress(w http.ResponseWriter, r *http.Request) {
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

	// Get visibility filter; when restricted, compute progress from
	// visible children only so hidden child counts don't leak.
	progVisIDs, _ := s.visibleCollectionIDs(r, workspaceID)
	progFullCollIDs, progGrantedItemIDs, progGrantErr := s.guestResourceFilter(r, workspaceID)
	if progGrantErr != nil {
		writeInternalError(w, progGrantErr)
		return
	}
	if progVisIDs != nil {
		// Restricted: compute from visible children in Go using per-collection
		// schemas to determine terminal statuses correctly.
		children, cerr := s.store.GetChildItems(item.ID)
		if cerr != nil {
			writeInternalError(w, cerr)
			return
		}
		// Cache a schema+settings done-context per child collection so
		// each child is evaluated against its own configured done field.
		ctxCache := make(map[string]doneContext)
		total, done := 0, 0
		for _, child := range children {
			if !isCollectionVisible(child.CollectionID, progVisIDs) {
				continue
			}
			if !s.isItemVisibleToGuest(r, workspaceID, &child, progFullCollIDs, progGrantedItemIDs) {
				continue
			}
			total++
			ctx, cached := ctxCache[child.CollectionID]
			if !cached {
				if coll, cerr := s.store.GetCollection(child.CollectionID); cerr == nil && coll != nil {
					_ = json.Unmarshal([]byte(coll.Schema), &ctx.schema)
					if coll.Settings != "" {
						_ = json.Unmarshal([]byte(coll.Settings), &ctx.settings)
					}
				}
				ctxCache[child.CollectionID] = ctx
			}
			// Build a one-entry ctx map for isItemDone so it hits the
			// typed-context branch rather than the status-only fallback.
			if isItemDone(child.Fields, child.CollectionID, map[string]doneContext{child.CollectionID: ctx}) {
				done++
			}
		}
		pct := 0
		if total > 0 {
			pct = (done * 100) / total
		}
		writeJSON(w, http.StatusOK, map[string]int{
			"total":      total,
			"done":       done,
			"percentage": pct,
		})
		return
	}

	total, done, err := s.store.GetItemProgress(item.ID)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	pct := 0
	if total > 0 {
		pct = (done * 100) / total
	}

	writeJSON(w, http.StatusOK, map[string]int{
		"total":      total,
		"done":       done,
		"percentage": pct,
	})
}

// resolveParentFilter extracts a "parent" (or "plan") key from the field
// filters and converts it to a ParentLinkID filter (which uses item_links instead of json_extract).
// An optional schema can be passed; if the schema defines a field with the key,
// that key is left as a normal field filter instead of being treated as a parent link.
func (s *Server) resolveParentFilter(r *http.Request, workspaceID string, params *models.ItemListParams, schemas ...models.CollectionSchema) error {
	if params.Fields == nil {
		return nil
	}

	// Accept both "parent" and "plan" as parent filter keys
	// but skip if the schema defines a real field with that key
	var schema *models.CollectionSchema
	if len(schemas) > 0 {
		schema = &schemas[0]
	}
	var val string
	for _, key := range []string{"parent", "plan"} {
		if schema != nil && schemaHasField(*schema, key) {
			continue
		}
		if v, ok := params.Fields[key]; ok && v != "" {
			val = v
			delete(params.Fields, key)
			break
		}
	}
	if val == "" {
		return nil
	}

	// Resolve slug/ref to UUID
	var resolved *models.Item
	if !isUUID(val) {
		var rerr error
		resolved, rerr = s.store.ResolveItem(workspaceID, val)
		if rerr != nil || resolved == nil {
			return fmt.Errorf("parent %q not found", val)
		}
		params.ParentLinkID = resolved.ID
	} else {
		params.ParentLinkID = val
		resolved, _ = s.store.GetItem(val)
	}

	// Ensure the parent is visible — return the same not-found error for
	// hidden parents so restricted users can't probe hidden item existence.
	if resolved != nil {
		visIDs, verr := s.visibleCollectionIDs(r, workspaceID)
		if verr != nil {
			return verr
		}
		if !isCollectionVisible(resolved.CollectionID, visIDs) {
			return fmt.Errorf("parent %q not found", val)
		}
	}

	return nil
}

// schemaHasField returns true if the collection schema defines a field with the given key.
func schemaHasField(schema models.CollectionSchema, key string) bool {
	for _, f := range schema.Fields {
		if f.Key == key {
			return true
		}
	}
	return false
}

// checkUniqueFields enforces FieldDef.UniqueScope == "workspace_collection"
// for the given collection. For each schema field with that scope, any
// non-empty string value in fieldMap is checked against existing items in
// the same collection; a match returns a conflict error. excludeItemID
// allows update flows to ignore the item being updated.
//
// Only string-typed unique values are supported today (the only consumer is
// playbooks.invocation_slug). Non-string values are skipped without error.
func (s *Server) checkUniqueFields(workspaceID, collectionID, excludeItemID string, schema models.CollectionSchema, fieldMap map[string]any) error {
	for _, def := range schema.Fields {
		if def.UniqueScope != "workspace_collection" {
			continue
		}
		raw, ok := fieldMap[def.Key]
		if !ok || raw == nil {
			continue
		}
		val, ok := raw.(string)
		if !ok || val == "" {
			continue
		}
		// IncludeArchived stays false so the application-layer check stays
		// consistent with the partial unique index's `deleted_at IS NULL`
		// predicate. A soft-deleted playbook releases its slug back to the
		// pool; trying to reclaim it should succeed, not 409.
		existing, err := s.store.ListItems(workspaceID, models.ItemListParams{
			CollectionIDs: []string{collectionID},
			Fields:        map[string]string{def.Key: val},
			Limit:         2,
		})
		if err != nil {
			return err
		}
		for _, item := range existing {
			if item.ID == excludeItemID {
				continue
			}
			return fmt.Errorf("field %q value %q is already used by item %s", def.Key, val, item.Slug)
		}
	}
	return nil
}

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

// maxItemListQueryLimit is the hard-max clamp applied to a caller-supplied
// `?limit=` on the item-list endpoints (TASK-2000). It matches the CLI's
// maxItemListLimit so both layers agree on the ceiling. A zero/absent limit is
// left unbounded — this only clamps an explicit oversized request.
const maxItemListQueryLimit = 1000

// parseItemListParams extracts item list parameters from the request query string.
func parseItemListParams(r *http.Request) models.ItemListParams {
	params := models.ItemListParams{
		Sort:           r.URL.Query().Get("sort"),
		GroupBy:        r.URL.Query().Get("group_by"),
		Search:         r.URL.Query().Get("search"),
		ParentID:       r.URL.Query().Get("parent_id"),
		Tag:            r.URL.Query().Get("tag"),
		AssignedUserID: r.URL.Query().Get("assigned_user_id"),
		AgentRoleID:    r.URL.Query().Get("agent_role_id"),
	}

	if r.URL.Query().Get("include_archived") == "true" {
		params.IncludeArchived = true
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}
	// Hard-max backstop (TASK-2000): clamp an explicit oversized limit so a
	// raw `?limit=999999` against this endpoint can't materialize the whole
	// workspace. No default is applied here — a zero/absent limit stays
	// unbounded so the many internal callers of store.ListItems (dashboard,
	// bootstrap, playbooks, …) that intentionally fetch every row are
	// untouched. The CLI applies its own default before it ever reaches here.
	if params.Limit > maxItemListQueryLimit {
		params.Limit = maxItemListQueryLimit
	}
	if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
		if o, err := strconv.Atoi(offsetStr); err == nil {
			params.Offset = o
		}
	}

	// non_terminal=true restricts results to items that are NOT in their
	// collection's terminal state, resolved per-collection from each
	// schema's terminal_options (BUG-2001). This is the CLI/MCP default
	// list behavior; an explicit status/field filter is applied on top.
	if r.URL.Query().Get("non_terminal") == "true" {
		params.NonTerminal = true
	}

	// Extract field filters: any query param that isn't a known param is a field filter.
	knownParams := map[string]bool{
		"sort": true, "group_by": true, "search": true, "parent_id": true,
		"tag": true, "include_archived": true, "limit": true, "offset": true,
		"assigned_user_id": true, "agent_role_id": true, "non_terminal": true,
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

// handleListItemActivity returns the activity feed for a specific item.
func (s *Server) handleListItemActivity(w http.ResponseWriter, r *http.Request) {
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

	params := models.ActivityListParams{
		Action: r.URL.Query().Get("action"),
		Actor:  r.URL.Query().Get("actor"),
		Source: r.URL.Query().Get("source"),
	}
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil {
			params.Limit = l
		}
	}

	activities, err := s.store.ListDocumentActivity(item.ID, params)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if activities == nil {
		activities = []models.Activity{}
	}

	writeJSON(w, http.StatusOK, activities)
}
