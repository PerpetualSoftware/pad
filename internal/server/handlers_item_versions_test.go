package server

import (
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/collab"
	"github.com/PerpetualSoftware/pad/internal/events"
	"github.com/PerpetualSoftware/pad/internal/models"
)

const (
	reconcileOriginal = "original body\n"
	reconcileRestored = "restored-from-an-older-version body\n"
)

// errSimAckLoss simulates a Postgres commit whose tx durably landed but whose
// acknowledgement was lost at the connection boundary — the exact case BUG-2276
// residual 1's reconciliation exists for.
var errSimAckLoss = errors.New("sim: commit ack lost after durable landing")

func reconcileNewItem(t *testing.T, srv *Server, slug, content string) *models.Item {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/tasks/items", map[string]interface{}{
		"title":   "reconcile subject",
		"content": content,
		"source":  "cli",
		"fields":  `{"status":"open"}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create item: %d: %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	return &it
}

// runRealRestoreTx drives the REAL restore commit transaction against the store —
// the exact MaxOpLogIDTx + StampRestoreBoundaryOpIDTx + PruneItemOpLogTx +
// MarkRestoreBoundary sequence handleRestoreItemVersion runs — so reconcile is
// exercised against genuine ATOMIC durable stamps, not columns written by hand.
// Returns the restored item + the pre-prune MAX (the boundary is MAX+1).
func runRealRestoreTx(t *testing.T, srv *Server, itemID, content string) (*models.Item, int64) {
	t.Helper()
	var maxID int64
	u, err := srv.store.UpdateItemWithPreCheck(itemID, models.ItemUpdate{
		Content:             &content,
		ChangeSummary:       "Restored from an older version",
		LastModifiedBy:      "user",
		Source:              "web",
		ForceVersion:        true,
		MarkRestoreBoundary: true,
	}, func(tx *sql.Tx, _ *models.Item) error {
		m, _, merr := srv.store.MaxOpLogIDTx(tx, itemID)
		if merr != nil {
			return merr
		}
		maxID = m
		if serr := srv.store.StampRestoreBoundaryOpIDTx(tx, itemID, m+1); serr != nil {
			return serr
		}
		return srv.store.PruneItemOpLogTx(tx, itemID)
	})
	if err != nil {
		t.Fatalf("real restore tx: %v", err)
	}
	if u == nil {
		t.Fatal("real restore tx: item vanished")
	}
	return u, maxID
}

// TestReconcileRestoreCommit exercises the Postgres commit-outcome reconciliation
// signal logic (BUG-2276 residual 1) against a real store, driving genuine restore
// tx stamps. After a version-restore commit reports an error, reconcileRestoreCommit
// re-reads the item and decides — from two independent durable signals (content ==
// the restored version AND last_restore_seq advanced past the UNDER-LOCK baseline
// seq) — whether the restore actually LANDED, DEFINITELY rolled back, or is
// UNCERTAIN (signals disagree / read error / not-found), the last of which returns
// an error so ForceRefreshRoom keeps the room frozen and plain-closes it.
//
// (Runs on the SQLite test store; the helper's LOGIC is dialect-independent — it is
// only WIRED IN on Postgres, where a durably-landed-but-ack-lost commit can occur.
// The store gate lives in handleRestoreItemVersion.)
func TestReconcileRestoreCommit(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	// (a) The restore's durable effects are all present on a fresh read → LANDED,
	// with Boundary/Seq recovered from the durable stamps AND the fresh item
	// returned (so the caller can respond with it + emit the SSE event).
	t.Run("landed_returns_item", func(t *testing.T) {
		it := reconcileNewItem(t, srv, slug, reconcileOriginal)
		baseline := it.Seq
		u, maxID := runRealRestoreTx(t, srv, it.ID, reconcileRestored)

		res, fresh, err := srv.reconcileRestoreCommit(it.ID, reconcileRestored, baseline, true)
		if err != nil {
			t.Fatalf("reconcile (landed) err: %v", err)
		}
		if !res.Landed {
			t.Fatal("reconcile: want Landed=true for a durably-landed restore")
		}
		if res.Boundary != maxID+1 {
			t.Fatalf("reconcile Boundary = %d, want %d (from restore_boundary_op_id)", res.Boundary, maxID+1)
		}
		if res.Seq != u.Seq {
			t.Fatalf("reconcile Seq = %d, want %d (from last_restore_seq)", res.Seq, u.Seq)
		}
		if fresh == nil || fresh.Content != reconcileRestored {
			t.Fatalf("reconcile must return the freshly-read restored item; got %+v", fresh)
		}
	})

	// (b) Nothing landed: content unchanged, last_restore_seq never set → both
	// signals say "not landed" → Landed=false, nil item (the genuine-rollback path).
	t.Run("rolled_back", func(t *testing.T) {
		it := reconcileNewItem(t, srv, slug, reconcileOriginal)
		res, fresh, err := srv.reconcileRestoreCommit(it.ID, reconcileRestored, it.Seq, true)
		if err != nil {
			t.Fatalf("reconcile (rolled back) err: %v", err)
		}
		if res.Landed {
			t.Fatal("reconcile: want Landed=false when nothing landed")
		}
		if fresh != nil {
			t.Fatal("reconcile: rolled-back must return a nil item")
		}
	})

	// (c) Signals disagree: content already equals the restore target but
	// last_restore_seq did NOT advance → UNCERTAIN → error → caller stays frozen.
	t.Run("uncertain_signals_disagree", func(t *testing.T) {
		it := reconcileNewItem(t, srv, slug, reconcileRestored)
		res, _, err := srv.reconcileRestoreCommit(it.ID, reconcileRestored, it.Seq, true)
		if err == nil {
			t.Fatalf("reconcile: want an error for ambiguous signals, got Landed=%v", res.Landed)
		}
	})

	// (d) BUG-2276 P1: a NOT-FOUND re-read is UNCERTAIN (error), NOT rolled-back.
	// The restore may have durably landed and a concurrent archive then soft-deleted
	// the item; classifying that as rolled-back would un-freeze stale peers onto the
	// archived item and poison its op-log.
	t.Run("not_found_is_uncertain", func(t *testing.T) {
		res, fresh, err := srv.reconcileRestoreCommit("nonexistent-item-id", reconcileRestored, 0, true)
		if err == nil {
			t.Fatalf("reconcile: a not-found re-read must be UNCERTAIN (error), got Landed=%v", res.Landed)
		}
		if fresh != nil {
			t.Fatal("reconcile: not-found must return a nil item")
		}
	})

	// (e) BUG-2276 P2: a prior restore advanced last_restore_seq + stamped the
	// boundary; THIS attempt then genuinely rolls back choosing identical content.
	// With the baseline captured UNDER the lock (= the post-prior-restore seq),
	// reconcile must NOT falsely classify this rollback as LANDED. (A stale pre-lock
	// baseline below the prior restore's seq WOULD have made seqAdvanced true.)
	t.Run("stale_baseline_prior_restore_not_false_landed", func(t *testing.T) {
		it := reconcileNewItem(t, srv, slug, reconcileOriginal)
		u, _ := runRealRestoreTx(t, srv, it.ID, reconcileRestored) // prior restore R1
		// R2 rolled back: nothing changed since R1 (content==restored,
		// last_restore_seq==u.Seq). The under-lock baseline == u.Seq.
		res, _, err := srv.reconcileRestoreCommit(it.ID, reconcileRestored, u.Seq, true)
		if err == nil && res.Landed {
			t.Fatal("stale-baseline guard: a rollback after a prior restore must NOT be classified LANDED")
		}
	})
}

// TestForceRefreshRoomAckLossReconciledReturnsRestoredItem drives the REAL restore
// tx + REAL reconcile through ForceRefreshRoom with a synthetic ACK loss (the
// commit's tx durably lands, then the commit closure returns an error as if the
// ack were lost at the connection boundary). It proves the corrected BUG-2276 P2
// no-false-404 wiring: on reconciled-LANDED, the reconcile closure captures the
// freshly-read restored item into `updated`, so the handler returns it (200) and
// emits the item_updated SSE — instead of the false 404 it hit before
// (UpdateItemWithPreCheck returns (nil, ackErr) on this path).
func TestForceRefreshRoomAckLossReconciledReturnsRestoredItem(t *testing.T) {
	srv := testServerWithCollab(t)
	slug := createWSWithCollections(t, srv)
	it := reconcileNewItem(t, srv, slug, reconcileOriginal)

	// Mirror handleRestoreItemVersion's wiring: baseline captured under the lock in
	// the precheck; reconcile surfaces the restored item into `updated`.
	var (
		updated          *models.Item
		baselineSeq      int64
		baselineCaptured bool
	)
	content := reconcileRestored
	input := models.ItemUpdate{
		Content:             &content,
		ChangeSummary:       "Restored from an older version",
		LastModifiedBy:      "user",
		Source:              "web",
		ForceVersion:        true,
		MarkRestoreBoundary: true,
	}
	reconcile := func() (collab.RestoreReconcileResult, error) {
		res, fresh, rerr := srv.reconcileRestoreCommit(it.ID, content, baselineSeq, baselineCaptured)
		if rerr == nil && res.Landed {
			updated = fresh
		}
		return res, rerr
	}

	werr := srv.collab.ForceRefreshRoom(it.ID, func() (int64, int64, error) {
		_, uerr := srv.store.UpdateItemWithPreCheck(it.ID, input, func(tx *sql.Tx, existing *models.Item) error {
			baselineSeq = existing.Seq
			baselineCaptured = true
			m, _, merr := srv.store.MaxOpLogIDTx(tx, it.ID)
			if merr != nil {
				return merr
			}
			if serr := srv.store.StampRestoreBoundaryOpIDTx(tx, it.ID, m+1); serr != nil {
				return serr
			}
			return srv.store.PruneItemOpLogTx(tx, it.ID)
		})
		if uerr != nil {
			return 0, 0, uerr
		}
		// The tx committed durably; now simulate the lost ACK. Return an error WITHOUT
		// setting `updated`, exactly as the real UpdateItemWithPreCheck would return
		// (nil, err) on ack loss.
		return 0, 0, errSimAckLoss
	}, reconcile)

	if werr != nil {
		t.Fatalf("ForceRefreshRoom must reconcile the ack-lost-but-landed commit to success, got: %v", werr)
	}
	if updated == nil {
		t.Fatal("BUG-2276 P2: reconciled-LANDED must surface the restored item into `updated` (else the handler false-404s)")
	}
	if updated.Content != reconcileRestored {
		t.Fatalf("updated.Content = %q, want the restored content", updated.Content)
	}
	if b, ok := srv.collab.RestoreBoundary(it.ID); !ok || b <= 0 {
		t.Fatalf("reconciled-LANDED must publish the restore boundary; got (%d, %v)", b, ok)
	}
	if s, ok := srv.collab.LastRestoreSeq(it.ID); !ok || s != updated.Seq {
		t.Fatalf("reconciled-LANDED last restore seq = (%d, %v), want (%d, true)", s, ok, updated.Seq)
	}
}

// TestRestoreItemVersionEmitsItemUpdatedSSE proves the restore handler emits the
// item_updated SSE whenever it produces an updated item — the event path the
// BUG-2276 P2 fix relies on (reconciled-LANDED sets `updated`, so this same emit
// fires). Driven through the real HTTP handler on the normal restore path.
func TestRestoreItemVersionEmitsItemUpdatedSSE(t *testing.T) {
	srv := testServer(t)
	bus := events.New()
	srv.SetEventBus(bus)
	slug := createWSWithCollections(t, srv)

	ws, err := srv.store.GetWorkspaceBySlug(slug)
	if err != nil || ws == nil {
		t.Fatalf("resolve workspace: %v", err)
	}

	// Create v1, then update to v2 so a version bracketing v1 exists to restore.
	it := reconcileNewItem(t, srv, slug, reconcileOriginal)
	time.Sleep(1100 * time.Millisecond)
	rr := doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+it.Slug, map[string]interface{}{
		"content": reconcileRestored,
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("update item: %d: %s", rr.Code, rr.Body.String())
	}

	rr = doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/items/"+it.Slug+"/versions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list versions: %d: %s", rr.Code, rr.Body.String())
	}
	var versions []models.Version
	parseJSON(t, rr, &versions)
	var versionID string
	for _, v := range versions {
		if v.Content == reconcileOriginal {
			versionID = v.ID
			break
		}
	}
	if versionID == "" {
		t.Fatalf("no version with the original content to restore; got %d versions", len(versions))
	}

	// Subscribe right before the restore so the next item_updated is the restore's.
	ch := bus.Subscribe(ws.ID)
	defer bus.Unsubscribe(ch)

	rr = doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+it.Slug+"/versions/"+versionID+"/restore", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore version: %d: %s", rr.Code, rr.Body.String())
	}

	select {
	case ev := <-ch:
		if ev.Type != "item_updated" {
			t.Fatalf("restore emitted SSE type %q, want item_updated", ev.Type)
		}
		if ev.ItemID != it.ID {
			t.Fatalf("restore SSE ItemID = %q, want %q", ev.ItemID, it.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restore did not emit an item_updated SSE event")
	}
}
