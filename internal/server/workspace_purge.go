package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Scheduled hard-purge of soft-deleted workspaces (TASK-1966).
//
// The published /privacy policy promises "personal account data and
// workspaces you solely own are removed from our live systems within 30
// days." Account deletion (DeleteAccountAtomic) and manual workspace
// deletion (DeleteWorkspace) only SOFT-delete a workspace — they stamp
// workspaces.deleted_at and leave every item/comment/attachment row in
// place indefinitely. This sweeper honors the promise: it hard-deletes
// soft-deleted workspaces past the retention window, cascading every
// child row (via store.PurgeWorkspaceData) and reclaiming attachment
// blobs through the storage abstraction (FS + S3 safe).
//
// Modeled on the orphan-GC sweeper (orphan_gc.go): its own periodic
// loop tracked by s.bg, an env-overridable interval + retention for
// tests, failure isolation per workspace, and blob reclamation that
// reuses the orphan GC's content-addressed dedupe + in-flight-upload
// guards. Runs on every deployment (self-host and cloud), mirroring the
// orphan GC, which already hard-reclaims soft-deleted attachment blobs
// on the same 30-day horizon everywhere.

const (
	// workspacePurgeRetention is the age a workspace's deleted_at must
	// exceed before it is hard-purged. 30 days matches the /privacy
	// erasure SLA — keep the two in lockstep.
	workspacePurgeRetention = 30 * 24 * time.Hour

	// defaultWorkspacePurgeInterval is how often the sweep runs. Daily,
	// matching the orphan GC and MCP-audit retention sweepers.
	defaultWorkspacePurgeInterval = 24 * time.Hour
)

// workspacePurgeResult records what one sweep accomplished — returned
// from runWorkspacePurgeSweep so tests can assert on the counters and
// the periodic logger can summarize a run in one line.
type workspacePurgeResult struct {
	Eligible       int   // workspaces matched by the eligibility query
	Purged         int   // workspaces fully purged (DB rows removed)
	BlobsReclaimed int   // on-disk/S3 blobs Delete'd through the backend
	BytesReclaimed int64 // sum of size_bytes for reclaimed blobs
	Skipped        int   // workspaces skipped due to a mid-sweep error
}

// workspacePurgeConfig captures the runtime knobs + lifecycle for the
// periodic loop. Mirrors orphanGCConfig / tokenReaperConfig.
type workspacePurgeConfig struct {
	mu        sync.Mutex
	interval  time.Duration
	retention time.Duration
	stop      chan struct{}
	running   bool
}

// SetWorkspacePurgeConfig overrides the default sweep interval (24h) and
// retention window (30d). Pass 0 for either to keep the package default.
// Must be called before StartWorkspacePurgeSweeper.
func (s *Server) SetWorkspacePurgeConfig(interval, retention time.Duration) {
	s.workspacePurge.mu.Lock()
	defer s.workspacePurge.mu.Unlock()
	if interval > 0 {
		s.workspacePurge.interval = interval
	}
	if retention > 0 {
		s.workspacePurge.retention = retention
	}
}

// StartWorkspacePurgeSweeper kicks off the periodic sweep loop.
// Idempotent — calling twice is a no-op. Must be called AFTER
// SetAttachments; the sweep no-ops (and logs) when the registry isn't
// wired so a server without attachment storage doesn't purge workspaces
// whose blobs it couldn't reclaim.
//
// Started from the real server bootstrap path (cmd/pad/main.go), NOT
// from Server.New, so unit tests that construct a Server don't spawn a
// background goroutine unless they opt in (mirrors StartOrphanGC). The
// loop is tracked by Server.bg so Stop() drains it before the process
// exits / the DB is closed (BUG-842 invariant).
func (s *Server) StartWorkspacePurgeSweeper() {
	s.workspacePurge.mu.Lock()
	if s.workspacePurge.running {
		s.workspacePurge.mu.Unlock()
		return
	}
	if s.workspacePurge.interval == 0 {
		s.workspacePurge.interval = defaultWorkspacePurgeInterval
	}
	if s.workspacePurge.retention == 0 {
		s.workspacePurge.retention = workspacePurgeRetention
	}
	s.workspacePurge.stop = make(chan struct{})
	s.workspacePurge.running = true
	interval := s.workspacePurge.interval
	retention := s.workspacePurge.retention
	stop := s.workspacePurge.stop
	s.workspacePurge.mu.Unlock()

	slog.Info("workspace purge sweeper started",
		"interval", interval.String(), "retention", retention.String())

	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		// One sweep on startup so a long-stopped server catches up
		// immediately rather than waiting a full interval.
		s.runWorkspacePurgeTick(retention)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				s.runWorkspacePurgeTick(retention)
			}
		}
	}()
}

