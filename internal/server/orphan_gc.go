package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/attachments"
)

// Default GC parameters. Operators override via env vars wired in
// cmd/pad/main.go (PAD_ORPHAN_GC_INTERVAL / PAD_ORPHAN_GC_GRACE).
const (
	defaultOrphanGCInterval = 24 * time.Hour
	defaultOrphanGCGrace    = 30 * 24 * time.Hour

	// orphanGCRefStaleWindow is a CORRECTNESS parameter, not a tuning
	// knob (BUG-2415). A never-attached row is only claimable when its
	// last_referenced_at stamp is older than this window.
	//
	// What it must cover: the stamp is NOT a lease on long-lived
	// references — those are caught by the content LIKE scan
	// (AttachmentReferenced) that runs immediately before the claim.
	// The stamp only has to cover references that commit AFTER that
	// scan, i.e. within the scan→claim gap of a single sweep iteration
	// (milliseconds) PLUS the full duration of the writer transaction
	// that carries the stamp — including a pathologically stalled one
	// (lock waits, an operator-paused Postgres, a laptop suspending
	// mid-commit). 15 minutes exceeds any plausible single write
	// transaction by orders of magnitude while delaying reclamation of
	// a genuinely-orphaned row by at most one extra sweep against a
	// 30-day grace period — the asymmetry is entirely in favor of
	// generosity. If you are tempted to lower this for faster tests,
	// inject the cutoff in the test instead; the constant guards
	// production correctness.
	orphanGCRefStaleWindow = 15 * time.Minute
)

// orphanGCResult records what one sweep accomplished. Returned from
// runOrphanGCSweep so tests can assert on the counters and the
// periodic logger can summarize a run in one line.
type orphanGCResult struct {
	Scanned        int   // rows considered (matched the orphan SELECT)
	Deleted        int   // rows hard-deleted from the DB
	BlobsReclaimed int   // on-disk blobs Delete'd through the storage backend
	BytesReclaimed int64 // sum of size_bytes for reclaimed blobs
	Skipped        int   // rows skipped due to mid-sweep errors
}

