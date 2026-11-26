package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3691: ProvisionAppInstall and UpgradeAppInstall must enforce the
// items_per_workspace plan limit when passed WithPlanLimit(), aborting the
// entire transaction on refusal so nothing is written and the pending record
// stays staged.

func bug3691SetWorkspacePlanCap(t *testing.T, s *Store, workspaceID, ownerID string, cap int) {
	t.Helper()
	if _, err := s.db.Exec(s.q(`UPDATE workspaces SET owner_id = ? WHERE id = ?`), ownerID, workspaceID); err != nil {
		t.Fatalf("set workspace owner_id: %v", err)
	}
	if _, err := s.SetUserPlan(ownerID, PlanWrite{Plan: "free", Source: PlanSourceManual, Force: true}); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	if err := s.SetUserPlanOverrides(ownerID, fmt.Sprintf(`{"items_per_workspace":%d}`, cap)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
}

func bug3691LiveItems(t *testing.T, s *Store, workspaceID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND deleted_at IS NULL`), workspaceID).Scan(&n); err != nil {
		t.Fatalf("count live items: %v", err)
	}
	return n
}

func bug3691Count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(s.q(query), args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func bug3691TwoArtifactDerived(f task3397Fix) *ProvisionDerived {
	return &ProvisionDerived{
		Collections: []ProvisionCollection{
			{Key: "t", Slug: "portal-t", Name: "Tickets", Schema: `{"fields":[]}`},
		},
		Artifacts: []ProvisionArtifact{
			{
				Key: "a1", CollectionID: f.dest.ID, Title: "Artifact One",
				Fields: map[string]any{"owner": f.target.ID}, RawSHA256: "raw1", NormalizedSHA256: "norm1",
			},
			{
				Key: "a2", CollectionID: f.dest.ID, Title: "Artifact Two",
				Fields: map[string]any{"owner": f.target.ID}, RawSHA256: "raw2", NormalizedSHA256: "norm2",
			},
		},
	}
}

func TestBUG3691_ProvisionAppInstall_PlanLimit_RoomForOneOfTwo_RefusedAndRolledBack(t *testing.T) {
	t.Parallel()
	f := task3397Fixture(t)
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	collsBefore := bug3691Count(t, f.s, `SELECT COUNT(*) FROM collections WHERE workspace_id = ?`, f.ws.ID)
	// Room for exactly one item while the manifest carries two artifacts:
	// the second artifact insert must fail and roll back the entire transaction.
	limit := itemsBefore + 1
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, f.owner.ID, limit)

	res, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) {
		return bug3691TwoArtifactDerived(f), nil
	}, WithPlanLimit())
	var ple *PlanLimitError
	if !errors.As(err, &ple) {
		t.Fatalf("ProvisionAppInstall err = %v (res=%+v), want *PlanLimitError", err, res)
	}
	if ple.Result.Feature != "items_per_workspace" || ple.Result.Limit != limit || ple.Result.Current != limit {
		t.Errorf("PlanLimitError result = %+v, want items_per_workspace %d/%d", ple.Result, limit, limit)
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != itemsBefore {
		t.Errorf("live item count = %d, want unchanged %d (rollback failed)", got, itemsBefore)
	}
	if got := bug3691Count(t, f.s, `SELECT COUNT(*) FROM app_installs WHERE workspace_id = ?`, f.ws.ID); got != 0 {
		t.Errorf("app_installs count = %d, want 0", got)
	}
	if got := bug3691Count(t, f.s, `SELECT COUNT(*) FROM collections WHERE workspace_id = ?`, f.ws.ID); got != collsBefore {
		t.Errorf("collections count = %d, want unchanged %d", got, collsBefore)
	}
	var pendingState string
	if err := f.s.db.QueryRow(f.s.q(`SELECT state FROM app_install_pending WHERE id = ?`), f.req.PendingID).Scan(&pendingState); err != nil {
		t.Fatalf("pending record missing after rollback: %v", err)
	}
	if pendingState != "staged" {
		t.Errorf("pending state = %q, want staged", pendingState)
	}
}

func TestBUG3691_ProvisionAppInstall_PlanLimit_RoomForTwo_ControlSucceeds(t *testing.T) {
	t.Parallel()
	f := task3397Fixture(t)
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	limit := itemsBefore + 2
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, f.owner.ID, limit)

	res, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) {
		return bug3691TwoArtifactDerived(f), nil
	}, WithPlanLimit())
	if err != nil {
		t.Fatalf("ProvisionAppInstall with room for two failed: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("created %d items, want 2", len(res.Items))
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != limit {
		t.Errorf("live item count = %d, want %d", got, limit)
	}
	if got := bug3691Count(t, f.s, `SELECT COUNT(*) FROM app_install_pending WHERE id = ?`, f.req.PendingID); got != 0 {
		t.Errorf("pending record left behind (%d)", got)
	}
}

func TestBUG3691_ProvisionAppInstall_WithoutPlanLimit_OverCapSucceeds(t *testing.T) {
	t.Parallel()
	f := task3397Fixture(t)
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	// Already at the cap, but self-hosted passes no WithPlanLimit().
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, f.owner.ID, itemsBefore)

	res, err := f.s.ProvisionAppInstall(f.req, func(Queryer) (*ProvisionDerived, error) {
		return bug3691TwoArtifactDerived(f), nil
	})
	if err != nil {
		t.Fatalf("ProvisionAppInstall without WithPlanLimit failed: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("created %d items, want 2", len(res.Items))
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != itemsBefore+2 {
		t.Errorf("live item count = %d, want %d", got, itemsBefore+2)
	}
}

func bug3691UpgradeWithDest(t *testing.T, installID string) (task3394Fix, UpgradeRequest, *models.Collection) {
	t.Helper()
	f, req := task3397UpgradeFix(t, installID)
	dest, err := f.s.CreateCollection(f.ws.ID, models.CollectionCreate{Name: "Playbooks", Slug: "playbooks-" + installID})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	if _, err := f.s.CreateItem(f.ws.ID, dest.ID, models.ItemCreate{Title: "Existing item"}); err != nil {
		t.Fatalf("CreateItem: %v", err)
	}
	return f, req, dest
}

func TestBUG3691_UpgradeAppInstall_PlanLimit_AtCapWithArtifact_RefusedAndRolledBack(t *testing.T) {
	t.Parallel()
	f, req, dest := bug3691UpgradeWithDest(t, "inst-up-cap-refuse")
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, req.OwnerID, itemsBefore)

	items, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) {
		p := task3397Plan(inst)
		p.Artifacts = []ProvisionArtifact{{
			Key: "new-draft", CollectionID: dest.ID, Title: "Upgraded Draft",
			Fields: map[string]any{}, RawSHA256: "raw-up", NormalizedSHA256: "norm-up",
		}}
		return p, nil
	}, WithPlanLimit())
	var ple *PlanLimitError
	if !errors.As(err, &ple) {
		t.Fatalf("UpgradeAppInstall err = %v (items=%+v), want *PlanLimitError", err, items)
	}
	if ple.Result.Feature != "items_per_workspace" || ple.Result.Limit != itemsBefore || ple.Result.Current != itemsBefore {
		t.Errorf("PlanLimitError result = %+v, want items_per_workspace %d/%d", ple.Result, itemsBefore, itemsBefore)
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != itemsBefore {
		t.Errorf("live item count = %d, want unchanged %d", got, itemsBefore)
	}
	var manifestSHA string
	if err := f.s.db.QueryRow(f.s.q(`SELECT manifest_sha256 FROM app_installs WHERE id = ?`), f.installID).Scan(&manifestSHA); err != nil {
		t.Fatalf("read install manifest_sha256: %v", err)
	}
	if manifestSHA != req.FromManifestSHA256 {
		t.Errorf("install manifest_sha256 = %q, want old %q", manifestSHA, req.FromManifestSHA256)
	}
	var pendingState string
	if err := f.s.db.QueryRow(f.s.q(`SELECT state FROM app_install_pending WHERE id = ?`), req.PendingID).Scan(&pendingState); err != nil {
		t.Fatalf("pending upgrade missing after rollback: %v", err)
	}
	if pendingState != "staged" {
		t.Errorf("pending state = %q, want staged", pendingState)
	}
}

func TestBUG3691_UpgradeAppInstall_PlanLimit_UnderCapWithArtifact_ControlSucceeds(t *testing.T) {
	t.Parallel()
	f, req, dest := bug3691UpgradeWithDest(t, "inst-up-cap-ok")
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	limit := itemsBefore + 1
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, req.OwnerID, limit)

	items, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) {
		p := task3397Plan(inst)
		p.Artifacts = []ProvisionArtifact{{
			Key: "new-draft", CollectionID: dest.ID, Title: "Upgraded Draft",
			Fields: map[string]any{}, RawSHA256: "raw-up", NormalizedSHA256: "norm-up",
		}}
		return p, nil
	}, WithPlanLimit())
	if err != nil {
		t.Fatalf("UpgradeAppInstall one under cap failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("created %d items, want 1", len(items))
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != limit {
		t.Errorf("live item count = %d, want %d", got, limit)
	}
	var manifestSHA string
	if err := f.s.db.QueryRow(f.s.q(`SELECT manifest_sha256 FROM app_installs WHERE id = ?`), f.installID).Scan(&manifestSHA); err != nil {
		t.Fatalf("read install manifest_sha256: %v", err)
	}
	if manifestSHA != req.ManifestSHA256 {
		t.Errorf("install manifest_sha256 = %q, want new %q", manifestSHA, req.ManifestSHA256)
	}
}

func TestBUG3691_UpgradeAppInstall_PlanLimit_AtCapNoArtifact_Succeeds(t *testing.T) {
	t.Parallel()
	f, req, _ := bug3691UpgradeWithDest(t, "inst-up-cap-noitem")
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	// At the cap, but this upgrade inserts no new artifacts: must not be refused.
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, req.OwnerID, itemsBefore)

	items, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) {
		return task3397Plan(inst), nil
	}, WithPlanLimit())
	if err != nil {
		t.Fatalf("UpgradeAppInstall with no new items at cap failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("created %d items, want 0", len(items))
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != itemsBefore {
		t.Errorf("live item count = %d, want unchanged %d", got, itemsBefore)
	}
	var manifestSHA string
	if err := f.s.db.QueryRow(f.s.q(`SELECT manifest_sha256 FROM app_installs WHERE id = ?`), f.installID).Scan(&manifestSHA); err != nil {
		t.Fatalf("read install manifest_sha256: %v", err)
	}
	if manifestSHA != req.ManifestSHA256 {
		t.Errorf("install manifest_sha256 = %q, want new %q", manifestSHA, req.ManifestSHA256)
	}
}

func TestBUG3691_UpgradeAppInstall_WithoutPlanLimit_AtCapWithArtifact_Succeeds(t *testing.T) {
	t.Parallel()
	f, req, dest := bug3691UpgradeWithDest(t, "inst-up-selfhost")
	itemsBefore := bug3691LiveItems(t, f.s, f.ws.ID)
	bug3691SetWorkspacePlanCap(t, f.s, f.ws.ID, req.OwnerID, itemsBefore)

	items, err := f.s.UpgradeAppInstall(req, func(_ Queryer, inst *InstallUpgradeState) (*UpgradePlan, error) {
		p := task3397Plan(inst)
		p.Artifacts = []ProvisionArtifact{{
			Key: "new-draft", CollectionID: dest.ID, Title: "Upgraded Draft",
			Fields: map[string]any{}, RawSHA256: "raw-up", NormalizedSHA256: "norm-up",
		}}
		return p, nil
	})
	if err != nil {
		t.Fatalf("UpgradeAppInstall without WithPlanLimit failed: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("created %d items, want 1", len(items))
	}
	if got := bug3691LiveItems(t, f.s, f.ws.ID); got != itemsBefore+1 {
		t.Errorf("live item count = %d, want %d", got, itemsBefore+1)
	}
}
