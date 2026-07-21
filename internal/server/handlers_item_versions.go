package server

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
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
		//
		// Postgres-only commit-outcome reconciliation (BUG-2276 residual 1): on
		// Postgres a commit that DURABLY lands but whose ack is lost at the
		// connection boundary surfaces to ForceRefreshRoom as an error; supply a
		// reconcile callback that re-reads to distinguish that from a genuine
		// rollback. On SQLite a commit error is unambiguous, so we pass nil and keep
		// the verbatim rollback-on-error path. `content` is the exact restored
		// version content.
		//
		// baselineSeq is the pre-restore item.seq the reconcile compares
		// last_restore_seq against. It is captured INSIDE the commit's precheck —
		// i.e. UNDER ForceRefreshRoom's per-item lock + the workspace seq lock,
		// immediately before the mutation — NOT from the pre-lock `item.Seq` read at
		// the top of the handler. A restore that completed while this request waited
		// for the per-item lock would otherwise leave its advanced last_restore_seq
		// visible against a stale baseline and make reconcile falsely classify a
		// genuine rollback of THIS attempt as landed (BUG-2276 P2).
		var (
			baselineSeq      int64
			baselineCaptured bool
			reconcile        func() (collab.RestoreReconcileResult, error)
		)
		if s.store.D().Driver() == store.DriverPostgres {
			reconcile = func() (collab.RestoreReconcileResult, error) {
				res, fresh, rerr := s.reconcileRestoreCommit(item.ID, content, baselineSeq, baselineCaptured)
				if rerr == nil && res.Landed {
					// Reconciled LANDED despite the lost ack: surface the freshly-read
					// restored item so the handler returns it AND emits the item SSE
					// event, instead of the false 404 it would hit with updated==nil
					// (the commit's UpdateItemWithPreCheck returned (nil, ackErr) on this
					// path). BUG-2276 P2.
					updated = fresh
				}
				return res, rerr
			}
		}
		werr := s.collab.ForceRefreshRoom(item.ID, func() (int64, int64, error) {
			var maxID int64
			u, uerr := s.store.UpdateItemWithPreCheck(item.ID, input,
				func(tx *sql.Tx, existing *models.Item) error {
					// Capture the pre-restore seq under the per-item + workspace seq
					// lock, before any mutation (BUG-2276 P2 — see the baselineSeq note
					// above). `existing` is the row as read at the top of the update tx.
					baselineSeq = existing.Seq
					baselineCaptured = true
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
			if s.restoreAckFault != nil {
				if fe := s.restoreAckFault(); fe != nil {
					// TEST SEAM (BUG-2276 residual 1): the tx above committed DURABLY,
					// but return an error exactly as UpdateItemWithPreCheck would on a
					// lost ack — and do NOT set `updated`, so reconcile must recover the
					// restored item from the durable state.
					return 0, 0, fe
				}
			}
			updated = u
			// (pre-prune MAX for the stale-flush boundary, restored seq for the
			// content generation Join uses to force_refresh stale-seeded peers).
			return maxID, u.Seq, nil
		}, reconcile)
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

// reconcileRestoreCommit re-reads an item after a version-restore commit reported
// an error, to determine whether the restore's transaction DURABLY landed anyway
// (a Postgres commit whose ack was lost at the connection boundary — BUG-2276
// residual 1). It is supplied to ForceRefreshRoom ONLY on Postgres; SQLite passes
// nil, keeping the unambiguous rollback-on-error behavior.
//
// A restore's defining durable effects, all written in ONE tx, are: items.content
// = the target version's content, items.last_restore_seq = the restore's new
// item.seq (strictly greater than the pre-restore seq), and
// items.restore_boundary_op_id = pre-prune MAX(op-log.id)+1. We read all three and
// require the two INDEPENDENT signals to AGREE:
//
//   - contentMatches: fresh items.content == the exact restored version content
//     (a restore sets content to an EXACT prior version).
//   - seqAdvanced:    items.last_restore_seq > baselineSeq (the pre-restore seq
//     captured UNDER the per-item lock, immediately before the tx). last_restore_seq
//     is written ONLY by restores, which are serialised under the collab per-item
//     lock, so during this reconcile it holds either the pre-restore value (rolled
//     back) or this restore's new seq (landed) — never a concurrent writer's. Gated
//     on baselineCaptured: if the tx failed before the precheck ran (nothing
//     mutated), the baseline is unknown, so seqAdvanced is forced false.
//
// Outcomes:
//   - Both true  → LANDED (recover Boundary = restore_boundary_op_id and Seq =
//     last_restore_seq so ForceRefreshRoom's success path publishes the same fences
//     it would have on a clean commit, and return the freshly-read item so the
//     caller can respond with it + emit the SSE event).
//   - Both false → DEFINITELY rolled back (un-freeze, the genuine-rollback path);
//     returns a nil item.
//   - Disagree, read error, or NOT-FOUND → ambiguous; return an error so
//     ForceRefreshRoom keeps the room frozen and plain-closes it (the safe degraded
//     mode). A not-found re-read is UNCERTAIN, NOT rolled-back: the restore may have
//     durably landed and a concurrent archive then soft-deleted the item — treating
//     that as rolled-back would un-freeze stale peers onto the archived item and let
//     them poison its op-log for a later unarchive (BUG-2276 P1). Also covers the
//     rare restore-to-identical-content tx that ALSO ack-lost (contentMatches
//     coincidentally true while seqAdvanced is false): staying frozen is safe.
func (s *Server) reconcileRestoreCommit(itemID, targetContent string, baselineSeq int64, baselineCaptured bool) (collab.RestoreReconcileResult, *models.Item, error) {
	fresh, err := s.store.GetItem(itemID)
	if err != nil {
		return collab.RestoreReconcileResult{}, nil, fmt.Errorf("reconcile restore: read item: %w", err)
	}
	if fresh == nil {
		// NOT-FOUND is UNCERTAIN, not rolled-back (BUG-2276 P1): a durable restore
		// could have landed and a concurrent archive then soft-deleted the item.
		// Return an error → ForceRefreshRoom stays frozen + plain-closes, so no stale
		// peer resumes onto the archived item's op-log.
		return collab.RestoreReconcileResult{}, nil, fmt.Errorf("reconcile restore: item %s not found on re-read (archived mid-restore?)", itemID)
	}
	lastRestoreSeq, lrOK, err := s.store.ItemLastRestoreSeq(itemID)
	if err != nil {
		return collab.RestoreReconcileResult{}, nil, fmt.Errorf("reconcile restore: read last_restore_seq: %w", err)
	}
	boundaryOpID, bOK, err := s.store.ItemRestoreBoundaryOpID(itemID)
	if err != nil {
		return collab.RestoreReconcileResult{}, nil, fmt.Errorf("reconcile restore: read restore_boundary_op_id: %w", err)
	}

	contentMatches := fresh.Content == targetContent
	seqAdvanced := baselineCaptured && lrOK && lastRestoreSeq > baselineSeq
	switch {
	case contentMatches && seqAdvanced && bOK:
		return collab.RestoreReconcileResult{Landed: true, Boundary: boundaryOpID, Seq: lastRestoreSeq}, fresh, nil
	case !contentMatches && !seqAdvanced:
		return collab.RestoreReconcileResult{Landed: false}, nil, nil
	default:
		return collab.RestoreReconcileResult{}, nil, fmt.Errorf(
			"reconcile restore: ambiguous outcome (contentMatches=%v seqAdvanced=%v boundaryStamped=%v baselineCaptured=%v)",
			contentMatches, seqAdvanced, bOK, baselineCaptured)
	}
}
