package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3601: a limit check names its payer: the workspace owner for a
// workspace limit, the user for a user limit.
func TestTask3601_LimitChecksNameThePayer(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	owner := createTestUser(t, s, "payer3601@example.com", "Dana", "s3cret")
	if _, err := s.SetUserPlan(owner.ID, PlanWrite{Plan: "free", Source: PlanSourceManual, Force: true}); err != nil {
		t.Fatal(err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Payer 3601", OwnerID: owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	wl, err := s.CheckLimit(ws.ID, "members_per_workspace")
	if err != nil {
		t.Fatal(err)
	}
	if wl.PayerID != owner.ID || wl.PayerName != "Dana" {
		t.Errorf("workspace limit payer = %q/%q, want %q/Dana", wl.PayerID, wl.PayerName, owner.ID)
	}
	ul, err := s.CheckUserLimit(owner.ID, "workspaces")
	if err != nil {
		t.Fatal(err)
	}
	if ul.PayerID != owner.ID {
		t.Errorf("user limit payer = %q, want %q", ul.PayerID, owner.ID)
	}
}
