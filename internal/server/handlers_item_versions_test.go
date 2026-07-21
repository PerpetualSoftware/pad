package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestReconcileRestoreCommit exercises the Postgres commit-outcome reconciliation
// signal logic (BUG-2276 residual 1) against a real store. After a version-restore
// commit reports an error, reconcileRestoreCommit re-reads the item and decides,
// from two independent durable signals (content == the restored version AND
// last_restore_seq advanced past the pre-restore seq), whether the restore
// actually LANDED, DEFINITELY rolled back, or is UNCERTAIN (signals disagree /
// read error) — the last of which returns an error so ForceRefreshRoom keeps the
// room frozen.
//
// (This runs on the SQLite test store; the helper's LOGIC is dialect-independent —
// it is only WIRED IN on Postgres, where a durably-landed-but-ack-lost commit can
// actually occur. The store gate lives in handleRestoreItemVersion.)
func TestReconcileRestoreCommit(t *testing.T) {
	srv := testServer(t)
	slug := createWSWithCollections(t, srv)

	const original = "original body\n"
	const restored = "restored-from-an-older-version body\n"

	newItem := func(t *testing.T, content string) *models.Item {
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

	// (a) The restore's durable effects are all present on a fresh read → LANDED,
	// with Boundary/Seq recovered from the durable stamps.
	t.Run("landed", func(t *testing.T) {
		it := newItem(t, original)
		preRestoreSeq := it.Seq

		// Simulate the restore's durable tx: content=restored, a new version,
		// last_restore_seq bumped to the new seq, and restore_boundary_op_id stamped.
		content := restored
		updated, err := srv.store.UpdateItem(it.ID, models.ItemUpdate{
			Content:             &content,
			ChangeSummary:       "Restored from an older version",
			LastModifiedBy:      "user",
			Source:              "web",
			ForceVersion:        true,
			MarkRestoreBoundary: true,
		})
		if err != nil {
			t.Fatalf("simulate restore: %v", err)
		}
		if updated.Seq <= preRestoreSeq {
			t.Fatalf("test invariant: restored seq %d must exceed pre-restore seq %d", updated.Seq, preRestoreSeq)
		}
		const boundary = int64(1234)
		if err := srv.store.SetItemRestoreBoundaryOpID(it.ID, boundary); err != nil {
			t.Fatalf("stamp boundary: %v", err)
		}

		res, err := srv.reconcileRestoreCommit(it.ID, restored, preRestoreSeq)
		if err != nil {
			t.Fatalf("reconcile (landed) err: %v", err)
		}
		if !res.Landed {
			t.Fatal("reconcile: want Landed=true for a durably-landed restore")
		}
		if res.Boundary != boundary {
			t.Fatalf("reconcile Boundary = %d, want %d (from restore_boundary_op_id)", res.Boundary, boundary)
		}
		if res.Seq != updated.Seq {
			t.Fatalf("reconcile Seq = %d, want %d (from last_restore_seq)", res.Seq, updated.Seq)
		}
	})

	// (b) Nothing landed: content unchanged, last_restore_seq never set → both
	// signals say "not landed" → Landed=false (the genuine-rollback path).
	t.Run("rolled_back", func(t *testing.T) {
		it := newItem(t, original)
		res, err := srv.reconcileRestoreCommit(it.ID, restored, it.Seq)
		if err != nil {
			t.Fatalf("reconcile (rolled back) err: %v", err)
		}
		if res.Landed {
			t.Fatal("reconcile: want Landed=false when nothing landed")
		}
	})

	// (c) Signals disagree: content already equals the restore target but
	// last_restore_seq did NOT advance (the rare restore-to-identical-content tx
	// that also ack-lost). Must be UNCERTAIN → error → caller stays frozen.
	t.Run("uncertain_signals_disagree", func(t *testing.T) {
		it := newItem(t, restored)
		res, err := srv.reconcileRestoreCommit(it.ID, restored, it.Seq)
		if err == nil {
			t.Fatalf("reconcile: want an error for ambiguous signals, got Landed=%v", res.Landed)
		}
	})

	// A missing item on the fresh read is treated as NOT landed (un-freeze).
	t.Run("item_gone", func(t *testing.T) {
		res, err := srv.reconcileRestoreCommit("nonexistent-item-id", restored, 0)
		if err != nil {
			t.Fatalf("reconcile (gone) err: %v", err)
		}
		if res.Landed {
			t.Fatal("reconcile: a missing item is NOT landed")
		}
	})
}
