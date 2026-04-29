package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/store"
)

// storageInfoTTL is the cache lifetime for workspace storage usage
// summaries. Settings → Storage hits the endpoint on every page load,
// and other surfaces (admin user-detail, soon dashboard widgets) can
// poll it too — caching for a short window collapses repeated SUMs +
// owner lookups to a single DB round-trip per workspace.
//
// 30s is short enough that a freshly uploaded file appears in the bar
// within one refresh cycle even without explicit invalidation, and
// long enough to absorb a burst of reloads from the same client.
const storageInfoTTL = 30 * time.Second

// storageInfoCache memoizes WorkspaceStorageInfo results keyed by
// workspace ID. Invalidation is implicit via TTL expiry rather than
// coupling the upload/delete paths to a cache invalidation callback —
// the eventual-consistency window is bounded by storageInfoTTL.
type storageInfoCache struct {
	mu      sync.RWMutex
	entries map[string]storageInfoCacheEntry
	ttl     time.Duration
	now     func() time.Time // injected for tests; nil falls back to time.Now
}

type storageInfoCacheEntry struct {
	info      *store.WorkspaceStorageInfo
	expiresAt time.Time
}

func newStorageInfoCache(ttl time.Duration) *storageInfoCache {
	return &storageInfoCache{
		entries: make(map[string]storageInfoCacheEntry),
		ttl:     ttl,
	}
}

func (c *storageInfoCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// get returns a cached entry if present and unexpired. A nil result
// means the caller must recompute and call set.
func (c *storageInfoCache) get(workspaceID string) *store.WorkspaceStorageInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.entries[workspaceID]
	if !ok {
		return nil
	}
	if c.clock().After(e.expiresAt) {
		return nil
	}
	// Defensive copy: callers may mutate the returned pointer (e.g.
	// admin-only fields stripped before write). Without this, a single
	// admin-stripped read could poison the cache for non-admin readers.
	cp := *e.info
	return &cp
}

func (c *storageInfoCache) set(workspaceID string, info *store.WorkspaceStorageInfo) {
	if info == nil {
		return
	}
	cp := *info
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[workspaceID] = storageInfoCacheEntry{
		info:      &cp,
		expiresAt: c.clock().Add(c.ttl),
	}
}

// invalidate clears the cached entry for a workspace. Call after any
// mutation that changes used_bytes (uploads, deletes, transforms) so
// the next GET sees the fresh value without waiting for TTL expiry.
func (c *storageInfoCache) invalidate(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, workspaceID)
}

// handleGetWorkspaceStorageUsage returns the consolidated quota summary
// for the workspace: used bytes, effective limit, the owner's plan,
// and whether a per-user override is configured.
//
// GET /api/v1/workspaces/{ws}/storage/usage
//
// Auth: viewer+. RequireWorkspaceAccess admits item-grant guests
// with workspaceRole=="guest", who shouldn't see workspace-wide
// quota numbers — requireMinRole("viewer") is the explicit gate that
// matches the activity / members / versions read endpoints.
//
// Response: {used_bytes, limit_bytes, plan, override_active}.
//
// limit_bytes == -1 means unlimited (pro / self-hosted plans, or
// workspaces with no owner). The Settings → Storage UI uses this to
// decide whether to render a usage bar (capped) or a counter
// ("3.2 GB used") with no maximum.
func (s *Server) handleGetWorkspaceStorageUsage(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	if cached := s.storageInfoCache.get(workspaceID); cached != nil {
		writeJSON(w, http.StatusOK, cached)
		return
	}

	info, err := s.store.WorkspaceStorageInfo(workspaceID)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	s.storageInfoCache.set(workspaceID, info)
	writeJSON(w, http.StatusOK, info)
}

// handleListWorkspaceAttachments returns a paginated list of original
// (non-derived) attachments in the workspace. Supports filter +
// sort + pagination via query string:
//
//	GET /api/v1/workspaces/{ws}/attachments
//	    ?category=image|video|audio|document|text|archive|other
//	    &item=attached|unattached
//	    &collection=<collection_id>
//	    &sort=size|size_desc|filename|filename_desc|created_at|created_at_desc
//	    &limit=<1..200>
//	    &offset=<n>
//
// Unknown values are silently ignored — the server defaults
// (`created_at_desc`, limit 50, offset 0, no filters) take over.
//
// Auth: viewer+. Same gate as storage/usage — workspace-wide
// attachment metadata leaks the same surface area.
//
// Response: {attachments: [...], total: N, limit, offset}.
func (s *Server) handleListWorkspaceAttachments(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "viewer") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	q := r.URL.Query()
	filters := store.AttachmentListFilters{
		MimeCategory: strings.ToLower(strings.TrimSpace(q.Get("category"))),
		CollectionID: strings.TrimSpace(q.Get("collection")),
		Sort:         strings.ToLower(strings.TrimSpace(q.Get("sort"))),
	}
	switch strings.ToLower(strings.TrimSpace(q.Get("item"))) {
	case "attached":
		filters.Attached = true
	case "unattached":
		filters.Unattached = true
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filters.Limit = n
		}
	}
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filters.Offset = n
		}
	}

	rows, total, err := s.store.WorkspaceAttachments(workspaceID, filters)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if rows == nil {
		// Marshal `[]` rather than `null` so the UI can iterate
		// without a falsy-check guard on every render.
		rows = []store.AttachmentListItem{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"attachments": rows,
		"total":       total,
		"limit":       effectiveLimit(filters.Limit),
		"offset":      effectiveOffset(filters.Offset),
	})
}

// effectiveLimit / effectiveOffset mirror the store-side defaults so
// the response carries the canonical values the handler used. Useful
// to the UI when the request omitted them.
func effectiveLimit(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

func effectiveOffset(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// handleDeleteWorkspaceAttachment soft-deletes an attachment by ID.
// Tombstones the row + every thumbnail variant; the orphan GC reclaims
// the on-disk blob after the grace period (TASK-886).
//
//	DELETE /api/v1/workspaces/{ws}/attachments/{attachmentID}
//
// Auth: editor+. Delete is destructive (the bytes go away after GC) —
// view-only members shouldn't be able to remove attachments other
// users uploaded.
//
// Cross-workspace requests get 404 (not 403) to avoid leaking which
// IDs exist in other workspaces. Same pattern as the download
// handler.
//
// Returns 204 on success. The storage-usage cache is invalidated
// eagerly so the bar drops within a refresh cycle.
func (s *Server) handleDeleteWorkspaceAttachment(w http.ResponseWriter, r *http.Request) {
	if !requireMinRole(w, r, "editor") {
		return
	}
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "attachmentID")
	if id == "" {
		writeError(w, http.StatusBadRequest, "missing_id", "Attachment ID required")
		return
	}

	att, err := s.store.GetAttachment(id)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if att == nil || att.WorkspaceID != workspaceID || att.DeletedAt != nil {
		writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
		return
	}
	// Refuse to delete derived (thumbnail) rows directly — they're
	// auto-managed and deleting the original cascades. A direct
	// delete here would leave the original without thumbnails until
	// a future "regenerate" job runs.
	if att.ParentID != nil {
		writeError(w, http.StatusBadRequest, "derived_attachment",
			"Cannot delete a thumbnail directly — delete the original.")
		return
	}

	if err := s.store.SoftDeleteAttachment(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "not_found", "Attachment not found")
			return
		}
		writeInternalError(w, err)
		return
	}

	s.storageInfoCache.invalidate(workspaceID)

	w.WriteHeader(http.StatusNoContent)
}
