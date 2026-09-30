package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/materialize"
	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Op-log recovery (TASK-2198 U4): the triggers and the worker.
//
// A tab that dies without flushing leaves its last typing only in the
// op-log, and items.content (what REST, the CLI, MCP and search read) stays
// behind it until a tab next opens the item — before this, possibly never.
// This file closes that window in the background:
//
//   - T3, room idle: when a collab room is reclaimed at the end of its grace
//     TTL (collab.DefaultGraceTTL, 60s after the last connection left) and the
//     item still has content-bearing rows above its flush watermark, the item
//     is queued.
//   - T1, sweep: every materializeSweepInterval the store is asked for live
//     items whose op-log holds such rows, whose newest row is older than
//     materializeDormancy, and which have no set-aside rows
//     (store.ListMaterializeCandidates); those without an open room are
//     queued. It catches what T3 cannot see: a server restart, a room that
//     closed while the worker was failing, rows left by an older server.
//
// One goroutine drains a de-duplicated queue into the Materializer (in
// production the single materialize.Supervisor, which runs the job in a
// capped child process). Nothing here runs on a request path.
//
// FAILURE BUDGET. The input is untrusted (any editor can write op-log rows),
// so a job is never retried in a loop: a failed item waits
// materializeBackoffBase, doubling per consecutive failure, and after
// materializeMaxFailures consecutive failures it is not tried again until its
// op-log changes (MAX(id) moves past the value recorded at the last failure).
// The budget is kept in memory, bounded at materializeBudgetCap entries: a
// restart re-grants each item its K attempts, which bounds a poison item at K
// worker deaths per process lifetime. Each failure is logged with its typed
// error (materialize.Err*).

const (
	materializeSweepInterval = time.Minute
	materializeDormancy      = 2 * time.Minute
	materializeSweepLimit    = 200
	materializeMaxFailures   = 3
	materializeBackoffBase   = time.Minute
	materializeBudgetCap     = 10000
	materializeQueueCap      = 10000
)

// Materializer runs one materialization. *materialize.Supervisor implements it
// (and *materialize.Runner, which tests use in-process).
type Materializer interface {
	Materialize(ctx context.Context, job materialize.Job) (string, error)
}

type materializeBudget struct {
	failures    int
	opLogMax    int64
	nextAttempt time.Time
	lastErr     string
}

type materializeRecovery struct {
	s   *Server
	m   Materializer
	cfg materializeRecoveryConfig

	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	started bool
	stopped bool
	queued  map[string]bool
	queue   []string
	wake    chan struct{}
	budget  map[string]*materializeBudget

	// processed, when set by a test, receives each processed item and its
	// result after the item is done.
	processed func(itemID string, r materializeResult)
}

type materializeRecoveryConfig struct {
	sweepInterval time.Duration
	dormancy      time.Duration
	maxFailures   int
	backoffBase   time.Duration
	now           func() time.Time
}

// materializeResult is what one processing pass did with an item.
type materializeResult string

const (
	mrApplied        materializeResult = "applied"
	mrStamped        materializeResult = "stamped"
	mrNothing        materializeResult = "nothing_pending"
	mrRoomOpen       materializeResult = "room_open"
	mrSetAside       materializeResult = "set_aside"
	mrCursorMoved    materializeResult = "cursor_moved"
	mrGone           materializeResult = "gone"
	mrBudgetExceeded materializeResult = "budget_exhausted"
	mrBackoff        materializeResult = "backoff"
	mrSchemaVersion  materializeResult = "schema_version"
	mrFailed         materializeResult = "failed"
	mrStopped        materializeResult = "stopped"
)

// SetMaterializer installs the op-log recovery worker's Materializer. Nil (the
// default, and what PAD_MATERIALIZE=off leaves) disables recovery entirely:
// StartMaterializeRecovery then installs no hook and starts no goroutine. Must
// be called before StartMaterializeRecovery. If m also has a Close() error
// method, Stop() calls it after the worker has drained.
func (s *Server) SetMaterializer(m Materializer) {
	if m == nil {
		s.materializeRecovery = nil
		return
	}
	s.materializeRecovery = newMaterializeRecovery(s, m, materializeRecoveryConfig{})
}

