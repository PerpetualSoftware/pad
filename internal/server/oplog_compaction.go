package server

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3531: dormancy compaction. With a compactor set (PAD_OPLOG_COMPACT=on,
// default off), each op-log GC tick first replaces every dormant, flushed
// op-log with ONE frame, the Yjs state of the replayed document, instead of
// letting the sweep delete it. Yjs identity survives, so a tab that slept past
// the sweep resumes into the snapshot (collab Join, CompactedResumeCovers) and
// its unsent edits merge, where a deleted log forces it to refresh and hands
// its text back. The sweep then keeps compacted logs (PruneSweepKeeping). An
// item the store refuses (its log changed, or is no longer dormant and
// flushed) is left to the sweep's own checks. An item whose SNAPSHOT failed is
// KEPT and retried with backoff (TASK-3574, lead ruling): deleting its log on
// a failed job threw away the Yjs identity compaction exists to keep.

// OpLogCompactor builds a compaction snapshot. *materialize.Supervisor
// implements it (and *materialize.Runner in tests).
type OpLogCompactor interface {
	Snapshot(ctx context.Context, job materialize.Job) (materialize.Snapshot, error)
}

// opLogCompactionPerTick caps the snapshot jobs one GC tick runs (codex, TASK-3531):
// compaction shares the materializer's single job slot with op-log recovery,
// which carries UNFLUSHED edits, so a large dormant backlog must not hold the
// slot for a whole pass. Items past the cap are deferred, kept from this tick's
// sweep, and compacted by a later tick. A var so tests can lower it.
var opLogCompactionPerTick = 25

// opLogCompactionJobTimeout bounds one snapshot job from the sweep's side; the
// supervisor's own per-KiB deadline (BUG-3521) usually ends it first.
const opLogCompactionJobTimeout = 2 * time.Minute

// Compaction outcomes, the `outcome` label of pad_oplog_compactions_total.
const (
	compactCompacted   = "compacted"
	compactRefused     = "refused"      // the store's re-check: the log changed, or is no longer dormant and flushed
	compactFailed      = "failed"       // the snapshot job errored, or the bundle refused (pending structs)
	compactRoomOpen    = "room_open"    // a tab joined between the listing and the swap
	compactSetAside    = "set_aside"    // rows a schema rebuild set aside: left to the sweep, untouched
	compactSchema      = "schema"       // rows under another schema version: not replayable here
	compactUnflushable = "not_dormant"  // rows above the watermark: not what the dormancy listing promised
	compactReadFailed  = "read_failed"  // reading the op-log failed
	compactAlreadyDone = "already_done" // the log is already exactly its snapshot (not counted)
)

// SetOpLogCompactor turns dormancy compaction on (nil turns it off). Call
// before StartOpLogGC.
func (s *Server) SetOpLogCompactor(c OpLogCompactor) {
	s.opLogCompactor = c
}

// compactDormantOpLogs runs one compaction pass over the items the sweep would
// prune. It never holds an item's collab lock across a job: it reads the rows
// and builds the snapshot unlocked, then swaps under the lock with no room
// open, and CompactItemOpLog re-checks that the log is the one it read.
//
// It runs at most opLogCompactionPerTick snapshot jobs and returns the items
// it DEFERRED past that cap, which this tick's sweep must keep (a deferred
// item deleted now would lose its chance to compact).
func (s *Server) compactDormantOpLogs(minAge time.Duration) map[string]bool {
	deferred := map[string]bool{}
	if s.opLogCompactor == nil || s.collab == nil {
		return deferred
	}
	cutoff := time.Now().Add(-minAge)
	ids, err := s.store.ListDormantOpLogItemsBefore(cutoff)
	if err != nil {
		slog.Warn("op-log compaction: listing dormant items failed", "error", err)
		return deferred
	}
	now := compactBackoffNow()
	s.compactBackoffForgetUnlisted(ids)
	jobs := 0
	for _, itemID := range ids {
		if s.compactBackingOff(itemID, now) {
			deferred[itemID] = true // kept; retried when its backoff ends
			continue
		}
		if jobs >= opLogCompactionPerTick {
			deferred[itemID] = true
			continue
		}
		outcome := s.compactOne(itemID, cutoff)
		if outcome == compactAlreadyDone {
			continue
		}
		switch outcome {
		case compactFailed, compactReadFailed:
			deferred[itemID] = true // TASK-3574 (3): keep the log, retry later
			s.compactBackoffFailed(itemID, now)
		case compactCompacted:
			s.compactBackoffClear(itemID)
		}
		jobs++
		if s.metrics != nil {
			s.metrics.OpLogCompactionsTotal.WithLabelValues(outcome).Inc()
		}
	}
	if len(deferred) > 0 {
		slog.Info("op-log compaction: deferred items past this tick's cap", "deferred", len(deferred), "cap", opLogCompactionPerTick)
	}
	return deferred
}

func (s *Server) compactOne(itemID string, cutoff time.Time) string {
	if s.collab.HasRoom(itemID) {
		return compactRoomOpen
	}
	if done, err := s.store.IsCompactedLog(itemID); err != nil {
		slog.Warn("op-log compaction: reading the item failed", "item_id", itemID, "error", err)
		return compactReadFailed
	} else if done {
		return compactAlreadyDone
	}
	in, err := s.store.LoadMaterializeInput(itemID)
	if err != nil || in == nil {
		if err != nil {
			slog.Warn("op-log compaction: reading the op-log failed", "item_id", itemID, "error", err)
		}
		return compactReadFailed
	}
	if in.SetAside > 0 {
		return compactSetAside
	}
	if in.Pending > 0 {
		return compactUnflushable
	}
	for _, v := range in.SchemaVersions {
		if v != collab.DefaultSchemaVersion {
			return compactSchema
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), opLogCompactionJobTimeout)
	defer cancel()
	snap, err := s.opLogCompactor.Snapshot(ctx, materialize.Job{
		Rows:          in.Rows,
		SchemaVersion: collab.DefaultSchemaVersion,
		LinkIndex:     []materialize.LinkEntry{},
	})
	if err != nil {
		slog.Warn("op-log compaction: building the snapshot failed; the sweep will delete the log as before",
			"item_id", itemID, "error", err)
		return compactFailed
	}

	err = s.collab.UnderItemLockIfNoRoom(itemID, func() error {
		_, cerr := s.store.CompactItemOpLog(itemID, cutoff, in.IDs, snap.Frame, collab.DefaultSchemaVersion)
		return cerr
	})
	switch {
	case errors.Is(err, collab.ErrRoomOpen):
		return compactRoomOpen
	case errors.Is(err, store.ErrCompactionRefused):
		return compactRefused
	case err != nil:
		slog.Warn("op-log compaction: the swap failed", "item_id", itemID, "error", err)
		return compactFailed
	}
	if s.metrics != nil {
		s.metrics.OpLogCompactionSnapshotBytesTotal.Add(float64(len(snap.Frame)))
		s.metrics.OpLogCompactionMarkdownBytesTotal.Add(float64(len(snap.Markdown)))
	}
	slog.Info("op-log compacted", "item_id", itemID, "rows", len(in.Rows), "snapshot_bytes", len(snap.Frame), "markdown_bytes", len(snap.Markdown))
	return compactCompacted
}

// keepCompacted is the sweep's keep func while compaction is on: a log that is
// exactly its snapshot stays, and so does one deferred past this tick's cap.
func (s *Server) keepCompacted(deferred map[string]bool) func(string) bool {
	return func(itemID string) bool {
		if deferred[itemID] {
			return true
		}
		return s.isCompactedOrUnknown(itemID)
	}
}

func (s *Server) isCompactedOrUnknown(itemID string) bool {
	done, err := s.store.IsCompactedLog(itemID)
	if err != nil {
		// Unknown: keep it. A wrong keep costs one sweep; a wrong delete
		// costs the snapshot.
		return true
	}
	return done
}

// TASK-3574 (3): a failed compaction snapshot keeps the log and retries with
// backoff, instead of leaving the item to the sweep's delete. The state is
// in memory: a restart retries every kept item once, which is harmless.
var (
	compactBackoffBase = 10 * time.Minute
	compactBackoffMax  = 24 * time.Hour
	compactBackoffNow  = time.Now
)

type compactBackoffState struct {
	failures int
	next     time.Time
}

func (s *Server) compactBackingOff(itemID string, now time.Time) bool {
	s.compactBackoffMu.Lock()
	defer s.compactBackoffMu.Unlock()
	b, ok := s.compactBackoff[itemID]
	return ok && now.Before(b.next)
}

func (s *Server) compactBackoffFailed(itemID string, now time.Time) {
	s.compactBackoffMu.Lock()
	defer s.compactBackoffMu.Unlock()
	if s.compactBackoff == nil {
		s.compactBackoff = map[string]compactBackoffState{}
	}
	b := s.compactBackoff[itemID]
	b.failures++
	d := compactBackoffBase << min(b.failures-1, 20)
	if d <= 0 || d > compactBackoffMax {
		d = compactBackoffMax
	}
	b.next = now.Add(d)
	s.compactBackoff[itemID] = b
	slog.Warn("op-log compaction: snapshot failed; the log is kept and retried later",
		"item_id", itemID, "failures", b.failures, "retry_in", d.String())
}

func (s *Server) compactBackoffClear(itemID string) {
	s.compactBackoffMu.Lock()
	defer s.compactBackoffMu.Unlock()
	delete(s.compactBackoff, itemID)
}

// compactBackoffForgetUnlisted drops state for items the dormancy listing no
// longer returns (edited, deleted, or compacted another way).
func (s *Server) compactBackoffForgetUnlisted(ids []string) {
	s.compactBackoffMu.Lock()
	defer s.compactBackoffMu.Unlock()
	if len(s.compactBackoff) == 0 {
		return
	}
	listed := make(map[string]bool, len(ids))
	for _, id := range ids {
		listed[id] = true
	}
	for id := range s.compactBackoff {
		if !listed[id] {
			delete(s.compactBackoff, id)
		}
	}
}
