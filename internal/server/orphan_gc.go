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
	// generosity. The window is also the bound on residual (2) in
	// stampAttachmentRefsTx's ORDERING note: a writer transaction whose
	// stamp-to-commit span exceeds it loses protection, which is why
	// "longest plausible stalled writer transaction" is the sizing
	// criterion and not a soft target. If you are tempted to lower this
	// for faster tests, inject the cutoff in the test instead; the
	// constant guards production correctness.
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
	// BlobDeleteFailures counts rows whose DB row was claimed but whose
	// blob delete then failed — stranded bytes an operator must clean by
	// hand. Surfaced as its own counter so a sweep summary can't read
	// "deleted=N, blobs_reclaimed=0, skipped=0" as if nothing went wrong
	// (BUG-2415 codex round 3).
	BlobDeleteFailures int
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
		// LIVE variant whose parent is tombstoned/gone (BUG-2388): the
		// leak artifact of the old non-transactional delete cascade, and
		// the retro-cleanup class for rows already leaked. Tried FIRST
		// for live parented rows — an item_id-NULL variant of a DEAD
		// parent would otherwise fall into the never-attached scan,
		// where a lingering content reference to that dead parent could
		// keep the leaked row alive forever. The claim's own predicate
		// re-asserts parent-not-live at delete time, so a concurrent
		// parent restore refuses it — and a refusal simply means the
		// parent is live, in which case the row falls through to the
		// ordinary classes below (e.g. a live orphan upload's thumbnail
		// stays governed by the parent-aware never-attached path).
		claimedAsOrphanedVariant := false
		if a.DeletedAt == nil && a.ParentID != nil && *a.ParentID != "" {
			var claimErr error
			claimedAsOrphanedVariant, claimErr = s.store.ClaimOrphanedVariantAttachment(a.ID)
			if claimErr != nil {
				slog.Warn("orphan GC: orphaned-variant claim failed",
					"attachment_id", a.ID, "error", claimErr)
				res.Skipped++
				continue
			}
		}

		neverAttached := a.ItemID == nil && a.DeletedAt == nil
		if !claimedAsOrphanedVariant && neverAttached {
			// Variants (thumbnails) are never text-referenced by their
			// OWN id — content references the original. Scan the parent's
			// id for them, or a referenced upload would keep its original
			// while the sweep destroyed its thumbnails (codex round 1 #4;
			// pre-existing hole, closed alongside the claim protocol).
			scanID := a.ID
			if a.ParentID != nil && *a.ParentID != "" {
				scanID = *a.ParentID
			}
			referenced, err := s.store.AttachmentReferenced(a.WorkspaceID, scanID)
			if err != nil {
				slog.Warn("orphan GC: ref-scan failed",
					"attachment_id", a.ID, "workspace_id", a.WorkspaceID, "error", err)
				res.Skipped++
				continue
			}
			if referenced {
				// Some live content — item content or fields, a comment
				// body, or a document body (BUG-2614) — references the
				// attachment, so leave it alone. Bonus side effect: the
				// row will be picked up next sweep if the reference goes
				// away.
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
		switch {
		case claimedAsOrphanedVariant:
			claimed = true
		case neverAttached:
			claimed, claimErr = s.store.ClaimNeverAttachedAttachment(
				a.ID, time.Now().Add(-orphanGCRefStaleWindow))
		case a.DeletedAt != nil:
			claimed, claimErr = s.store.ClaimSoftDeletedAttachment(a.ID, graceCutoff)
		default:
			// Live, parented, parent turned out to be LIVE (restored
			// between the candidate SELECT and the variant claim), and
			// attached (non-nil item_id) — nothing reclaimable about it.
			res.Skipped++
			continue
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
				res.BlobDeleteFailures++
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

// rowlessBlobSweepResult records what one rowless-blob sweep (BUG-2406)
// accomplished, mirroring orphanGCResult's role for the row-driven sweep.
type rowlessBlobSweepResult struct {
	Listed         int   // blobs enumerated across all Lister-capable backends
	Reclaimed      int   // blobs deleted (no row in any state, past grace, not in flight)
	BytesReclaimed int64 // sum of reclaimed blob sizes
	Skipped        int   // candidates skipped due to mid-sweep errors
	Failures       int   // backends whose listing failed outright
}

// runRowlessBlobSweep reclaims blobs that NO attachments row references —
// the leak class the row-driven sweep above cannot see (BUG-2406). Every
// write path calls AttachmentStore.Put BEFORE inserting the row, so a
// failure (or crash) between the two leaves a blob on disk with nothing
// pointing at it; being row-driven, runOrphanGCSweep never scans it.
//
// Candidate = enumerated blob whose content hash has ZERO rows in ANY
// state AND whose ModTime predates blobCutoff. Both halves matter:
//
//   - ANY state: a soft-deleted row — even one already past its own
//     grace — still owns its bytes under the row sweep's row-before-bytes
//     claim protocol (BUG-2415); reaching around it here would recreate
//     the stranded-row-without-bytes state that protocol prevents. Rows
//     and their blobs are the row sweep's business; this sweep takes only
//     what no row claims.
//   - ModTime age: a young rowless blob is indistinguishable from an
//     upload whose row insert hasn't happened yet — rowlessness is the
//     NORMAL transient state of every in-progress upload. The cutoff is
//     the same operator-configured GC grace the row sweep uses (30d
//     default), orders of magnitude beyond any plausible Put-to-insert
//     gap; the in-flight guard below covers the active window, age is
//     the backstop for windows the process didn't live to observe.
//
// TOCTOU at delete time: a writer could commit a row for a candidate
// hash after the batched subtraction ran. Every Put-then-insert path
// runs inside markUploadInFlight (its documented contract), and the copy
// path's row clones only reference hashes that already have a live
// source row (so they were never candidates) — which leaves exactly two
// interleavings for a fresh writer, both closed under inFlightHashesMu:
// it marked before we locked (in-flight check skips), or it marks after
// we delete (its Put finds the file missing and rewrites it — Put is
// create-if-absent by contract). Between those sits the writer that
// marked, inserted, and RELEASED entirely inside our check-to-lock gap:
// in-flight is zero again but its row exists, which is what the row
// RE-CHECK inside the critical section catches. The mutex is held across
// re-check + Delete, same ms-class discipline the row sweep documents.
//
// Backends without the Lister capability are skipped, with a
// once-per-process notice (silent skipping would hide that this leak
// class is unguarded there). Cost on capable backends: one full listing
// + chunked hash-subtraction queries per sweep, O(blobs) — bounded by
// the GC cadence (24h default), never on a request path.
func (s *Server) runRowlessBlobSweep(ctx context.Context, blobCutoff time.Time) (*rowlessBlobSweepResult, error) {
	if s.attachments == nil {
		return nil, errors.New("attachments registry not configured")
	}
	res := &rowlessBlobSweepResult{}

	for prefix, backend := range s.attachments.Backends() {
		lister, ok := backend.(attachments.Lister)
		if !ok {
			s.rowlessNoListerOnce.Do(func() {
				slog.Info("rowless-blob sweep: backend has no Lister capability; rowless blobs on it are not reclaimed",
					"backend", prefix)
			})
			continue
		}
		blobs, err := lister.ListBlobs(ctx)
		if err != nil {
			slog.Warn("rowless-blob sweep: listing failed", "backend", prefix, "error", err)
			res.Failures++
			continue
		}
		res.Listed += len(blobs)

		const chunkSize = 400
		for start := 0; start < len(blobs); start += chunkSize {
			if err := ctx.Err(); err != nil {
				return res, err
			}
			chunk := blobs[start:min(start+chunkSize, len(blobs))]
			hashes := make([]string, len(chunk))
			for i, b := range chunk {
				hashes[i] = b.Hash
			}
			withRows, err := s.store.AttachmentHashesWithRows(hashes)
			if err != nil {
				slog.Warn("rowless-blob sweep: hash subtraction failed", "backend", prefix, "error", err)
				res.Skipped += len(chunk)
				continue
			}
			for _, b := range chunk {
				if err := ctx.Err(); err != nil {
					return res, err
				}
				if withRows[b.Hash] || !b.ModTime.Before(blobCutoff) {
					continue
				}

				s.inFlightHashesMu.Lock()
				if s.inFlightHashes[b.Hash] > 0 {
					s.inFlightHashesMu.Unlock()
					continue
				}
				if s.rowlessPreDeleteHook != nil {
					s.rowlessPreDeleteHook(b.Hash)
				}
				exists, err := s.store.AttachmentRowsExistForHash(b.Hash)
				if err != nil {
					s.inFlightHashesMu.Unlock()
					slog.Warn("rowless-blob sweep: delete-time row re-check failed",
						"backend", prefix, "hash", b.Hash, "error", err)
					res.Skipped++
					continue
				}
				if exists {
					s.inFlightHashesMu.Unlock()
					continue
				}
				delErr := backend.Delete(ctx, b.Key)
				s.inFlightHashesMu.Unlock()
				if delErr != nil {
					slog.Warn("rowless-blob sweep: blob delete failed",
						"backend", prefix, "storage_key", b.Key, "error", delErr)
					res.Skipped++
					continue
				}
				res.Reclaimed++
				res.BytesReclaimed += b.Size
			}
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

// orphanGCGraceConfigured returns the effective grace period —
// operator-configured when set, the default otherwise. Read under the
// config mutex; used by the thumbnail refusal cleanup so its hash-
// protection window honors SetOrphanGCConfig (BUG-2388 codex round 1).
func (s *Server) orphanGCGraceConfigured() time.Duration {
	s.orphanGC.mu.Lock()
	defer s.orphanGC.mu.Unlock()
	if s.orphanGC.grace > 0 {
		return s.orphanGC.grace
	}
	return defaultOrphanGCGrace
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
		"skipped", res.Skipped,
		"blob_delete_failures", res.BlobDeleteFailures)

	// The rowless-blob sweep runs AFTER the row sweep, same tick and same
	// grace cutoff: rows the row sweep just claimed are gone, so their
	// now-unreferenced blobs age normally into the NEXT tick's rowless
	// candidates rather than racing this one.
	rres, err := s.runRowlessBlobSweep(ctx, cutoff)
	if err != nil {
		slog.Warn("rowless-blob sweep failed", "error", err)
		return
	}
	slog.Info("rowless-blob sweep",
		"listed", rres.Listed,
		"reclaimed", rres.Reclaimed,
		"bytes_reclaimed", rres.BytesReclaimed,
		"skipped", rres.Skipped,
		"backend_failures", rres.Failures)
}

// _ keeps the attachments import alive even if every callsite ends
// up only touching s.store — the storage-backend Resolve call lives
// inside runOrphanGCSweep regardless.
var _ = attachments.ErrNotFound
