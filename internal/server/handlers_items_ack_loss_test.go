package server

import (
	"database/sql"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-2994: a content PATCH whose transaction COMMITS but whose tx.Commit()
// reports an error must not be written a second time.
//
// The path, enumerated rather than assumed: handleUpdateItem writes twice iff
// routeContentUpdate returns contentRouteFallThrough, and that return had exactly
// one producer — the settleDirectFailed arm, reached when PruneAndApply's applyFn
// (the store write) fails and writeTypedItemRefusal does not recognise the error.
// PruneAndApply's only other error, ErrRoomActiveDuringPrune, is consumed by the
// settle loop above and never reaches that arm, so EVERY error arriving there is a
// store write-transaction error. The fall-through's premise — "nothing was written"
// — is an inference from rollback semantics, and a lost commit ack falsifies it.
//
// WHY THE OBVIOUS INSTRUMENT DOES NOT WORK. Counting item_versions rows does not
// discriminate here, and a test built on it would pass against the defect. The
// version INSERT is gated on `*input.Content != existing.Content` (items.go), and
// the second write re-reads `existing` AFTER the first commit landed — so it sees
// the content it is about to write and mints no row. One version row is what BOTH
// the broken and the fixed code produce.
//
// What does discriminate: how many transactions reached COMMIT (counted at the seam
// itself, which is the property stated directly), the number of item_updated events
// the request emits, and the status code — a second write that succeeds answers 200
// about a request whose first write's outcome was never established.

var errSimItemAckLoss = errors.New("simulated commit ack loss")

// ackLossServer builds a *Server on the named backend with collab and an event bus
// wired. The Postgres leg skips unless PAD_TEST_POSTGRES_URL is set (make test-pg).
//
// It exists because testServer is hardwired to storetest.NewSQLite, so a test built
// on that helper measures SQLite whatever gate is running it — the trap recorded on
// this bug's sibling unit. testServerPostgres is the precedent for the other leg.
func ackLossServer(t *testing.T, driver store.DriverType) *Server {
	t.Helper()
	var s *store.Store
	if driver == store.DriverPostgres {
		s = storetest.NewPostgres(t) // skips if PAD_TEST_POSTGRES_URL is unset
	} else {
		s = storetest.NewSQLite(t)
	}
	if got := s.D().Driver(); got != driver {
		t.Fatalf("wanted a %s store, got %s — this leg would have measured the wrong backend", driver, got)
	}
	srv := New(s)
	t.Cleanup(func() { srv.Stop() })

	obus := collab.NewMemoryOpBus()
	t.Cleanup(obus.Close)
	rm := collab.NewRoomManager(srv.store, obus)
	t.Cleanup(rm.Close)
	srv.SetCollabRoomManager(rm)

	srv.SetEventBus(events.New())
	return srv
}

func TestContentPatchAckLossCommitsOnce_SQLite(t *testing.T) {
	assertContentPatchAckLossCommitsOnce(t, ackLossServer(t, store.DriverSQLite))
}

func TestContentPatchAckLossCommitsOnce_Postgres(t *testing.T) {
	assertContentPatchAckLossCommitsOnce(t, ackLossServer(t, store.DriverPostgres))
}

func assertContentPatchAckLossCommitsOnce(t *testing.T, srv *Server) {
	t.Helper()

	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Ack loss", `{"status":"open"}`)

	// PRECONDITION, not decoration: with no conn dialled there is no electable
	// applier, so routeContentUpdate takes the DIRECT-WRITE route — the only route
	// that can reach the fall-through. Without this the test could pass by never
	// visiting the path it exists to measure.
	if srv.collab.HasElectableApplier(item.ID) {
		t.Fatal("an applier is electable; this test would have measured the applier path instead")
	}

	// Fail the FIRST commit only, and commit it for real first. That is the whole
	// point: the transaction's effects are durable and the caller is told they are
	// not. A hook that returned an error WITHOUT committing would exercise the
	// ordinary rollback case, which was never broken.
	var commits int32
	restore := srv.store.SetItemUpdateCommitHookForTesting(func(tx *sql.Tx) error {
		n := atomic.AddInt32(&commits, 1)
		if cerr := tx.Commit(); cerr != nil {
			return cerr
		}
		if n == 1 {
			return errSimItemAckLoss
		}
		return nil
	})
	t.Cleanup(restore)

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{
			"title":   "Renamed by the one request",
			"content": "content written once",
		})

	// The seam must have fired, or nothing below means anything.
	if atomic.LoadInt32(&commits) == 0 {
		t.Fatal("the commit seam never fired; the request did not reach the store write this test measures")
	}

	// THE PROPERTY. One request, at most one committed transaction.
	//
	// Counted at the seam because the two instruments a reader reaches for first
	// cannot tell ONE committed write from TWO, so a test built on either passes
	// against the defect:
	//
	//   - item_versions rows. The INSERT is gated on *input.Content !=
	//     existing.Content, and the replayed write re-reads `existing` AFTER the
	//     first commit landed, so it finds the content already at its target value
	//     and mints nothing.
	//   - item_updated events. The store's outbox emit is gated on
	//     itemUpdatedSliceChanged and finds nothing changed for the same reason; the
	//     handler's SSE publish fires once per successful REQUEST, not once per
	//     write. Stated carefully because an earlier draft of this comment said
	//     "events do not discriminate" full stop, and that is wrong in the other
	//     direction (codex round 1): the fixed tree emits ZERO, because 500 returns
	//     before the publish. That distinguishes fixed from unfixed — but it is the
	//     same fact the status assertion below already pins, and it says nothing
	//     about how many times the row was written, which is the property here.
	//
	// Measured against the unfixed tree, not reasoned about.
	if n := atomic.LoadInt32(&commits); n != 1 {
		t.Errorf("one PATCH drove %d transactions to COMMIT, want 1: the lost ack was read as a rollback "+
			"and the write was replayed against the row it had already changed", n)
	}

	// ANTI-VACUITY CONTROL. The seam is only honest if the first transaction really
	// did land, so assert the effects are on disk even though the caller was told
	// they were not. Without this the whole test could pass against a seam that
	// merely failed the commit, which is the case the code already handled.
	after, gerr := srv.store.GetItem(item.ID)
	if gerr != nil || after == nil {
		t.Fatalf("re-read item: %v", gerr)
	}
	if after.Title != "Renamed by the one request" || after.Content != "content written once" {
		t.Fatalf("the first transaction did not durably land (title=%q content=%q); the seam did not "+
			"reproduce a lost ack and nothing above measures this bug", after.Title, after.Content)
	}

	// A commit whose outcome was never established must not be answered as success.
	// 500 is what the plain path already answers for the same unrecognised store
	// error, so this is parity rather than a new refusal.
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("PATCH answered %d; a write whose commit outcome is unknown must not read as success "+
			"(body %s)", rr.Code, rr.Body.String())
	}
}