// runOrphanGCSweep walks the orphaned-attachments query and reclaims
// rows past the grace period. Two reclamation paths:
//
//   - DB row only. content_hash is still referenced by another live
//     row (dedup hit). Drop the row, leave the blob on disk.
//   - DB row + blob. No other live row references the hash. Delete
//     the blob through the storage backend, then drop the row.
//
// Failures within a single row are logged and skipped — the sweep
// keeps making progress. A genuine catastrophic error (e.g. DB
// connection lost) returns up so the caller can decide whether to
// retry the whole sweep.
//
// Splitting this out from the periodic loop lets tests drive a
// single sweep deterministically. Pass a graceCutoff so tests can
// inject a known time without waiting for real elapsed grace.
func (s *Server) runOrphanGCSweep(ctx context.Context, graceCutoff time.Time) (*orphanGCResult, error) {
	if s.attachments == nil {
		return nil, errors.New("attachments registry not configured")
	}
	res := &orphanGCResult{}

	orphans, err := s.store.OrphanedAttachments(graceCutoff)
	if err != nil {
		return nil, err
	}
	res.Scanned = len(orphans)

	// Track hashes whose blob has already been deleted earlier in
	// this same sweep so we don't double-count. Without this, two
	// soft-deleted peers sharing a content_hash would both report
	// BlobsReclaimed=1 — AttachmentStore.Delete treats a missing
	// key as success, so the second row's Delete returns nil and
	// the counter increments again. Functional cleanup is correct
	// (idempotent); only the metric was wrong. Codex round 4.
	reclaimedThisSweep := make(map[string]bool)

	for _, a := range orphans {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// "Never-attached" rows (item_id IS NULL, deleted_at IS NULL)
		// can still be referenced from item content or a comment body
		// via `pad-attachment:UUID` — the editor / comment composer
		// upload first, then save the reference, but the attachments
		// row's item_id stays NULL. Scan items.content + items.fields
		// + comment bodies before reclaiming so the GC doesn't destroy
		// a legitimate reference. Codex P1 on PR #307 round 1;
		// comment-body coverage added for IDEA-1650.
		neverAttached := a.ItemID == nil && a.DeletedAt == nil
		if neverAttached {
			referenced, err := s.store.AttachmentReferenced(a.WorkspaceID, a.ID)
			if err != nil {
				slog.Warn("orphan GC: ref-scan failed",
					"attachment_id", a.ID, "workspace_id", a.WorkspaceID, "error", err)
				res.Skipped++
				continue
			}
			if referenced {
				// Item content references the attachment — leave it
				// alone. Bonus side effect: the row will be picked
				// up next sweep if the reference goes away.
				continue
			}
		}

		// CLAIM — row BEFORE bytes (BUG-2415). The row deletion is a
		// conditional DELETE that re-asserts the row's reclaimable
		// state inside the statement itself, so it serializes at the
		// database against every writer:
		//
		//   - never-attached: still unattached, still live, and no
		//     reference stamp fresher than the stale window. A writer
		//     committing a `pad-attachment:` reference stamps the row
		//     in the SAME transaction as the content
		//     (stampAttachmentRefsTx), so either the stamp lands first
		//     and this claim matches zero rows, or the claim lands
		//     first and the writer's stamp matches zero rows — no
		//     interleaving destroys a referenced row.
		//   - soft-deleted: deleted_at still set and still past grace
		//     at delete time, so a mid-sweep restore survives.
		//
		// Row-first ordering is what makes a surviving row imply
		// surviving bytes: the blob is only reclaimed after the row is
		// provably gone. (The old order deleted the blob first, so a
		// reference landing mid-sweep could keep a row whose content
		// was already destroyed — the worst failure mode of the race.)
		var claimed bool
		var claimErr error
		if neverAttached {
			claimed, claimErr = s.store.ClaimNeverAttachedAttachment(
				a.ID, time.Now().Add(-orphanGCRefStaleWindow))
		} else {
			claimed, claimErr = s.store.ClaimSoftDeletedAttachment(a.ID, graceCutoff)
		}
		if claimErr != nil {
			slog.Warn("orphan GC: claim failed",
				"attachment_id", a.ID, "error", claimErr)
			res.Skipped++
			continue
		}
		if !claimed {
			// The row's state changed since the candidate SELECT — a
			// writer referenced it, an upload attached it, or a restore
			// revived it. That is the claim protocol WORKING, not a
			// failure; the next sweep takes a fresh look.
			slog.Debug("orphan GC: claim refused — row state changed mid-sweep",
				"attachment_id", a.ID)
			res.Skipped++
			continue
		}
		res.Deleted++

		// Row is gone — decide whether the on-disk blob can also be
		// reclaimed. Two protections to consider:
		//
		//   1. content-addressed dedupe: another row at the same
		//      hash may still need the blob. CountProtecting includes
		//      both LIVE rows and soft-deleted rows still inside
		//      their own grace window — the latter case keeps the
		//      blob around for un-delete / inspection until each
		//      row's own grace lapses.
		//
		//   2. in-flight uploads: an upload that called
		//      AttachmentStore.Put but hasn't yet inserted its DB
		//      row. markUploadInFlight registers the hash before
		//      Put; we MUST observe that under the same mutex we
		//      use to gate blob deletion, otherwise a TOCTOU race
		//      between our check and store.Delete lets a new
		//      upload's Put land on a blob we're about to remove.
		//      Codex P1 round 3.
		others, err := s.store.CountProtectingAttachmentsForHash(a.ContentHash, a.ID, graceCutoff)
		if err != nil {
			slog.Warn("orphan GC: count protecting refs failed",
				"attachment_id", a.ID, "hash", a.ContentHash, "error", err)
			res.Skipped++
			continue
		}

		// Critical section: hold the in-flight mutex across the
		// uploadInFlight check AND the FS Delete so a concurrent
		// markUploadInFlight blocks until we either skip (because
		// it's in flight) or finish deleting. The lock window is
		// ms-class on FSStore; for S3 backends in Phase 2 a
		// per-hash lock will replace this server-wide mutex.
		blobDeleted := false
		alreadyReclaimed := reclaimedThisSweep[a.ContentHash]
		s.inFlightHashesMu.Lock()
		inFlight := s.inFlightHashes[a.ContentHash] > 0
		if others == 0 && !inFlight && !alreadyReclaimed {
			store, resolveErr := s.attachments.Resolve(a.StorageKey)
			if resolveErr != nil {
				slog.Warn("orphan GC: resolve backend failed",
					"attachment_id", a.ID, "storage_key", a.StorageKey, "error", resolveErr)
				s.inFlightHashesMu.Unlock()
				res.Skipped++
				continue
			}
			if delErr := store.Delete(ctx, a.StorageKey); delErr != nil {
				// AttachmentStore.Delete documents that deleting a
				// missing key is NOT an error, so anything reaching
				// here is a real failure (permission, IO, etc.). The
				// row is already claimed (deleted), so this strands
				// the blob on disk for the operator to clean by hand
				// — the inverse of the old order's failure mode, and
				// the safe direction: a stranded blob wastes disk, a
				// stranded row without bytes lied about data that no
				// longer existed (BUG-2415).
				slog.Warn("orphan GC: blob delete failed",
					"attachment_id", a.ID, "storage_key", a.StorageKey, "error", delErr)
			} else {
				blobDeleted = true
			}
		}
		s.inFlightHashesMu.Unlock()

		if blobDeleted {
			res.BlobsReclaimed++
			res.BytesReclaimed += a.SizeBytes
			reclaimedThisSweep[a.ContentHash] = true
		}
	}

	return res, nil
}