func newMaterializeRecovery(s *Server, m Materializer, cfg materializeRecoveryConfig) *materializeRecovery {
	if cfg.sweepInterval <= 0 {
		cfg.sweepInterval = materializeSweepInterval
	}
	if cfg.dormancy <= 0 {
		cfg.dormancy = materializeDormancy
	}
	if cfg.maxFailures <= 0 {
		cfg.maxFailures = materializeMaxFailures
	}
	if cfg.backoffBase <= 0 {
		cfg.backoffBase = materializeBackoffBase
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &materializeRecovery{
		s: s, m: m, cfg: cfg, ctx: ctx, cancel: cancel,
		queued: map[string]bool{},
		wake:   make(chan struct{}, 1),
		budget: map[string]*materializeBudget{},
	}
}

// StartMaterializeRecovery starts the op-log recovery worker and its two
// triggers. A no-op without a Materializer (SetMaterializer) or without a
// collab room manager (SetCollabRoomManager), and idempotent.
func (s *Server) StartMaterializeRecovery() {
	r := s.materializeRecovery
	if r == nil || s.collab == nil {
		return
	}
	r.mu.Lock()
	if r.started || r.stopped {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.mu.Unlock()

	s.collab.SetIdleHook(r.onRoomIdle)
	slog.Info("op-log recovery started",
		"sweep_interval", r.cfg.sweepInterval.String(),
		"dormancy", r.cfg.dormancy.String(),
		"max_failures", r.cfg.maxFailures)

	s.bg.Add(2)
	go func() {
		defer s.bg.Done()
		r.workLoop()
	}()
	go func() {
		defer s.bg.Done()
		defer s.recoverSweeper("materialize-sweep")
		t := time.NewTicker(r.cfg.sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-t.C:
				r.sweep()
			}
		}
	}()
}

func (s *Server) stopMaterializeRecovery() {
	r := s.materializeRecovery
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stopped = true
	r.mu.Unlock()
	if s.collab != nil {
		s.collab.SetIdleHook(nil)
	}
	r.cancel()
}

// closeMaterializer closes the Materializer when it has a Close method (the
// Supervisor: kills its worker process). Called by Stop after bg.Wait.
func (s *Server) closeMaterializer() {
	r := s.materializeRecovery
	if r == nil {
		return
	}
	if c, ok := r.m.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			slog.Warn("op-log recovery: closing the materializer failed", "error", err)
		}
	}
}

// onRoomIdle is T3: the collab room for itemID was reclaimed. Runs on the
// grace timer's goroutine; one indexed read, then a non-blocking enqueue.
func (r *materializeRecovery) onRoomIdle(itemID string) {
	pending, err := r.s.store.ItemHasPendingContent(itemID)
	if err != nil {
		slog.Warn("op-log recovery: pending check on room idle failed", "item_id", itemID, "error", err)
		return
	}
	if pending {
		r.enqueue(itemID)
	}
}

// sweep is T1.
func (r *materializeRecovery) sweep() {
	ids, err := r.s.store.ListMaterializeCandidates(r.cfg.now().Add(-r.cfg.dormancy), materializeSweepLimit)
	if err != nil {
		slog.Warn("op-log recovery: sweep query failed", "error", err)
		return
	}
	for _, id := range ids {
		if r.s.collab != nil && r.s.collab.HasRoom(id) {
			continue
		}
		r.enqueue(id)
	}
}

// enqueue adds itemID unless it is already queued. Never blocks; a full queue
// drops the item, which the next sweep will offer again.
func (r *materializeRecovery) enqueue(itemID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.queued[itemID] {
		return false
	}
	if len(r.queue) >= materializeQueueCap {
		return false
	}
	r.queued[itemID] = true
	r.queue = append(r.queue, itemID)
	select {
	case r.wake <- struct{}{}:
	default:
	}
	return true
}

func (r *materializeRecovery) dequeue() (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.queue) == 0 {
		return "", false
	}
	id := r.queue[0]
	r.queue = r.queue[1:]
	delete(r.queued, id)
	return id, true
}

func (r *materializeRecovery) workLoop() {
	for {
		for {
			if r.ctx.Err() != nil {
				return
			}
			id, ok := r.dequeue()
			if !ok {
				break
			}
			res := r.processSafely(id)
			if r.processed != nil {
				r.processed(id, res)
			}
		}
		select {
		case <-r.ctx.Done():
			return
		case <-r.wake:
		}
	}
}