var errSimPruneFailure = errors.New("simulated op-log prune failure")

// TestDirectWritePruneFailureDoesNotReplayWithoutThePrune is the SECOND defect of
// the same removed fall-through (BUG-2994), and it is a different error shape on
// purpose.
//
// The direct write rides the op-log prune inside its own transaction
// (composePruneWithPrecheck) so a refusal from either half rolls the other back.
// The handler's ordinary write does NOT carry that composition. So a prune failure
// — an honest rollback, nothing committed, no double write — fell through to a
// write that set items.content while the per-item op-log still held exactly the ops
// the prune existed to remove. PruneAndApply has already established under appendMu
// that no live writer holds the room, so those rows belong to departed peers and a
// reconnecting client replays them over the content just seeded.
//
// WHY THIS EXISTS ALONGSIDE THE ACK-LOSS TEST rather than being folded into it: the
// two errors are terminal for different reasons, and the arm must be terminal for
// ALL of them. Re-opening the fall-through for "non-commit" errors only — the
// plausible shape of a future regression, since a transient error looks retryable —
// leaves the ack-loss test green and is caught only here.
func TestDirectWritePruneFailureDoesNotReplayWithoutThePrune(t *testing.T) {
	srv := ackLossServer(t, store.DriverSQLite)
	slug := createWSWithCollections(t, srv)
	item := createTaskWithFields(t, srv, slug, "Prune failure", `{"status":"open"}`)

	// Op-log rows standing in for a departed peer's unflushed edits: content that
	// exists ONLY in the op-log, which is what the prune is there to discard.
	seedOpLog(t, srv, item.ID, 3)
	if got := countOpLog(t, srv, item.ID); got != 3 {
		t.Fatalf("seeded op-log = %d rows, want 3", got)
	}
	if srv.collab.HasElectableApplier(item.ID) {
		t.Fatal("an applier is electable; this test would have measured the applier path instead")
	}

	before, err := srv.store.GetItem(item.ID)
	if err != nil || before == nil {
		t.Fatalf("GetItem: %v", err)
	}

	var faults int32
	srv.directWritePruneFault = func() error {
		atomic.AddInt32(&faults, 1)
		return errSimPruneFailure
	}
	t.Cleanup(func() { srv.directWritePruneFault = nil })

	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+item.Slug,
		map[string]interface{}{"content": "content that must not outrun the prune"})

	if atomic.LoadInt32(&faults) == 0 {
		t.Fatal("the prune seam never fired; the request did not reach the direct write this test measures")
	}
	if rr.Code != http.StatusInternalServerError {
		t.Errorf("PATCH answered %d, want 500: a write that could not prune the op-log must be refused, "+
			"not retried without the prune (body %s)", rr.Code, rr.Body.String())
	}

	after, err := srv.store.GetItem(item.ID)
	if err != nil || after == nil {
		t.Fatalf("GetItem: %v", err)
	}
	if after.Content != before.Content {
		t.Errorf("items.content moved to %q despite the prune failing: the replayed write did not carry "+
			"composePruneWithPrecheck, so it seeded content the surviving op-log will be replayed over",
			after.Content)
	}
	// PRECONDITION, not a discriminator — it holds either way, and saying so keeps
	// it from reading as coverage it does not provide. The rows survive in BOTH
	// trees (the replayed write carried no prune at all), which is precisely what
	// makes the content move above harmful: there is something left to replay over
	// it. Measured against the unfixed tree.
	if got := countOpLog(t, srv, item.ID); got != 3 {
		t.Errorf("op-log = %d rows, want the 3 seeded: the prune must roll back with its transaction", got)
	}
}
