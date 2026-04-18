package store

import (
	"testing"

	"github.com/xarmian/pad/internal/models"
)

// TestGetItemProgress_DoneFieldFollowsBoardGroupBy verifies the core
// TASK-604 acceptance criterion: when a child collection's
// settings.board_group_by points at a non-status select field that has
// terminal_options declared, child-progress counts reflect THAT field's
// terminals — not status, and not the global default list.
func TestGetItemProgress_DoneFieldFollowsBoardGroupBy(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "DoneFieldTest")

	// Parent collection — generic plans holder. Nothing special here.
	plans, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Plans",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["draft","active","completed"],"default":"draft"}]}`,
	})
	if err != nil {
		t.Fatalf("create plans collection: %v", err)
	}

	// Child collection: a Bugs tracker whose board groups by `resolution`
	// with terminal options `fixed` and `wontfix`. `status` is still on
	// the schema with its own terminal options; we're specifically
	// asserting that those don't win over `resolution`.
	bugsSchema := `{"fields":[
		{"key":"status","label":"Status","type":"select","options":["new","investigating","fixed"],"terminal_options":["fixed"],"default":"new"},
		{"key":"resolution","label":"Resolution","type":"select","options":["open","fixed","wontfix"],"terminal_options":["fixed","wontfix"],"default":"open"}
	]}`
	bugsSettings := `{"board_group_by":"resolution"}`
	bugs, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Bugs",
		Schema:   bugsSchema,
		Settings: bugsSettings,
	})
	if err != nil {
		t.Fatalf("create bugs collection: %v", err)
	}

	// Create a parent plan.
	plan, err := s.CreateItem(ws.ID, plans.ID, models.ItemCreate{
		Title:  "Release Plan",
		Fields: `{"status":"active"}`,
	})
	if err != nil {
		t.Fatalf("create plan: %v", err)
	}

	// Create four bug items as children:
	//   b1: resolution=open          → not done
	//   b2: resolution=fixed         → done (terminal on resolution)
	//   b3: resolution=wontfix       → done (terminal on resolution)
	//   b4: resolution=open, status=fixed → NOT done
	//        (status is terminal, but status isn't the done field anymore)
	bugCases := []struct {
		title  string
		fields string
	}{
		{"Crash on login", `{"resolution":"open","status":"new"}`},
		{"Memory leak", `{"resolution":"fixed","status":"fixed"}`},
		{"Wrong copy", `{"resolution":"wontfix","status":"new"}`},
		{"Layout glitch", `{"resolution":"open","status":"fixed"}`},
	}
	var bugItems []*models.Item
	for _, bc := range bugCases {
		it, cerr := s.CreateItem(ws.ID, bugs.ID, models.ItemCreate{
			Title:  bc.title,
			Fields: bc.fields,
		})
		if cerr != nil {
			t.Fatalf("create bug %q: %v", bc.title, cerr)
		}
		bugItems = append(bugItems, it)
	}

	// Link each bug as a child of the plan.
	for _, b := range bugItems {
		if _, lerr := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
			TargetID: plan.ID,
			LinkType: "parent",
		}, b.ID); lerr != nil {
			t.Fatalf("link %s → plan: %v", b.Title, lerr)
		}
	}

	// Act: compute progress.
	total, done, err := s.GetItemProgress(plan.ID)
	if err != nil {
		t.Fatalf("GetItemProgress: %v", err)
	}

	// Expected:
	//   total = 4 (all children)
	//   done  = 2 (b2 + b3 — the ones with terminal `resolution` values)
	if total != 4 {
		t.Errorf("expected total=4, got %d", total)
	}
	if done != 2 {
		t.Errorf("expected done=2 (resolution-driven), got %d", done)
	}
}

// TestGetItemProgress_DefaultsToStatusWithoutSettings verifies back-compat:
// a collection without board_group_by (i.e. shipped today) should have its
// `status` field drive done-detection, exactly as before.
func TestGetItemProgress_DefaultsToStatusWithoutSettings(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "DefaultStatusTest")

	plans, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Plans",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["draft","active"],"default":"draft"}]}`,
	})
	if err != nil {
		t.Fatalf("create plans: %v", err)
	}

	tasks, err := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","in-progress","done","cancelled"],"terminal_options":["done","cancelled"],"default":"open"}]}`,
	})
	if err != nil {
		t.Fatalf("create tasks: %v", err)
	}

	plan, _ := s.CreateItem(ws.ID, plans.ID, models.ItemCreate{Title: "P", Fields: `{"status":"active"}`})

	taskCases := []string{
		`{"status":"open"}`,
		`{"status":"done"}`,
		`{"status":"cancelled"}`,
		`{"status":"in-progress"}`,
	}
	for _, f := range taskCases {
		it, _ := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "t", Fields: f})
		if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
			TargetID: plan.ID,
			LinkType: "parent",
		}, it.ID); err != nil {
			t.Fatalf("link: %v", err)
		}
	}

	total, done, err := s.GetItemProgress(plan.ID)
	if err != nil {
		t.Fatalf("GetItemProgress: %v", err)
	}
	if total != 4 {
		t.Errorf("expected total=4, got %d", total)
	}
	if done != 2 {
		t.Errorf("expected done=2 (status-driven), got %d", done)
	}
}

