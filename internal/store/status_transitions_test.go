package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// listTransitions returns all status_transitions rows for an item, oldest
// first.
func listTransitions(t *testing.T, s *Store, itemID string) []models.StatusTransition {
	t.Helper()
	rows, err := s.db.Query(s.q(`
		SELECT id, item_id, workspace_id, collection_id, from_status, to_status, created_at
		FROM status_transitions WHERE item_id = ? ORDER BY created_at, id
	`), itemID)
	if err != nil {
		t.Fatalf("query transitions: %v", err)
	}
	defer rows.Close()
	var out []models.StatusTransition
	for rows.Next() {
		var st models.StatusTransition
		if err := rows.Scan(&st.ID, &st.ItemID, &st.WorkspaceID, &st.CollectionID, &st.FromStatus, &st.ToStatus, &st.CreatedAt); err != nil {
			t.Fatalf("scan transition: %v", err)
		}
		out = append(out, st)
	}
	return out
}

func newTransitionTestWorkspace(t *testing.T, s *Store) (workspaceID, collectionID string) {
	t.Helper()
	u, err := s.CreateUser(models.UserCreate{Name: "Tess", Email: "tess@example.com"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Trans", Slug: "trans", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	col := createTestCollection(t, s, ws.ID, "Tasks")
	return ws.ID, col.ID
}

func TestStatusTransition_CapturedOnStatusChange(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Do a thing", "")

	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"done"}`)}); err != nil {
		t.Fatalf("update item: %v", err)
	}

	trans := listTransitions(t, s, item.ID)
	if len(trans) != 1 {
		t.Fatalf("expected 1 transition, got %d", len(trans))
	}
	got := trans[0]
	if got.FromStatus != "open" || got.ToStatus != "done" {
		t.Fatalf("expected open → done, got %q → %q", got.FromStatus, got.ToStatus)
	}
	if got.WorkspaceID != wsID || got.CollectionID != colID {
		t.Fatalf("transition not stamped with workspace/collection: ws=%q col=%q", got.WorkspaceID, got.CollectionID)
	}
}

func TestStatusTransition_MultiHopEachRecorded(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Multi", "")

	for _, st := range []string{"in-progress", "done"} {
		if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"` + st + `"}`)}); err != nil {
			t.Fatalf("update to %s: %v", st, err)
		}
	}

	trans := listTransitions(t, s, item.ID)
	if len(trans) != 2 {
		t.Fatalf("expected 2 transitions, got %d", len(trans))
	}
	// Both updates can land in the same wall-clock second (now() is
	// second-resolution), so row order isn't guaranteed — assert the SET of
	// hops instead. Each hop must still record the correct from→to pair.
	hops := map[string]string{} // to -> from
	for _, st := range trans {
		hops[st.ToStatus] = st.FromStatus
	}
	if from, ok := hops["in-progress"]; !ok || from != "open" {
		t.Fatalf("missing/incorrect open→in-progress hop: %+v", hops)
	}
	if from, ok := hops["done"]; !ok || from != "in-progress" {
		t.Fatalf("missing/incorrect in-progress→done hop: %+v", hops)
	}
}

func TestStatusTransition_NoRowWhenStatusUnchanged(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Title only", "")

	// Title change, no status change.
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Title: strPtr("Renamed")}); err != nil {
		t.Fatalf("update title: %v", err)
	}
	// Fields update that keeps the same status.
	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"status":"open"}`)}); err != nil {
		t.Fatalf("update fields: %v", err)
	}

	if trans := listTransitions(t, s, item.ID); len(trans) != 0 {
		t.Fatalf("expected no transitions, got %d", len(trans))
	}
}

func TestParseStatusChange(t *testing.T) {
	cases := []struct {
		in       string
		from, to string
		ok       bool
	}{
		{"status: open → done", "open", "done", true},
		{"priority: low → high; status: open → in-progress", "open", "in-progress", true},
		{"status: → done", "", "done", true}, // newly-set status
		{"priority: low → high", "", "", false},
		{"status: open", "", "", false}, // malformed, no arrow
		{"", "", "", false},
	}
	for _, c := range cases {
		from, to, ok := parseStatusChange(c.in)
		if ok != c.ok || from != c.from || to != c.to {
			t.Errorf("parseStatusChange(%q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, from, to, ok, c.from, c.to, c.ok)
		}
	}
}

func TestBackfillStatusTransitions(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	// Create item WITHOUT going through the status-changing update path, so
	// the only transition rows come from the backfill.
	item := createTestItem(t, s, wsID, colID, "Historical", "")

	// Simulate a historical activity row carrying a status change in its
	// metadata.changes blob (the shape diffFields emits).
	if _, err := s.CreateActivity(models.Activity{
		WorkspaceID: wsID,
		DocumentID:  item.ID,
		Action:      "updated",
		Actor:       "user",
		Source:      "web",
		Metadata:    `{"changes":"status: open → done"}`,
	}); err != nil {
		t.Fatalf("create activity: %v", err)
	}

	res, err := s.BackfillStatusTransitions()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Skipped {
		t.Fatalf("backfill unexpectedly skipped (table should have been empty)")
	}
	if res.Inserted != 1 {
		t.Fatalf("expected 1 inserted, got %d (scanned=%d errors=%d)", res.Inserted, res.ActivitiesScanned, res.Errors)
	}

	trans := listTransitions(t, s, item.ID)
	if len(trans) != 1 || trans[0].FromStatus != "open" || trans[0].ToStatus != "done" {
		t.Fatalf("backfilled transition wrong: %+v", trans)
	}

	// Second run must short-circuit (table now non-empty).
	res2, err := s.BackfillStatusTransitions()
	if err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if !res2.Skipped {
		t.Fatalf("expected second backfill to skip, got %+v", res2)
	}
}