// processSafely is process with a panic fence, so one bad item cannot end the
// worker goroutine.
func (r *materializeRecovery) processSafely(itemID string) (res materializeResult) {
	defer func() {
		if p := recover(); p != nil {
			slog.Error("op-log recovery: panic processing item",
				"item_id", itemID, "panic", p, "stack", string(debug.Stack()))
			res = mrFailed
		}
	}()
	return r.process(itemID)
}

func (r *materializeRecovery) process(itemID string) materializeResult {
	s := r.s
	if s.collab == nil {
		return mrStopped
	}
	if s.collab.HasRoom(itemID) {
		return mrRoomOpen
	}
	in, err := s.store.LoadMaterializeInput(itemID)
	if err != nil {
		slog.Warn("op-log recovery: reading the op-log failed", "item_id", itemID, "error", err)
		return mrFailed
	}
	if in == nil {
		r.forget(itemID)
		return mrGone
	}
	if in.SetAside > 0 {
		return mrSetAside
	}
	if in.Pending == 0 {
		r.forget(itemID)
		return mrNothing
	}
	if res, ok := r.admit(itemID, in.Cursor); !ok {
		return res
	}
	for _, v := range in.SchemaVersions {
		if v != collab.DefaultSchemaVersion {
			r.exhaust(itemID, in.Cursor, fmt.Sprintf("op-log schema version %q, server %q", v, collab.DefaultSchemaVersion))
			slog.Info("op-log recovery: skipping item written under another editor schema",
				"item_id", itemID, "row_schema_version", v, "server_schema_version", collab.DefaultSchemaVersion)
			return mrSchemaVersion
		}
	}

	ws, err := s.store.GetWorkspaceByID(in.WorkspaceID)
	if err != nil || ws == nil {
		slog.Warn("op-log recovery: reading the workspace failed", "item_id", itemID, "error", err)
		return mrFailed
	}
	index, err := s.materializeLinkIndex(in.WorkspaceID)
	if err != nil {
		slog.Warn("op-log recovery: building the link index failed", "item_id", itemID, "error", err)
		return mrFailed
	}

	job := materialize.Job{
		Rows:          in.Rows,
		SchemaVersion: collab.DefaultSchemaVersion,
		LinkIndex:     index,
		WorkspaceSlug: ws.Slug,
	}
	started := time.Now()
	md, err := r.m.Materialize(r.ctx, job)
	if err != nil {
		if r.ctx.Err() != nil {
			return mrStopped
		}
		r.recordFailure(itemID, in.Cursor, err)
		return mrFailed
	}

	var (
		outcome store.MaterializeOutcome
		updated *models.Item
	)
	err = s.collab.UnderItemLockIfNoRoom(itemID, func() error {
		var ferr error
		outcome, updated, ferr = s.store.MaterializeFlush(itemID, in.Cursor, md)
		return ferr
	})
	if errors.Is(err, collab.ErrRoomOpen) {
		return mrRoomOpen
	}
	if err != nil {
		if r.ctx.Err() != nil {
			return mrStopped
		}
		slog.Warn("op-log recovery: writing the recovered body failed", "item_id", itemID, "error", err)
		return mrFailed
	}
	r.forget(itemID)
	switch outcome {
	case store.MaterializeApplied:
		slog.Info("op-log recovery: recovered an item's unsaved editor session",
			"item_id", itemID, "op_log_cursor", in.Cursor, "rows", len(in.Rows),
			"pending_rows", in.Pending, "bytes", len(md), "elapsed", time.Since(started).String())
		if updated != nil {
			s.publishItemEventWithName(sseItemUpdated, updated.WorkspaceID, updated.ID, updated.Title,
				updated.CollectionSlug, models.VersionCreatedBySystem, "", models.VersionSourceRecovery, updated.Seq)
		}
		return mrApplied
	case store.MaterializeStamped:
		return mrStamped
	case store.MaterializeCursorMoved:
		return mrCursorMoved
	case store.MaterializeNothingPending:
		return mrNothing
	case store.MaterializeSetAside:
		return mrSetAside
	default:
		return mrGone
	}
}

// admit applies the failure budget. The budget resets when the op-log has
// moved past the MAX(id) recorded at the last failure.
func (r *materializeRecovery) admit(itemID string, cursor int64) (materializeResult, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.budget[itemID]
	if b == nil {
		return "", true
	}
	if cursor > b.opLogMax {
		delete(r.budget, itemID)
		return "", true
	}
	if b.failures >= r.cfg.maxFailures {
		return mrBudgetExceeded, false
	}
	if r.cfg.now().Before(b.nextAttempt) {
		return mrBackoff, false
	}
	return "", true
}