// TestGetItemProgress_HonorsSoftDeletedChildCollections verifies a
// regression Codex flagged: soft-deleted child collections must still
// contribute done filters, otherwise their items can never satisfy the
// per-collection `collection_id = ? AND ...` clause and would always be
// counted as active.
func TestGetItemProgress_HonorsSoftDeletedChildCollections(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "SoftDeleteTest")

	plans, _ := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Plans",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["active"],"default":"active"}]}`,
	})
	tasks, _ := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`,
	})

	plan, _ := s.CreateItem(ws.ID, plans.ID, models.ItemCreate{Title: "P", Fields: `{"status":"active"}`})
	done, _ := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "done task", Fields: `{"status":"done"}`})
	open, _ := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "open task", Fields: `{"status":"open"}`})

	for _, id := range []string{done.ID, open.ID} {
		if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{
			TargetID: plan.ID,
			LinkType: "parent",
		}, id); err != nil {
			t.Fatalf("link: %v", err)
		}
	}

	// Baseline: 1 done out of 2.
	_, doneBefore, err := s.GetItemProgress(plan.ID)
	if err != nil {
		t.Fatalf("GetItemProgress before soft-delete: %v", err)
	}
	if doneBefore != 1 {
		t.Fatalf("expected done=1 before soft-delete, got %d", doneBefore)
	}

	// Soft-delete the tasks collection. Its items remain in the DB and
	// are still linked to the plan.
	if err := s.DeleteCollection(tasks.ID); err != nil {
		t.Fatalf("soft delete tasks: %v", err)
	}

	// Expect the same done count — soft-deleted collections still
	// contribute their done rules.
	_, doneAfter, err := s.GetItemProgress(plan.ID)
	if err != nil {
		t.Fatalf("GetItemProgress after soft-delete: %v", err)
	}
	if doneAfter != 1 {
		t.Errorf(
			"expected done count to remain 1 after collection soft-delete, got %d — "+
				"soft-deleted collections must still contribute done filters",
			doneAfter,
		)
	}
}

// TestGetItemProgress_MixedChildCollections verifies that when children
// come from multiple collections with different done-field configurations,
// each child is evaluated against its own collection's rules.
func TestGetItemProgress_MixedChildCollections(t *testing.T) {
	s := testStore(t)
	ws := createTestWorkspace(t, s, "MixedChildTest")

	plans, _ := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Plans",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["active"],"default":"active"}]}`,
	})

	// Child A: status-driven, terminals ["done"].
	tasks, _ := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:   "Tasks",
		Schema: `{"fields":[{"key":"status","label":"Status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`,
	})

	// Child B: resolution-driven, terminals ["resolved"].
	bugs, _ := s.CreateCollection(ws.ID, models.CollectionCreate{
		Name:     "Bugs",
		Schema:   `{"fields":[{"key":"resolution","label":"Resolution","type":"select","options":["open","resolved"],"terminal_options":["resolved"],"default":"open"}]}`,
		Settings: `{"board_group_by":"resolution"}`,
	})

	plan, _ := s.CreateItem(ws.ID, plans.ID, models.ItemCreate{Title: "P", Fields: `{"status":"active"}`})

	// 1 task done + 1 task open + 1 bug resolved + 1 bug open  → expect done=2, total=4.
	link := func(id string) {
		if _, err := s.CreateItemLink(ws.ID, models.ItemLinkCreate{TargetID: plan.ID, LinkType: "parent"}, id); err != nil {
			t.Fatalf("link: %v", err)
		}
	}
	t1, _ := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "t1", Fields: `{"status":"done"}`})
	t2, _ := s.CreateItem(ws.ID, tasks.ID, models.ItemCreate{Title: "t2", Fields: `{"status":"open"}`})
	b1, _ := s.CreateItem(ws.ID, bugs.ID, models.ItemCreate{Title: "b1", Fields: `{"resolution":"resolved"}`})
	b2, _ := s.CreateItem(ws.ID, bugs.ID, models.ItemCreate{Title: "b2", Fields: `{"resolution":"open"}`})
	link(t1.ID)
	link(t2.ID)
	link(b1.ID)
	link(b2.ID)

	total, done, err := s.GetItemProgress(plan.ID)
	if err != nil {
		t.Fatalf("GetItemProgress: %v", err)
	}
	if total != 4 {
		t.Errorf("expected total=4, got %d", total)
	}
	if done != 2 {
		t.Errorf("expected done=2 (one per collection's rules), got %d", done)
	}
}
