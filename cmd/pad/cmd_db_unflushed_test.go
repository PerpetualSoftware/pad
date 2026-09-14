package main

import (
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// BUG-3032: `pad db migrate-to-pg` is the only door that abandons its source
// database, so it is the only door that refuses rather than warns. These tests
// pin the three things an operator's outcome depends on — that a clean
// migration is not gated at all, that a gated one is refused with the items
// NAMED and nothing migrated, and that the escape hatch still puts the loss on
// the terminal record.

func gateFixture() ([]models.Workspace, map[string][]store.PendingFlushItem) {
	return []models.Workspace{
			{Name: "Alpha", Slug: "alpha"},
			{Name: "Beta", Slug: "beta"},
		}, map[string][]store.PendingFlushItem{
			"beta": {
				{Ref: "TASK-7", Title: "half-typed body"},
				{Ref: "TASK-9", Title: "another one"},
			},
		}
}

func TestGateUnflushedEditsPassesACleanMigrationSilently(t *testing.T) {
	ws, _ := gateFixture()
	// The CONTROL, and it runs first: a gate that fired on zero pending items
	// would refuse every migration, and the two assertions below would pass for
	// that gate as readily as for a correct one.
	report, err := gateUnflushedEdits(ws, map[string][]store.PendingFlushItem{}, 0, false)
	if err != nil {
		t.Errorf("a workspace set with nothing pending was refused: %v", err)
	}
	if report != "" {
		t.Errorf("a clean migration printed %q; an operator with nothing pending must see nothing", report)
	}
}

func TestGateUnflushedEditsRefusesAndNamesEveryAffectedItem(t *testing.T) {
	ws, pending := gateFixture()
	report, err := gateUnflushedEdits(ws, pending, 2, false)
	if err == nil {
		t.Fatal("the gate permitted a migration that would have destroyed two items' edits")
	}
	if report != "" {
		t.Errorf("a refusal returned a printable report %q as well as an error; the detail belongs "+
			"INSIDE the error so a caller cannot print the refusal without its reason", report)
	}
	msg := err.Error()
	// Named items, not a count. A count tells an operator a migration is blocked;
	// the refs tell them which tabs to open, which is the only remedy there is.
	for _, want := range []string{"TASK-7", "half-typed body", "TASK-9", "another one"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal does not name %q:\n%s", want, msg)
		}
	}
	// The workspace that has nothing pending must not appear — an operator
	// reading "Alpha" here would go looking for items that are fine.
	if strings.Contains(msg, "alpha") {
		t.Errorf("refusal names a workspace with nothing pending:\n%s", msg)
	}
	if !strings.Contains(msg, "Beta (beta)") {
		t.Errorf("refusal does not attribute the items to their workspace:\n%s", msg)
	}
	// The NUL gate's promise, verbatim, because this check runs across EVERY
	// workspace before the first import precisely so it can be made.
	if !strings.HasSuffix(msg, "nothing has been migrated") {
		t.Errorf("refusal does not end by promising nothing was migrated:\n%s", msg)
	}
	// The remedy and the escape hatch both have to be IN the refusal: a refusal
	// with no way forward is a trap, and per BUG-3000 the flush may never happen
	// on its own.
	if !strings.Contains(msg, "web UI") {
		t.Errorf("refusal does not name the only remedy (open the item so a tab flushes):\n%s", msg)
	}
	if !strings.Contains(msg, "--discard-unflushed-edits") {
		t.Errorf("refusal does not name the escape hatch:\n%s", msg)
	}
}

func TestGateUnflushedEditsUnderTheFlagProceedsButRecordsTheLoss(t *testing.T) {
	ws, pending := gateFixture()
	report, err := gateUnflushedEdits(ws, pending, 2, true)
	if err != nil {
		t.Fatalf("--discard-unflushed-edits did not permit the migration: %v", err)
	}
	// The flag's NAME is the acknowledgement, so there is no prompt — which is
	// exactly why the same list must reach the terminal anyway. A silent flag
	// would leave no record of what was destroyed.
	if !strings.Contains(report, "WARNING") {
		t.Errorf("proceeding under the flag printed no warning banner:\n%s", report)
	}
	for _, want := range []string{"LOST", "TASK-7", "TASK-9", "Beta (beta)"} {
		if !strings.Contains(report, want) {
			t.Errorf("the discard warning does not contain %q:\n%s", want, report)
		}
	}
}

