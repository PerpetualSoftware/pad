package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TASK-3397 (U8b2): the upgrade transaction's lock order.

func task3397UpgradeFix(t *testing.T, installID string) (task3394Fix, UpgradeRequest) {
	t.Helper()
	f := task3397LifecycleFix(t, installID)
	owner := createTestUser(t, f.s, "upgrade-owner-"+installID+"@example.com", "Owner", "correct-horse-battery")
	if err := f.s.AddWorkspaceMember(f.ws.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	const from = "1111111111111111111111111111111111111111111111111111111111111111"
	const to = "2222222222222222222222222222222222222222222222222222222222222222"
	if _, err := f.s.db.Exec(f.s.q(`UPDATE app_installs SET manifest_sha256 = ?, service_access = 'write', digests = '{}' WHERE id = ?`), from, f.installID); err != nil {
		t.Fatal(err)
	}
	p, err := f.s.ReservePendingUpgrade(f.ws.ID, owner.ID, "https://portal.example", f.installID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.FinishPendingInstall(p.ID, to, "{}"); err != nil {
		t.Fatal(err)
	}
	return f, UpgradeRequest{PendingID: p.ID, WorkspaceID: f.ws.ID, OwnerID: owner.ID, InstallID: f.installID,
		ManifestSHA256: to, FromManifestSHA256: from}
}

func task3397Plan(inst *InstallUpgradeState) *UpgradePlan {
	return &UpgradePlan{ManifestSHA256: "2222222222222222222222222222222222222222222222222222222222222222", ManifestVersion: "1.1.0",
		ManifestJSON: "{}", DigestsJSON: "{}", ServiceAccess: inst.ServiceAccess, DelegatedAccess: inst.DelegatedAccess,
		SourcePack: "https://portal.example@1.1.0", Renamed: map[string]string{}}
}

// A fenced app write holds the install row FOR SHARE and then takes the
// workspace lock (an item create). The upgrade takes the install row BEFORE
// the workspace lock, so the two queue rather than deadlock; the reverse
// order is a cycle (the red check).
func TestTask3397_UpgradeDoesNotDeadlockWithAFencedWrite(t *testing.T) {
	f, req := task3397UpgradeFix(t, "inst-up-fence")
	epoch := task3397Epoch(t, f.s, f.installID)
	ftx, err := f.s.BeginFenced(context.Background(), FenceSpec{InstallID: f.installID, WorkspaceID: f.ws.ID, Epoch: epoch})
	if err != nil {
		t.Fatalf("fence: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) { return task3397Plan(inst), nil })
		done <- err
	}()
	time.Sleep(700 * time.Millisecond) // the upgrade is waiting on the install row (or, on SQLite, on BEGIN)
	lockErr := f.s.acquireWorkspaceSeqLock(ftx.tx, f.ws.ID)
	_ = ftx.Rollback()
	if lockErr != nil {
		<-done
		t.Fatalf("the fenced write could not take the workspace lock (a deadlock victim?): %v", lockErr)
	}
	select {
	case err := <-done:
		if err != nil && strings.Contains(err.Error(), "40P01") {
			t.Fatalf("the upgrade was a deadlock victim: %v", err)
		}
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the upgrade never finished")
	}
}

// The install moved since the preview (its manifest hash, or its state): the
// upgrade refuses and writes nothing.
func TestTask3397_UpgradeRefusesAMovedInstall(t *testing.T) {
	for name, move := range map[string]string{
		"manifest": `UPDATE app_installs SET manifest_sha256 = '3333333333333333333333333333333333333333333333333333333333333333' WHERE id = ?`,
		"state":    `UPDATE app_installs SET state = 'uninstalled' WHERE id = ?`,
	} {
		t.Run(name, func(t *testing.T) {
			f, req := task3397UpgradeFix(t, "inst-up-moved-"+name)
			if _, err := f.s.db.Exec(f.s.q(move), f.installID); err != nil {
				t.Fatal(err)
			}
			called := false
			_, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) {
				called = true
				return task3397Plan(inst), nil
			})
			if !errors.Is(err, ErrInstallMoved) || called {
				t.Fatalf("got %v (derived %v), want ErrInstallMoved before any derivation", err, called)
			}
		})
	}
}

// The store refuses to provision an upgrade's pending record as a fresh
// install, whatever the caller (the handler refuses first; this is the
// transaction's own guard).
func TestTask3397_ProvisionRefusesAnUpgradeRecord(t *testing.T) {
	f, req := task3397UpgradeFix(t, "inst-up-cross")
	_, err := f.s.ProvisionAppInstall(ProvisionRequest{
		PendingID: req.PendingID, WorkspaceID: req.WorkspaceID, OwnerID: req.OwnerID,
		Origin: "https://portal.example", ManifestSHA256: req.ManifestSHA256,
	}, func(Queryer) (*ProvisionDerived, error) { return &ProvisionDerived{}, nil })
	if !errors.Is(err, ErrPendingNotStaged) {
		t.Fatalf("got %v, want ErrPendingNotStaged", err)
	}
}

// The transaction never skips a release that does not resolve: a plan naming
// a companion slug that is not this install's is refused, not ignored (the
// derivation's companion check normally catches it first; this is the
// transaction's own backstop).
func TestTask3397_UpgradeNeverSkipsAnUnresolvedRelease(t *testing.T) {
	f, req := task3397UpgradeFix(t, "inst-up-release")
	_, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) {
		p := task3397Plan(inst)
		p.Released = []string{"not-a-companion"}
		p.Restrictive = true
		return p, nil
	})
	var pc *ProvisionConflictError
	if !errors.As(err, &pc) || pc.Collection != "not-a-companion" {
		t.Fatalf("got %v, want a conflict naming the unresolved companion", err)
	}
}
