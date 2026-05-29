package store

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// listTransitions returns all status_transitions rows for an item, oldest
// first.
func listTransitions(t *testing.T, s *Store, itemID string) []models.StatusTransition {
	t.Helper()
	rows, err := s.db.Query(s.q(`
		SELECT id, item_id, workspace_id, collection_id, field_key, from_status, to_status, created_at
		FROM status_transitions WHERE item_id = ? ORDER BY created_at, id
	`), itemID)
	if err != nil {
		t.Fatalf("query transitions: %v", err)
	}
	defer rows.Close()
	var out []models.StatusTransition
	for rows.Next() {
		var st models.StatusTransition
		if err := rows.Scan(&st.ID, &st.ItemID, &st.WorkspaceID, &st.CollectionID, &st.FieldKey, &st.FromStatus, &st.ToStatus, &st.CreatedAt); err != nil {
			t.Fatalf("scan transition: %v", err)
		}
		out = append(out, st)
	}
	return out
}

// hopTransitions returns only the "real" status hops — excluding the
// create-time "entered initial status" seed row (id prefix "create_") that
// every created item gets. Hop-focused tests use this so the baseline seed
// doesn't skew their counts.
func hopTransitions(t *testing.T, s *Store, itemID string) []models.StatusTransition {
	t.Helper()
	var out []models.StatusTransition
	for _, st := range listTransitions(t, s, itemID) {
		if !strings.HasPrefix(st.ID, "create_") {
			out = append(out, st)
		}
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

	hops := hopTransitions(t, s, item.ID)
	if len(hops) != 1 {
		t.Fatalf("expected 1 hop transition, got %d", len(hops))
	}
	got := hops[0]
	if got.FromStatus != "open" || got.ToStatus != "done" {
		t.Fatalf("expected open → done, got %q → %q", got.FromStatus, got.ToStatus)
	}
	if got.FieldKey != "status" {
		t.Fatalf("expected field_key=status, got %q", got.FieldKey)
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

	hops := hopTransitions(t, s, item.ID)
	if len(hops) != 2 {
		t.Fatalf("expected 2 hop transitions, got %d", len(hops))
	}
	// Both updates can land in the same wall-clock second (now() is
	// second-resolution), so row order isn't guaranteed — assert the SET of
	// hops instead. Each hop must still record the correct from→to pair.
	byTo := map[string]string{} // to -> from
	for _, st := range hops {
		byTo[st.ToStatus] = st.FromStatus
	}
	if from, ok := byTo["in-progress"]; !ok || from != "open" {
		t.Fatalf("missing/incorrect open→in-progress hop: %+v", byTo)
	}
	if from, ok := byTo["done"]; !ok || from != "in-progress" {
		t.Fatalf("missing/incorrect in-progress→done hop: %+v", byTo)
	}
}

func TestStatusTransition_NoHopWhenStatusUnchanged(t *testing.T) {
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

	if hops := hopTransitions(t, s, item.ID); len(hops) != 0 {
		t.Fatalf("expected no hop transitions, got %d", len(hops))
	}
}

// Finding 2: every created item gets a create-time "entered initial status"
// seed row so it has an "entered status" timestamp.
func TestStatusTransition_CreateSeed(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Fresh", "") // status defaults to open

	all := listTransitions(t, s, item.ID)
	if len(all) != 1 {
		t.Fatalf("expected 1 create-seed row, got %d: %+v", len(all), all)
	}
	seed := all[0]
	if !strings.HasPrefix(seed.ID, "create_") {
		t.Fatalf("expected create_ id, got %q", seed.ID)
	}
	if seed.FromStatus != "" || seed.ToStatus != "open" || seed.FieldKey != "status" {
		t.Fatalf("unexpected create-seed: %+v", seed)
	}
}

// Finding 2: an item created directly in a terminal value must still get a
// create-seed row so reports can count it as a completion.
func TestStatusTransition_CreateInTerminalStatus(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item, err := s.CreateItem(wsID, colID, models.ItemCreate{Title: "Born done", Fields: `{"status":"done"}`})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}

	all := listTransitions(t, s, item.ID)
	if len(all) != 1 || all[0].ToStatus != "done" || all[0].FromStatus != "" {
		t.Fatalf("expected one '' → done create-seed, got %+v", all)
	}
}

// Finding 1: collections that designate a non-status select field as their
// done field (BoardGroupBy) must record transitions on THAT field.
func TestStatusTransition_NonStatusDoneField(t *testing.T) {
	s := testStore(t)
	u, err := s.CreateUser(models.UserCreate{Name: "Hank", Email: "hank@example.com"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "Hiring", Slug: "hiring", OwnerID: u.ID})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	col, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Candidates",
		Schema:   `{"fields":[{"key":"stage","label":"Stage","type":"select","options":["applied","interview","hired"],"terminal_options":["hired"],"default":"applied","required":true}]}`,
		Settings: `{"board_group_by":"stage"}`,
	})
	if err != nil {
		t.Fatalf("create collection: %v", err)
	}

	item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Ada", Fields: `{"stage":"applied"}`})
	if err != nil {
		t.Fatalf("create item: %v", err)
	}
	// create-seed should be on the "stage" field.
	seeds := listTransitions(t, s, item.ID)
	if len(seeds) != 1 || seeds[0].FieldKey != "stage" || seeds[0].ToStatus != "applied" {
		t.Fatalf("expected stage create-seed '' → applied, got %+v", seeds)
	}

	if _, err := s.UpdateItem(item.ID, models.ItemUpdate{Fields: strPtr(`{"stage":"hired"}`)}); err != nil {
		t.Fatalf("update stage: %v", err)
	}
	hops := hopTransitions(t, s, item.ID)
	if len(hops) != 1 {
		t.Fatalf("expected 1 stage hop, got %d", len(hops))
	}
	if hops[0].FieldKey != "stage" || hops[0].FromStatus != "applied" || hops[0].ToStatus != "hired" {
		t.Fatalf("expected stage applied → hired, got %+v", hops[0])
	}
}