func (r *materializeRecovery) forget(itemID string) {
	r.mu.Lock()
	delete(r.budget, itemID)
	r.mu.Unlock()
}

func (r *materializeRecovery) budgetSlotLocked(itemID string) *materializeBudget {
	b := r.budget[itemID]
	if b == nil {
		if len(r.budget) >= materializeBudgetCap {
			for k := range r.budget { // evict one; it re-earns its K attempts
				delete(r.budget, k)
				break
			}
		}
		b = &materializeBudget{}
		r.budget[itemID] = b
	}
	return b
}

func (r *materializeRecovery) recordFailure(itemID string, cursor int64, err error) {
	r.mu.Lock()
	b := r.budgetSlotLocked(itemID)
	b.failures++
	b.opLogMax = cursor
	b.lastErr = err.Error()
	delay := r.cfg.backoffBase << (b.failures - 1)
	b.nextAttempt = r.cfg.now().Add(delay)
	failures := b.failures
	r.mu.Unlock()

	attrs := []any{"item_id", itemID, "op_log_cursor", cursor, "failures", failures,
		"max_failures", r.cfg.maxFailures, "kind", materializeErrorKind(err), "error", err}
	if failures >= r.cfg.maxFailures {
		slog.Warn("op-log recovery: giving up on item until its op-log changes", attrs...)
		return
	}
	slog.Warn("op-log recovery: materialization failed; will retry after a backoff",
		append(attrs, "retry_after", delay.String())...)
}

// exhaust marks the item as not to be tried until its op-log changes.
func (r *materializeRecovery) exhaust(itemID string, cursor int64, why string) {
	r.mu.Lock()
	b := r.budgetSlotLocked(itemID)
	b.failures = r.cfg.maxFailures
	b.opLogMax = cursor
	b.lastErr = why
	r.mu.Unlock()
}

// materializeErrorKind names the typed materialize error, for the log line.
func materializeErrorKind(err error) string {
	for _, k := range []struct {
		err  error
		name string
	}{
		{materialize.ErrMemoryLimit, "memory_limit"},
		{materialize.ErrDeadline, "deadline"},
		{materialize.ErrChildDied, "worker_died"},
		{materialize.ErrProtocol, "protocol"},
		{materialize.ErrStart, "start"},
		{materialize.ErrJobFailed, "job_failed"},
		{materialize.ErrJobTooLarge, "too_large"},
		{materialize.ErrSchemaVersion, "schema_version"},
		{materialize.ErrClosed, "closed"},
		{materialize.ErrInterrupted, "interrupted"},
	} {
		if errors.Is(err, k.err) {
			return k.name
		}
	}
	return "other"
}

// materializeLinkIndex is the link index a recovery job converts internal
// links against: EVERY live item of the workspace (Dave's ruling), not the
// set any one viewer can see. A tab's index is its viewer's localIndex, but a
// recovery has no viewer, and an index missing an item a link points at would
// leave that link as a raw URL instead of the [[REF]] a full tab stores.
// Titles in the output come only from the document itself: markdownToWikiLinks
// compares an item's title with the link text and never emits it (proven by
// TestMaterializeLinkIndexContributesNoTitle).
//
// ORDER mirrors localIndex.getAll (web/src/lib/stores/localIndex.svelte.ts):
// updated_at DESC, then id ASC, because markdownToWikiLinks takes the FIRST
// match. Sorted here rather than trusting ORDER BY, because Postgres compares
// TEXT under the database collation and the tab compares UTF-16 code units;
// Go's byte order is the tab's order for the ASCII ids the store mints.
func (s *Server) materializeLinkIndex(workspaceID string) ([]materialize.LinkEntry, error) {
	items, err := s.store.ListItemsIndex(workspaceID, store.ItemIndexParams{})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.ID < b.ID
	})
	out := make([]materialize.LinkEntry, 0, len(items))
	for _, it := range items {
		e := materialize.LinkEntry{Slug: it.Slug, Title: it.Title, CollectionPrefix: it.CollectionPrefix}
		if it.ItemNumber != nil {
			e.ItemNumber = *it.ItemNumber
		}
		out = append(out, e)
	}
	return out, nil
}
