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

	for _, a := range orphans {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		// "Never-attached" rows (item_id IS NULL, deleted_at IS NULL)
		// can still be referenced from item content via
		// `pad-attachment:UUID` — the editor uploads first, then
		// PATCHes content with the reference, but the attachments
		// row's item_id stays NULL. Scan items.content + items.fields
		// before reclaiming so the GC doesn't destroy a legitimate
		// reference. Codex P1 on PR #307 round 1.
		if a.ItemID == nil && a.DeletedAt == nil {
			referenced, err := s.store.AttachmentReferencedInItems(a.WorkspaceID, a.ID)
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

		// Decide whether the on-disk blob can also be reclaimed.
		// Two protections to consider:
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
		s.inFlightHashesMu.Lock()
		inFlight := s.inFlightHashes[a.ContentHash] > 0
		if others == 0 && !inFlight {
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
				// here is a real failure (permission, IO, etc.).
				// Still drop the DB row — keeping it strands the
				// row indefinitely; the operator will have to clean
				// the disk by hand either way.
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
		}

		if err := s.store.HardDeleteAttachment(a.ID); err != nil {
			slog.Warn("orphan GC: hard delete failed",
				"attachment_id", a.ID, "error", err)
			res.Skipped++
			continue
		}
		res.Deleted++
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
