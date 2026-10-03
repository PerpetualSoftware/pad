package store

import (
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3365: account deletion reports the workspaces of the grants it
// deleted, from the deleted rows themselves (codex r2 on PR2).
func TestDeleteAccountAtomicReport_IssuedGrantWorkspaces(t *testing.T) {
	s := testStore(t)
	issuer := createTestUser(t, s, "issuer@test.com", "Issuer", "password123")
	other := createTestUser(t, s, "other@test.com", "Other", "password123")
	grantee := createTestUser(t, s, "grantee@test.com", "Grantee", "password123")
	mkWS := func(name string) *models.Workspace {
		ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: name, OwnerID: other.ID})
		if err != nil {
			t.Fatal(err)
		}
		return ws
	}
	w1, w2, w3 := mkWS("W1"), mkWS("W2"), mkWS("W3")
	c1 := createTestCollection(t, s, w1.ID, "Things")
	c2 := createTestCollection(t, s, w2.ID, "Things")
	c3 := createTestCollection(t, s, w3.ID, "Things")
	it2 := createTestItem(t, s, w2.ID, c2.ID, "Item", "")
	if _, err := s.CreateCollectionGrant(w1.ID, c1.ID, grantee.ID, "view", issuer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateItemGrant(w2.ID, it2.ID, grantee.ID, "view", issuer.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateCollectionGrant(w3.ID, c3.ID, grantee.ID, "view", other.ID); err != nil {
		t.Fatal(err) // issued by someone else: not reported
	}
	got, err := s.DeleteAccountAtomicReport(issuer.ID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := []string{w1.ID, w2.ID}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("issued-grant workspaces = %v, want %v", got, want)
	}
}