// orphanGCConfig captures runtime knobs for the periodic loop.
// Stored on Server via SetOrphanGCConfig so tests + cmd/pad can
// override defaults independently.
type orphanGCConfig struct {
	mu       sync.Mutex
	interval time.Duration
	grace    time.Duration
	stop     chan struct{}
	running  bool
}

// SetOrphanGCConfig overrides the default sweep interval (24h) and
// grace period (30d). Pass 0 for either to keep the package default.
// Must be called before StartOrphanGC.
func (s *Server) SetOrphanGCConfig(interval, grace time.Duration) {
	s.orphanGC.mu.Lock()
	defer s.orphanGC.mu.Unlock()
	if interval > 0 {
		s.orphanGC.interval = interval
	}
	if grace > 0 {
		s.orphanGC.grace = grace
	}
}

// StartOrphanGC kicks off the periodic sweep loop. Idempotent —
// calling twice is a no-op (existing loop continues, second call
// returns silently). Must be called AFTER SetAttachments; the loop
// no-ops sweeps when the registry isn't wired so a server without
// attachment storage doesn't log spurious errors.
//
// The loop is tracked by Server.bg so Stop() drains it before the
// process exits / SQLite is closed (BUG-842 invariant).
func (s *Server) StartOrphanGC() {
	s.orphanGC.mu.Lock()
	if s.orphanGC.running {
		s.orphanGC.mu.Unlock()
		return
	}
	if s.orphanGC.interval == 0 {
		s.orphanGC.interval = defaultOrphanGCInterval
	}
	if s.orphanGC.grace == 0 {
		s.orphanGC.grace = defaultOrphanGCGrace
	}
	s.orphanGC.stop = make(chan struct{})
	s.orphanGC.running = true
	interval := s.orphanGC.interval
	grace := s.orphanGC.grace
	stop := s.orphanGC.stop
	s.orphanGC.mu.Unlock()

	slog.Info("orphan GC started",
		"interval", interval.String(), "grace", grace.String())

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("orphan-gc") // BUG-2071
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runOrphanGCTick(grace)
			}
		}
	}()
}

// stopOrphanGC signals the loop to exit. Called from Server.Stop().
// Safe to call when the loop never started.
func (s *Server) stopOrphanGC() {
	s.orphanGC.mu.Lock()
	defer s.orphanGC.mu.Unlock()
	if !s.orphanGC.running {
		return
	}
	close(s.orphanGC.stop)
	s.orphanGC.running = false
}

// runOrphanGCTick is one tick of the periodic loop. Wrapped with a
// 30-minute cap on the sweep so a long-running scan can't pin the
// goroutine across multiple intervals. Logged at info on success,
// warn on failure.
func (s *Server) runOrphanGCTick(grace time.Duration) {
	if s.attachments == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cutoff := time.Now().UTC().Add(-grace)
	res, err := s.runOrphanGCSweep(ctx, cutoff)
	if err != nil {
		slog.Warn("orphan GC sweep failed", "error", err)
		return
	}
	slog.Info("orphan GC sweep",
		"scanned", res.Scanned,
		"deleted", res.Deleted,
		"blobs_reclaimed", res.BlobsReclaimed,
		"bytes_reclaimed", res.BytesReclaimed,
		"skipped", res.Skipped)
}

// _ keeps the attachments import alive even if every callsite ends
// up only touching s.store — the storage-backend Resolve call lives
// inside runOrphanGCSweep regardless.
var _ = attachments.ErrNotFound