func TestStatusTransition_CapturedOnMoveWithStatusOverride(t *testing.T) {
	s := testStore(t)
	wsID, srcColID := newTransitionTestWorkspace(t, s)
	dstCol := createTestCollection(t, s, wsID, "Done Bucket")
	item := createTestItem(t, s, wsID, srcColID, "Movable", "")

	// `pad item move ... --field status=done` rewrites fields through the
	// move path, which must also record the transition.
	if _, err := s.MoveItem(item.ID, dstCol.ID, `{"status":"done"}`); err != nil {
		t.Fatalf("move item: %v", err)
	}

	hops := hopTransitions(t, s, item.ID)
	if len(hops) != 1 {
		t.Fatalf("expected 1 hop transition from move, got %d", len(hops))
	}
	got := hops[0]
	if got.FromStatus != "open" || got.ToStatus != "done" {
		t.Fatalf("expected open → done, got %q → %q", got.FromStatus, got.ToStatus)
	}
	// collection_id should reflect the target collection the item moved into.
	if got.CollectionID != dstCol.ID {
		t.Fatalf("expected transition stamped with target collection %q, got %q", dstCol.ID, got.CollectionID)
	}
}

func TestStatusTransition_NoHopOnMoveWithoutStatusChange(t *testing.T) {
	s := testStore(t)
	wsID, srcColID := newTransitionTestWorkspace(t, s)
	dstCol := createTestCollection(t, s, wsID, "Other Bucket")
	item := createTestItem(t, s, wsID, srcColID, "Plain move", "")

	// Move preserving status — no hop transition expected.
	if _, err := s.MoveItem(item.ID, dstCol.ID, `{"status":"open"}`); err != nil {
		t.Fatalf("move item: %v", err)
	}
	if hops := hopTransitions(t, s, item.ID); len(hops) != 0 {
		t.Fatalf("expected no hop transitions on status-preserving move, got %d", len(hops))
	}
}

// A hard delete of an item must cascade to its status_transitions rows
// (FK ON DELETE CASCADE), or the delete would fail the FK constraint.
func TestStatusTransition_CascadesOnHardDelete(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
	item := createTestItem(t, s, wsID, colID, "Doomed", "") // seeds a create-row

	if rows := listTransitions(t, s, item.ID); len(rows) == 0 {
		t.Fatalf("expected at least the create-seed row before delete")
	}
	if _, err := s.db.Exec(s.dialect.Rebind(`DELETE FROM items WHERE id = ?`), item.ID); err != nil {
		t.Fatalf("hard delete item: %v", err)
	}
	if rows := listTransitions(t, s, item.ID); len(rows) != 0 {
		t.Fatalf("expected transitions to cascade-delete, got %d", len(rows))
	}
}

func TestParseFieldChange(t *testing.T) {
	cases := []struct {
		in       string
		key      string
		from, to string
		ok       bool
	}{
		{"status: open → done", "status", "open", "done", true},
		{"priority: low → high; status: open → in-progress", "status", "open", "in-progress", true},
		{"status: → done", "status", "", "done", true}, // newly-set value
		{"priority: low → high", "status", "", "", false},
		{"status: open", "status", "", "", false}, // malformed, no arrow
		{"", "status", "", "", false},
		{"stage: applied → hired", "stage", "applied", "hired", true}, // non-status field
		{"stage: applied → hired", "status", "", "", false},           // wrong key
	}
	for _, c := range cases {
		from, to, ok := parseFieldChange(c.in, c.key)
		if ok != c.ok || from != c.from || to != c.to {
			t.Errorf("parseFieldChange(%q, %q) = (%q, %q, %v); want (%q, %q, %v)",
				c.in, c.key, from, to, ok, c.from, c.to, c.ok)
		}
	}
}

func TestBackfillStatusTransitions(t *testing.T) {
	s := testStore(t)
	wsID, colID := newTransitionTestWorkspace(t, s)
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

	// createTestItem already seeded a create-row, so clear the table to
	// simulate the fresh, empty post-migration state the backfill expects.
	if _, err := s.db.Exec(s.q(`DELETE FROM status_transitions`)); err != nil {
		t.Fatalf("clear transitions: %v", err)
	}

	res, err := s.BackfillStatusTransitions()
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if res.Skipped {
		t.Fatalf("backfill unexpectedly skipped (table should have been empty)")
	}
	// Pass 1 inserts the open→done hop; Pass 2 seeds the create-time '' → open
	// row (initial value reconstructed from the hop's `from`).
	if res.Inserted != 2 {
		t.Fatalf("expected 2 inserted, got %d (scanned=%d errors=%d)", res.Inserted, res.ActivitiesScanned, res.Errors)
	}

	all := listTransitions(t, s, item.ID)
	var hop, seed *models.StatusTransition
	for i := range all {
		switch {
		case strings.HasPrefix(all[i].ID, "bf_"):
			hop = &all[i]
		case strings.HasPrefix(all[i].ID, "create_"):
			seed = &all[i]
		}
	}
	if hop == nil || hop.FromStatus != "open" || hop.ToStatus != "done" {
		t.Fatalf("backfilled hop wrong: %+v", hop)
	}
	if seed == nil || seed.FromStatus != "" || seed.ToStatus != "open" {
		t.Fatalf("backfilled create-seed wrong: %+v", seed)
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
