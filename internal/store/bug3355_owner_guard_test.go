package store

import (
	"errors"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3355: both removal doors (with and without revoking grants) and the
// role door refuse the canonical owner and the last owner member.
func TestBUG3355_StoreRemovalGuards(t *testing.T) {
	removers := map[string]func(s *Store, ws, user string) error{
		"remove":               (*Store).RemoveWorkspaceMember,
		"remove-revoke-grants": (*Store).RemoveWorkspaceMemberAndRevokeGrants,
		"demote-to-editor":     func(s *Store, ws, user string) error { return s.UpdateWorkspaceMemberRole(ws, user, "editor") },
	}
	for name, remove := range removers {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			canon := createTestUser(t, s, "canon@test.com", "Canon", "password123")
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Guard WS", OwnerID: canon.ID})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if err := s.AddWorkspaceMember(ws.ID, canon.ID, "owner"); err != nil {
				t.Fatalf("add canonical owner: %v", err)
			}
			co := createTestUser(t, s, "co@test.com", "Co", "password123")
			if err := s.AddWorkspaceMember(ws.ID, co.ID, "owner"); err != nil {
				t.Fatalf("add co-owner: %v", err)
			}

			if err := remove(s, ws.ID, canon.ID); !errors.Is(err, ErrCanonicalOwner) {
				t.Fatalf("canonical owner: got %v, want ErrCanonicalOwner", err)
			}

			// Legacy state: the canonical owner is not an owner member, so co
			// is the last owner and may not leave.
			if _, err := s.db.Exec(s.q(`UPDATE workspace_members SET role = 'editor' WHERE workspace_id = ? AND user_id = ?`), ws.ID, canon.ID); err != nil {
				t.Fatalf("set up legacy state: %v", err)
			}
			if err := remove(s, ws.ID, co.ID); !errors.Is(err, ErrLastOwner) {
				t.Fatalf("last owner: got %v, want ErrLastOwner", err)
			}

			// Control: with a second owner present, co may leave.
			other := createTestUser(t, s, "other@test.com", "Other", "password123")
			if err := s.AddWorkspaceMember(ws.ID, other.ID, "owner"); err != nil {
				t.Fatalf("add second owner: %v", err)
			}
			if err := remove(s, ws.ID, co.ID); err != nil {
				t.Fatalf("with another owner present: %v", err)
			}
		})
	}
}
