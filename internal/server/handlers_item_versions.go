package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// errRestoreItemGone signals that the item was concurrently deleted during a
// restore's transaction (the tx committed nothing). Used to distinguish a
// benign 404 from a genuine 500 out of ForceRefreshRoom's commit callback.
var errRestoreItemGone = errors.New("restore: item not found")

// handleListItemVersions returns all versions for an item with diffs resolved.
func (s *Server) handleListItemVersions(w http.ResponseWriter, r *http.Request) {
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

	versions, err := s.store.ListItemVersionsResolved(item.ID, item.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if versions == nil {
		versions = []models.Version{}
	}

	writeJSON(w, http.StatusOK, versions)
}

// handleGetItemVersion returns a single version with its diff resolved to full
// content. The paginated timeline serves raw reverse-patch text (it can't resolve
// a partial window), so the timeline card calls this to reconstruct real content
// when a diff version is expanded — see BUG-1612.
func (s *Server) handleGetItemVersion(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	versionID := chi.URLParam(r, "versionID")
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

	version, err := s.store.GetItemVersionResolved(item.ID, versionID, item.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}
	if version == nil {
		writeError(w, http.StatusNotFound, "not_found", "Version not found")
		return
	}

	writeJSON(w, http.StatusOK, version)
}

// handleRestoreItemVersion restores an item's content from a specific version.
func (s *Server) handleRestoreItemVersion(w http.ResponseWriter, r *http.Request) {
	workspaceID, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}

	itemSlug := chi.URLParam(r, "itemSlug")
	versionID := chi.URLParam(r, "versionID")

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

	// Get all resolved versions to find the target
	versions, err := s.store.ListItemVersionsResolved(item.ID, item.Content)
	if err != nil {
		writeInternalError(w, err)
		return
	}

	var targetVersion *models.Version
	for _, v := range versions {
		if v.ID == versionID {
			targetVersion = &v
			break
		}
	}
	if targetVersion == nil {
		writeError(w, http.StatusNotFound, "not_found", "Version not found")
		return
	}

	// Restore = prune + reseed (BUG-2264). A restore makes the old version's
	// content canonical and DISCARDS whatever peers are currently co-editing —
	// that is exactly restore semantics, so every peer must converge on the
	// restored content. When collab is configured, ForceRefreshRoom drives this
	// under the per-item lock: it freezes inbound persistence, runs the commit
	// (which writes items.content=restored + the "Restored from…" undo-point
	// version + prunes the ENTIRE op-log in ONE store transaction), publishes the
	// stale-flush boundary, then — if a live room exists — forces every connected
	// peer to discard its in-memory Y.Doc and rebuild from items.content. The
	// content write, the version, and the op-log wipe are atomic (Codex xhigh
	// [P1]): a failed commit rolls back all three, so there is no divergent
	// "restored content + stale op-log" or "wiped op-log + unchanged content"
	// state on any failure path. The pruned op-log means a reconnecting or cold
	// client lazy-seeds from the restored content; the boundary rejects any
	// in-flight pre-restore snapshot. This replaces the earlier
	// applier/epoch/watermark routing: prune+reseed needs none of it.
	content := targetVersion.Content
	summary := "Restored from version " + targetVersion.CreatedAt.Format("Jan 2, 2006 3:04 PM")
	input := models.ItemUpdate{
		Content:        &content,
		ChangeSummary:  summary,
		LastModifiedBy: "user",
		Source:         "web",
		// A restore must always leave an undo point + a version bracketing the
		// content it moves items.content back to, even on a repeat restore within
		// the version-throttle window (VersionThrottleInterval = 1h).
		// NOTE(BUG-2270): ForceVersion can mint same-second versions; the
		// item_versions ordering tie-breaker is tracked there.
		ForceVersion: true,
		// Stamp the DURABLE restore boundary (items.last_restore_seq) in the same
		// tx, so the Join stale-seed fence survives a server restart (BUG-2264).
		MarkRestoreBoundary: true,
	}

	var updated *models.Item
	if s.collab != nil {
		// Atomic capture + write + version + op-log prune in one tx: the precheck
		// hook reads the pre-prune MAX(op-log) AND wipes the op-log inside
		// UpdateItem's own transaction, so the restore boundary is captured
		// atomically (no out-of-tx fail-open) and a failed commit rolls back all of
		// it. ForceRefreshRoom then publishes the boundary (maxID+1) + content
		// generation (seq) and reseeds under the per-item lock.
		werr := s.collab.ForceRefreshRoom(item.ID, func() (int64, int64, error) {
			var maxID int64
			u, uerr := s.store.UpdateItemWithPreCheck(item.ID, input,
				func(tx *sql.Tx, _ *models.Item) error {
					m, _, merr := s.store.MaxOpLogIDTx(tx, item.ID)
					if merr != nil {
						return merr
					}
					maxID = m
					// Stamp the DURABLE op-log-id boundary (= pre-prune MAX+1) in
					// this same tx, so the collab-snapshot flush gate survives a
					// restart (residual #1's restore-boundary facet).
					if serr := s.store.StampRestoreBoundaryOpIDTx(tx, item.ID, m+1); serr != nil {
						return serr
					}
					return s.store.PruneItemOpLogTx(tx, item.ID)
				})
			if uerr != nil {
				return 0, 0, uerr
			}
			if u == nil {
				// Item vanished (concurrently deleted) — the tx committed nothing.
				// Fail so ForceRefreshRoom un-freezes without publishing a boundary
				// or reseeding; the handler surfaces it below.
				return 0, 0, errRestoreItemGone
			}
			updated = u
			// (pre-prune MAX for the stale-flush boundary, restored seq for the
			// content generation Join uses to force_refresh stale-seeded peers).
			return maxID, u.Seq, nil
		})
		if werr != nil {
			if errors.Is(werr, errRestoreItemGone) {
				writeError(w, http.StatusNotFound, "not_found", "Item not found")
				return
			}
			writeInternalError(w, werr)
			return
		}
	} else {
		// No collab configured (self-host build without the room manager): plain
		// direct write — no op-log, no live peers.
		u, uerr := s.store.UpdateItem(item.ID, input)
		if uerr != nil {
			writeInternalError(w, uerr)
			return
		}
		updated = u
	}
	if updated == nil {
		writeError(w, http.StatusNotFound, "not_found", "Item not found")
		return
	}

	// Emit event. Carry the post-update `seq` so SSE consumers
	// (PLAN-1343 / TASK-1358 — localIndex.classifySSEEvent) can
	// detect contiguity vs. gap rather than blindly falling back
	// to a generic /items-changes refetch.
	if s.events != nil {
		s.events.Publish(events.Event{
			Type:        "item_updated",
			WorkspaceID: workspaceID,
			Collection:  item.CollectionSlug,
			ItemID:      item.ID,
			Title:       item.Title,
			Seq:         updated.Seq,
		})
	}

	writeJSON(w, http.StatusOK, updated)
}
