package store

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3414 (SPEC-6 U11): every mutation that can invalidate a context code
// waits for a redeem holding the §6 locks (install, item, action), and a
// redeem that runs after it is refused. Deterministic: the redeem pauses
// with its locks held while the mutation is started.

type u11StoreFix struct {
	task3408Fix
	item     *models.Item
	actionID string
}

func u11StoreFixture(t *testing.T, installID string) u11StoreFix {
	t.Helper()
	base := task3408Fixture(t, installID)
	f := u11StoreFix{task3408Fix: base}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET manifest = ? WHERE id = ?`), `{"title":"Portal","base_url":"https://portal.example"}`, installID); err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.syncAppItemActionsTx(tx, f.ws.ID, installID, []AppActionSpec{{Key: "open", Label: "Open", Path: "/t", CollectionSlugs: []string{f.companion.Slug}}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := f.s.db.QueryRow(f.s.q(`SELECT id FROM app_item_actions WHERE install_id = ? AND action_key = 'open'`), installID).Scan(&f.actionID); err != nil {
		t.Fatal(err)
	}
	if f.item, err = f.s.CreateItem(f.ws.ID, f.companion.ID, models.ItemCreate{Title: "Ticket", Fields: `{}`}); err != nil {
		t.Fatal(err)
	}
	return f
}

func allVisible(Queryer, *models.Item, string) (bool, error) { return true, nil }

func (f u11StoreFix) epoch(t *testing.T) int64 {
	t.Helper()
	var e int64
	if err := f.s.db.QueryRow(f.s.q(`SELECT auth_epoch FROM app_installs WHERE id = ?`), f.installID).Scan(&e); err != nil {
		t.Fatal(err)
	}
	return e
}

// raceRedeem mints a code, then redeems it while mutate runs from inside the
// redeem's locks. mutate must still be blocked when the redeem's pause ends.
func (f u11StoreFix) raceRedeem(t *testing.T, mutate func() error) (*RedeemedContext, error) {
	t.Helper()
	minted, err := f.s.MintContextCode(f.ws.ID, f.installID, "open", f.item.ID, "viewer-1", allVisible)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	done := make(chan error, 1)
	var finishedDuringRedeem bool
	redeemContextHook = func() {
		go func() { done <- mutate() }()
		select {
		case err := <-done:
			finishedDuringRedeem = true
			done <- err
		case <-time.After(400 * time.Millisecond):
		}
	}
	t.Cleanup(func() { redeemContextHook = nil })
	red, err := f.s.RedeemContextCode(f.installID, f.epoch(t), minted.Code, allVisible)
	redeemContextHook = nil
	if mErr := <-done; mErr != nil {
		t.Fatalf("mutation: %v", mErr)
	}
	if finishedDuringRedeem {
		t.Fatal("the mutation completed while the redeem held its locks")
	}
	return red, err
}

func TestTask3414_InvalidatingMutationsWaitForARedeem(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f u11StoreFix) error
	}{
		{"disable", func(f u11StoreFix) error {
			return f.s.BeginInstallTeardown(f.ws.ID, f.installID, TeardownDisable)
		}},
		{"upgrade changes the action", func(f u11StoreFix) error {
			tx, err := f.s.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err := f.s.lockInstallTx(tx, f.ws.ID, f.installID); err != nil {
				return err
			}
			if err := f.s.syncAppItemActionsTx(tx, f.ws.ID, f.installID, []AppActionSpec{{Key: "open", Label: "Open v2", Path: "/t", CollectionSlugs: []string{f.companion.Slug}}}); err != nil {
				return err
			}
			return tx.Commit()
		}},
		{"item moved", func(f u11StoreFix) error {
			_, err := f.s.MoveItem(f.item.ID, f.other.ID, `{}`)
			return err
		}},
		{"item deleted", func(f u11StoreFix) error {
			return f.s.DeleteItem(f.item.ID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := u11StoreFixture(t, "inst-race-"+tc.name[:4])
			red, err := f.raceRedeem(t, func() error { return tc.mutate(f) })
			// The redeem ran first: it saw the pre-mutation state.
			if err != nil || red == nil || red.Item.ID != f.item.ID {
				t.Fatalf("redeem ahead of the mutation: %+v %v", red, err)
			}
			// After the mutation, a fresh code is refused.
			if _, err := f.s.MintContextCode(f.ws.ID, f.installID, "open", f.item.ID, "viewer-1", allVisible); err == nil {
				// A move to a non-companion, delete, or disable refuses the
				// mint; the action change keeps it mintable at the new
				// revision, so redeem an OLD code instead below.
				if tc.name != "upgrade changes the action" {
					t.Fatalf("mint after %s was accepted", tc.name)
				}
			}
		})
	}
}

// A code minted before an action change is refused after it.
func TestTask3414_CodeFromBeforeAnUpgradeIsRefused(t *testing.T) {
	f := u11StoreFixture(t, "inst-oldcode")
	minted, err := f.s.MintContextCode(f.ws.ID, f.installID, "open", f.item.ID, "viewer-1", allVisible)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := f.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.syncAppItemActionsTx(tx, f.ws.ID, f.installID, []AppActionSpec{{Key: "open", Label: "Open v2", Path: "/t", CollectionSlugs: []string{f.companion.Slug}}}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RedeemContextCode(f.installID, f.epoch(t), minted.Code, allVisible); err != ErrContextCodeRefused {
		t.Fatalf("redeem of a pre-upgrade code: %v", err)
	}
}

// The backfill re-decides under the install lock: an install that is no
// longer active or inactive gets nothing, even if the work list said so.
func TestTask3414_BackfillSkipsAnUninstalledInstall(t *testing.T) {
	f := u11StoreFixture(t, "inst-bf")
	if _, err := f.s.db.Exec(f.s.q(`DELETE FROM app_item_actions WHERE install_id = ?`), f.installID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET state = 'uninstalled' WHERE id = ?`), f.installID); err != nil {
		t.Fatal(err)
	}
	specs := func(string) ([]AppActionSpec, error) {
		return []AppActionSpec{{Key: "open", Label: "Open", Path: "/t", CollectionSlugs: []string{f.companion.Slug}}}, nil
	}
	if err := f.s.EnsureAppItemActions(f.ws.ID, f.installID, specs); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.db.QueryRow(f.s.q(`SELECT COUNT(*) FROM app_item_actions WHERE install_id = ?`), f.installID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("%d action rows backfilled for an uninstalled install", n)
	}
}

// Store-level guards the HTTP routing happens to shadow: a mint for another
// workspace than the install's, and a redeem in a soft-deleted workspace.
func TestTask3414_StoreGuardsWorkspace(t *testing.T) {
	f := u11StoreFixture(t, "inst-wsg")
	if _, err := f.s.MintContextCode("some-other-workspace", f.installID, "open", f.item.ID, "viewer-1", allVisible); err != ErrContextCodeRefused {
		t.Fatalf("mint for another workspace: %v", err)
	}
	minted, err := f.s.MintContextCode(f.ws.ID, f.installID, "open", f.item.ID, "viewer-1", allVisible)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.db.Exec(f.s.q(`UPDATE workspaces SET deleted_at = ? WHERE id = ?`), now(), f.ws.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RedeemContextCode(f.installID, f.epoch(t), minted.Code, allVisible); err != ErrContextCodeRefused {
		t.Fatalf("redeem in a deleted workspace: %v", err)
	}
}
