package server

import (
	"net/http"
	"sync"
	"time"

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