// staleBundleItems is the codex-round-1 P1 fix: the pre-pass reads the database
// at one instant and ExportWorkspace reads it at another, so the bundle about to
// be written is the only artifact whose staleness can be checked without a
// window. These pin the two things the refusal depends on — that it fires on
// exactly the marked items, and that every one it names is openable.
func TestStaleBundleItemsNamesOnlyMarkedItems(t *testing.T) {
	bundle := &models.WorkspaceExport{
		Collections: []models.CollectionExport{{ID: "c1", Prefix: "TASK"}},
		Items: []models.ItemExport{
			{ID: "i1", CollectionID: "c1", ItemNumber: 7, Title: "stale one", Slug: "stale-one",
				ContentState: models.ContentOutcomeAppliedPendingFlush},
			{ID: "i2", CollectionID: "c1", ItemNumber: 8, Title: "current", Slug: "current"},
			{ID: "i3", CollectionID: "c1", ItemNumber: 9, Title: "stale two", Slug: "stale-two",
				ContentState: models.ContentOutcomeAppliedPendingFlush},
		},
	}

	got := staleBundleItems(bundle)
	if len(got) != 2 {
		t.Fatalf("named %d item(s) %+v, want the 2 marked ones — a check that returns everything "+
			"refuses every migration, and one that returns nothing refuses none", len(got), got)
	}
	if got[0].Ref != "TASK-7" || got[1].Ref != "TASK-9" {
		t.Errorf("refs = %q, %q; want TASK-7, TASK-9", got[0].Ref, got[1].Ref)
	}
	if got[0].Title != "stale one" {
		t.Errorf("title = %q, want %q", got[0].Title, "stale one")
	}

	// CONTROL: a bundle with nothing marked must not fire. Without this the
	// assertion above passes for a function that ignores ContentState entirely
	// and simply returns the first two items.
	clean := &models.WorkspaceExport{
		Collections: []models.CollectionExport{{ID: "c1", Prefix: "TASK"}},
		Items: []models.ItemExport{
			{ID: "i1", CollectionID: "c1", ItemNumber: 7, Title: "a", Slug: "a"},
			{ID: "i2", CollectionID: "c1", ItemNumber: 8, Title: "b", Slug: "b"},
		},
	}
	if got := staleBundleItems(clean); len(got) != 0 {
		t.Errorf("a bundle with nothing marked named %+v", got)
	}
	if got := staleBundleItems(nil); got != nil {
		t.Errorf("a nil bundle named %+v", got)
	}
}

func TestStaleBundleItemsFallsBackToTheSlugWhenNoRefCanBeBuilt(t *testing.T) {
	// An item whose collection is not in the bundle, and one with no item
	// number. A refusal exists to tell an operator what to OPEN, so a
	// fabricated "PREFIX-0" — or an empty ref — sends them looking for
	// something that does not exist.
	bundle := &models.WorkspaceExport{
		Collections: []models.CollectionExport{{ID: "c1", Prefix: "TASK"}},
		Items: []models.ItemExport{
			{ID: "i1", CollectionID: "missing", ItemNumber: 3, Title: "orphan", Slug: "orphan-slug",
				ContentState: models.ContentOutcomeAppliedPendingFlush},
			{ID: "i2", CollectionID: "c1", ItemNumber: 0, Title: "no number", Slug: "no-number-slug",
				ContentState: models.ContentOutcomeAppliedPendingFlush},
		},
	}
	got := staleBundleItems(bundle)
	if len(got) != 2 {
		t.Fatalf("named %d, want 2: %+v", len(got), got)
	}
	for _, it := range got {
		if it.Ref == "" || strings.Contains(it.Ref, "-0") {
			t.Errorf("unopenable ref %q for %q — want the slug when no real ref exists", it.Ref, it.Title)
		}
	}
	if got[0].Ref != "orphan-slug" || got[1].Ref != "no-number-slug" {
		t.Errorf("refs = %q, %q; want the slugs", got[0].Ref, got[1].Ref)
	}
}