// stopWorkspacePurgeSweeper signals the loop to exit. Called from
// Server.Stop(). Safe to call when the loop never started.
func (s *Server) stopWorkspacePurgeSweeper() {
	s.workspacePurge.mu.Lock()
	defer s.workspacePurge.mu.Unlock()
	if !s.workspacePurge.running {
		return
	}
	close(s.workspacePurge.stop)
	s.workspacePurge.running = false
}

// runWorkspacePurgeTick runs one tick of the periodic loop, wrapped with
// a 30-minute cap so a large sweep can't pin the goroutine across
// intervals. Logged at info on any purge, warn on failure, quiet when
// there is nothing to do.
func (s *Server) runWorkspacePurgeTick(retention time.Duration) {
	if s.attachments == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	cutoff := time.Now().UTC().Add(-retention)
	res, err := s.runWorkspacePurgeSweep(ctx, cutoff)
	if err != nil {
		slog.Warn("workspace purge sweep failed", "error", err)
		return
	}
	if res.Purged > 0 || res.Skipped > 0 {
		slog.Info("workspace purge sweep",
			"eligible", res.Eligible,
			"purged", res.Purged,
			"blobs_reclaimed", res.BlobsReclaimed,
			"bytes_reclaimed", res.BytesReclaimed,
			"skipped", res.Skipped,
			"cutoff", cutoff.Format(time.RFC3339))
	}
}

// runWorkspacePurgeSweep purges every workspace whose deleted_at is
// older than cutoff. Blobs are reclaimed BEFORE the DB rows are removed
// so a failed reclamation never strands an orphaned blob: the DB purge
// (which deletes the attachment rows) only runs once every physical blob
// is confirmed gone-or-still-shared. For each eligible workspace it:
//
//  1. Loads the attachment rows (physical storage keys + sizes).
//  2. Reclaims each unique physical blob through the storage backend
//     (FS + S3 safe via Registry.Resolve). A blob is deleted only when
//     no OTHER workspace references that exact storage_key
//     (content-addressed dedupe) and no upload of its content hash is in
//     flight. Anything that can't be reclaimed this tick — a transient
//     backend error OR an in-flight upload — DEFERS the whole workspace:
//     its DB rows are left in place and it stays eligible, so the next
//     tick retries. AttachmentStore.Delete is idempotent (missing key =
//     success), so retries self-heal after a crash between reclaim and
//     purge.
//  3. Only when all blobs are reclaimed-or-shared, runs
//     store.PurgeWorkspaceData in a single transaction (cascades every
//     child row + the workspace row). PurgeWorkspaceData refuses to
//     touch anything that isn't soft-deleted, so a live workspace is
//     never at risk even if the eligibility list were stale.
//
// Failures are isolated per workspace — a bad/deferred workspace
// increments Skipped and the sweep continues, so a single failure never
// wedges the loop. A context error (shutdown / timeout) stops the sweep
// and returns what was done so far.
func (s *Server) runWorkspacePurgeSweep(ctx context.Context, cutoff time.Time) (*workspacePurgeResult, error) {
	if s.attachments == nil {
		return nil, errors.New("attachments registry not configured")
	}
	res := &workspacePurgeResult{}

	candidates, err := s.store.ListPurgeableWorkspaces(cutoff)
	if err != nil {
		return nil, err
	}
	res.Eligible = len(candidates)

	for _, c := range candidates {
		if err := ctx.Err(); err != nil {
			return res, err
		}

		blobs, err := s.store.WorkspaceAttachmentBlobs(c.ID)
		if err != nil {
			slog.Warn("workspace purge: load attachment blobs failed",
				"workspace_id", c.ID, "slug", c.Slug, "error", err)
			res.Skipped++
			continue
		}

		// Reclaim blobs FIRST. If anything can't be reclaimed this tick,
		// defer the whole workspace (leave its rows so it's retried) —
		// this is what keeps a transient backend failure from orphaning a
		// blob the DB no longer references.
		if !s.reclaimWorkspaceBlobs(ctx, c, blobs, res) {
			res.Skipped++
			continue
		}

		// Every blob is gone (or still shared by another workspace) — now
		// it's safe to remove the DB rows.
		if err := s.store.PurgeWorkspaceData(c.ID); err != nil {
			slog.Warn("workspace purge: cascade delete failed",
				"workspace_id", c.ID, "slug", c.Slug, "error", err)
			res.Skipped++
			continue
		}
		res.Purged++
	}

	return res, nil
}

