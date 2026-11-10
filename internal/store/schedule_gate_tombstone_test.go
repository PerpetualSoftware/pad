package store

import (
	"encoding/json"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// A soft-deleted blocker travels only so that its gate can still be judged. The
// row must not take anything from the live items beside it, and must not put
// back into the bundle the trashed body it has never carried.

// gateOnTombstone makes bundle item blocker a soft-deleted blocker of item blocked.
func gateOnTombstone(b *models.WorkspaceExport, blocker, blocked models.ItemExport) {
	b.ScheduleGates = append(b.ScheduleGates, models.ScheduleGateExport{
		ID: "gate-" + blocker.ID, BlockedItemID: blocked.ID, BlockerItemID: blocker.ID, CreatedAt: "2026-10-15T00:00:00Z",
	})
}

func TestTASK4423_R10ImportRefusesADeletedRowThatBlocksNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	bundle := bug3825Export("code")
	bundle.Version = models.WorkspaceExportVersion
	bundle.Items[0].DeletedAt = "2026-10-16T00:00:00Z"
	_, _, err := s.ImportWorkspaceWithReport(bundle, "Stray tombstone", "", "")
	if _, ok := AsValidationError(err); !ok {
		t.Fatalf("err = %v, want a validation refusal", err)
	}
	ws, err := s.GetWorkspaceBySlug("stray-tombstone")
	if err != nil {
		t.Fatal(err)
	}
	if ws != nil {
		t.Fatalf("a refused import left workspace %q", ws.Slug)
	}
}

func TestTASK4423_R10TombstoneHoldsNoUniqueValue(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"code", "invocation_slug"} {
		t.Run(key, func(t *testing.T) {
			s := testStore(t)
			bundle := bug3825Export(key)
			bundle.Version = models.WorkspaceExportVersion
			bundle.Items[0].DeletedAt = "2026-10-16T00:00:00Z"
			gateOnTombstone(bundle, bundle.Items[0], bundle.Items[1])
			ws, report, err := s.ImportWorkspaceWithReport(bundle, "Tombstone "+key, "", "")
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if len(report.DroppedUniqueValues) != 0 {
				t.Fatalf("dropped %+v, want the live item to keep its value", report.DroppedUniqueValues)
			}
			items, err := s.ListItems(ws.ID, models.ItemListParams{CollectionSlug: "keys"})
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Title != "Second" {
				t.Fatalf("live items = %+v, want only Second", items)
			}
			var fields map[string]any
			if err := json.Unmarshal([]byte(items[0].Fields), &fields); err != nil {
				t.Fatal(err)
			}
			if fields[key] != "k1" {
				t.Fatalf("Second %s = %v, want k1", key, fields[key])
			}
		})
	}
}

func TestTASK4423_R10TombstoneIsNotCountedByThePlanLimit(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const capItems = 3
	owner, bundle := importFixture(t, s, "tombstone", capItems, capItems)
	extra := bundle.Items[0]
	extra.ID = "33333333-3333-4333-8333-333333333333"
	extra.Slug = "tombstone-row"
	extra.ItemNumber = 99
	extra.DeletedAt = "2026-10-16T00:00:00Z"
	bundle.Items = append(bundle.Items, extra)
	gateOnTombstone(bundle, extra, bundle.Items[0])

	ws, err := s.ImportWorkspace(bundle, "tombstone-at-cap", owner, "api", WithPlanLimit())
	if err != nil {
		t.Fatalf("import = %v, want success: %d live items into a %d-item cap", err, capItems, capItems)
	}
	got, err := s.featureCountOn(s.db, ws.ID, owner, "items_per_workspace")
	if err != nil {
		t.Fatal(err)
	}
	if got != capItems {
		t.Fatalf("items_per_workspace = %d, want %d", got, capItems)
	}
}

func TestTASK4423_R10TombstoneCarriesNoBody(t *testing.T) {
	f := newLinkFix(t, 2)
	blocker, child := f.items[0], f.items[1]
	body := "trashed body"
	if _, err := f.s.UpdateItem(blocker.ID, models.ItemUpdate{Content: &body}); err != nil {
		t.Fatal(err)
	}
	live := "live body"
	if _, err := f.s.UpdateItem(child.ID, models.ItemUpdate{Content: &live}); err != nil {
		t.Fatal(err)
	}
	f.gate(t, child.ID, blocker.ID, f.allEdit())
	if err := f.s.DeleteItem(blocker.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, it := range f.exportBundle(t).Items {
		seen[it.ID] = it.Content
	}
	if got, ok := seen[blocker.ID]; !ok || got != "" {
		t.Fatalf("tombstone content = %q (present %v), want an empty body on a present row", got, ok)
	}
	if seen[child.ID] != live {
		t.Fatalf("live content = %q, want %q", seen[child.ID], live)
	}
}
