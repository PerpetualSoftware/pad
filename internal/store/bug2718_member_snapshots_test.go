package store

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2718: outboxMemberSnapshotsTx reads its members in batches instead of
// one joined read per member. What it returns must be exactly what the
// per-member version returned: de-duplicated in FIRST-OCCURRENCE order, rows
// that no longer resolve (archived, never existed) skipped, every snapshot
// scrubbed — across a chunk boundary, since a large rename spans several
// batches. The expected value is built with the per-member reader itself
// (getItemTx + scrubItemPII), so the comparison is against the old behaviour,
// not a restatement of the new code.
func TestOutboxMemberSnapshotsMatchPerMemberReads(t *testing.T) {
	s := testStore(t)
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Members"})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	coll, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TSK", Schema: `{"fields":[]}`})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	u, err := s.CreateUser(models.UserCreate{Email: "member-2718@example.com", Name: "Assignee", Password: "correct-horse-battery-staple"})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.AddWorkspaceMember(ws.ID, u.ID, "editor"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}

	// More than one chunk's worth, so a batch that stopped after its first
	// chunk, or dropped the last partial one, would lose members.
	n := outboxMemberChunk + 5
	var ids []string
	for i := 0; i < n; i++ {
		it, err := s.CreateItem(ws.ID, coll.ID, models.ItemCreate{Title: fmt.Sprintf("m%03d", i), Fields: `{}`})
		if err != nil {
			t.Fatalf("CreateItem %d: %v", i, err)
		}
		ids = append(ids, it.ID)
	}
	// An assignee, so the scrub has something to remove.
	if _, err := s.UpdateItem(ids[1], models.ItemUpdate{AssignedUserID: &u.ID}); err != nil {
		t.Fatalf("assign: %v", err)
	}
	// An archived member, which must be skipped.
	if err := s.DeleteItem(ids[2]); err != nil {
		t.Fatalf("DeleteItem: %v", err)
	}

	// Reverse order (so first-occurrence order is not the insert order),
	// duplicates on both sides of the chunk boundary, and an id that never
	// existed.
	var input []string
	for i := len(ids) - 1; i >= 0; i-- {
		input = append(input, ids[i])
	}
	input = append(input, ids[0], ids[n-1], "00000000-0000-0000-0000-000000002718", ids[1])

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	got, err := s.outboxMemberSnapshotsTx(tx, input)
	if err != nil {
		t.Fatalf("outboxMemberSnapshotsTx: %v", err)
	}

	var want []*models.Item
	seen := map[string]bool{}
	for _, id := range input {
		if seen[id] {
			continue
		}
		seen[id] = true
		item, err := s.getItemTx(tx, id)
		if err != nil {
			t.Fatalf("getItemTx %s: %v", id, err)
		}
		if item == nil {
			continue
		}
		want = append(want, scrubItemPII(item))
	}

	if len(want) != n-1 {
		t.Fatalf("control: expected %d live unique members, the per-member reader found %d", n-1, len(want))
	}
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("batched snapshots differ from per-member reads: got %d members, want %d", len(got), len(want))
		for i := range min(len(got), len(want)) {
			if got[i].ID != want[i].ID {
				t.Errorf("first divergence at %d: got %s, want %s", i, got[i].ID, want[i].ID)
				break
			}
		}
	}
	for _, m := range got {
		if m.ID == ids[1] && (m.AssignedUserName != "" || m.AssignedUserEmail != "") {
			t.Errorf("member %s was not scrubbed: %q / %q", m.ID, m.AssignedUserName, m.AssignedUserEmail)
		}
	}
}