// reclaimWorkspaceBlobs deletes every physical blob owned solely by
// workspace c, returning true only when the workspace is safe to purge —
// i.e. every blob was either deleted or is legitimately retained because
// another workspace still references the same storage_key. It returns
// false (defer this workspace to a later tick) on ANY transient
// condition: a backend/count error, or a content hash with an upload in
// flight. Counters land on res.
//
// Dedupe is tracked by physical storage_key, not content_hash, so a blob
// stored under two keys (mixed FS/S3 backends) is reclaimed once per
// physical object rather than once per hash.
//
// Single-instance assumption. The sweep runs as ONE goroutine per
// process and processes candidates sequentially, so two workspaces
// sharing a storage_key are handled in order: the first sees the second
// still referencing the blob (others > 0, retained) and the second — its
// only remaining sharer now purged — deletes it. The shared blob is thus
// reclaimed by the last sharer. This is correct for Pad's single-instance
// deployment (CLAUDE.md: "single-instance everywhere" for v1). Two
// separate server PROCESSES sweeping concurrently could both observe the
// other's row and both skip the delete, orphaning the physical blob — the
// same cross-process limitation the orphan GC has (its inFlightHashes
// guard is a per-process mutex, not distributed). A distributed purge
// lock / backend-enumeration reclaimer is deferred with multi-instance
// support (out of scope for v1).
func (s *Server) reclaimWorkspaceBlobs(ctx context.Context, c store.WorkspacePurgeCandidate, blobs []models.Attachment, res *workspacePurgeResult) bool {
	reclaimed := make(map[string]bool) // storage_key -> deleted this call
	for _, a := range blobs {
		if reclaimed[a.StorageKey] {
			continue
		}

		// Dedupe guard: another workspace may still point at this exact
		// physical object. This workspace's own rows still exist (we
		// haven't purged yet), and the query excludes workspace_id = c.ID,
		// so only OTHER workspaces count.
		others, err := s.store.CountAttachmentsForStorageKeyOutsideWorkspace(a.StorageKey, c.ID)
		if err != nil {
			slog.Warn("workspace purge: count protecting refs failed",
				"workspace_id", c.ID, "storage_key", a.StorageKey, "error", err)
			return false
		}
		if others > 0 {
			// Physical blob is still shared — leave it; purging this
			// workspace's rows is safe, the other workspace keeps the
			// blob alive.
			reclaimed[a.StorageKey] = true
			continue
		}

		// Critical section: hold the in-flight mutex across the
		// uploadInFlight check AND the backend Delete so a concurrent
		// markUploadInFlight (an upload of the same content to another
		// workspace, Put done but row not yet inserted) blocks until we
		// finish — closing the TOCTOU window the orphan GC documents.
		s.inFlightHashesMu.Lock()
		inFlight := s.inFlightHashes[a.ContentHash] > 0
		var delErr error
		deleted := false
		if !inFlight {
			backend, resolveErr := s.attachments.Resolve(a.StorageKey)
			if resolveErr != nil {
				delErr = resolveErr
			} else if e := backend.Delete(ctx, a.StorageKey); e != nil {
				// Delete of a missing key is NOT an error per the
				// AttachmentStore contract, so this is a real IO/
				// permission failure.
				delErr = e
			} else {
				deleted = true
			}
		}
		s.inFlightHashesMu.Unlock()

		if inFlight {
			// An upload of this content is racing us — defer so we don't
			// delete a blob it's about to depend on, and don't purge rows
			// while its physical object is in doubt. Retried next tick.
			slog.Info("workspace purge: deferring — upload in flight",
				"workspace_id", c.ID, "storage_key", a.StorageKey)
			return false
		}
		if delErr != nil {
			slog.Warn("workspace purge: blob delete failed, deferring workspace",
				"workspace_id", c.ID, "storage_key", a.StorageKey, "error", delErr)
			return false
		}
		if deleted {
			reclaimed[a.StorageKey] = true
			res.BlobsReclaimed++
			res.BytesReclaimed += a.SizeBytes
		}
	}
	return true
}
