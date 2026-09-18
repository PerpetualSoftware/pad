package store

import (
	"errors"
	"fmt"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3103, store half. ImportWorkspace with WithPlanLimit decides
// items_per_workspace inside its own transaction, and refuses a bundle that
// would land more items than the owner's plan allows.
//
// This door is NOT the race shape of #1393 / #1394, and these tests are built
// differently on purpose. ImportWorkspace mints the workspace inside the same
// transaction that inserts the items (BUG-2892), so the destination does not
// exist to any other writer until commit: there is no competitor to serialize
// against and nothing to race. What is being tested is arithmetic — would
// len(Items) fit under the cap — plus the refusal's blast radius.
//
// The blast radius is the assertion that matters most. A refusal is a new
// mid-import failure, and BUG-2892 exists because mid-import failures used to
// leave a workspace husk behind: named, slugged, owned, holding nothing. The
// husk was not merely clutter — uniqueWorkspaceSlug probes live rows, so the
// husk KEEPS THE SLUG, and the retry that succeeds lands on `name-2` with that
// slug in every URL afterwards. So every refusing leg below asserts that the
// workspace does not exist, not merely that the error came back.

// importFixture returns a free-plan owner whose items_per_workspace cap is
// `cap`, plus an export bundle carrying exactly `items` items.
//
// The bundle is produced by ExportWorkspace on a real workspace rather than
// hand-built, so the shape under test is the one the product actually
// imports. The SOURCE workspace is created with no MintOption, so building a
// bundle bigger than the cap is not itself refused.
func importFixture(t *testing.T, s *Store, tag string, capItems, items int) (ownerID string, bundle *models.WorkspaceExport) {
	t.Helper()

	owner, err := s.CreateUser(models.UserCreate{
		Email: "bug3103-" + tag + "@example.com", Name: "owner", Password: "pw-bug3103-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}

	src, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "bug3103-src-" + tag, OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace(src): %v", err)
	}
	if err := s.AddWorkspaceMember(src.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(src.ID, ""); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	coll, err := s.CreateCollection(src.ID, models.CollectionCreate{Name: "Tasks", Slug: "tasks", Prefix: "TASK"})
	if err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	for i := 0; i < items; i++ {
		// No MintOption: the SOURCE is deliberately unlimited, so the bundle
		// can exceed the cap the DESTINATION import is judged against.
		if _, err := s.CreateItem(src.ID, coll.ID, models.ItemCreate{Title: fmt.Sprintf("item %d", i)}); err != nil {
			t.Fatalf("CreateItem(%d): %v", i, err)
		}
	}

	bundle, err = s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}
	if got := len(bundle.Items); got != items {
		t.Fatalf("bundle carries %d items, want %d — the fixture is not testing what it claims", got, items)
	}

	// The cap is set AFTER the source is built, so building it never fought
	// the limit under test.
	if err := s.SetUserPlanOverrides(owner.ID, fmt.Sprintf(`{%q:%d}`, "items_per_workspace", capItems)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}
	return owner.ID, bundle
}

// workspaceExists answers whether a workspace with this name is present and
// not soft-deleted — the husk check. It counts rows directly rather than
// going through a lookup helper, so a helper that filters husks out could not
// make a husk invisible to this assertion.
func workspaceExists(t *testing.T, s *Store, name string) bool {
	t.Helper()
	var n int
	err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM workspaces WHERE name = ? AND deleted_at IS NULL`), name).Scan(&n)
	if err != nil {
		t.Fatalf("count workspaces: %v", err)
	}
	return n > 0
}

// TestImportWorkspace_OverItemCap_RefusesAndLeavesNothing is the first RED:
// the refusal happens AND the transaction leaves no husk behind.
func TestImportWorkspace_OverItemCap_RefusesAndLeavesNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const capItems = 3
	owner, bundle := importFixture(t, s, "over", capItems, capItems+1)

	const dstName = "bug3103-dst-over"
	ws, err := s.ImportWorkspace(bundle, dstName, owner, "api", WithPlanLimit())

	var ple *PlanLimitError
	if !errors.As(err, &ple) {
		t.Fatalf("ImportWorkspace err = %v (ws = %v), want *PlanLimitError: %d items into a %d-item cap", err, ws, len(bundle.Items), capItems)
	}
	if ple.Result.Feature != "items_per_workspace" {
		t.Errorf("Feature = %q, want items_per_workspace", ple.Result.Feature)
	}
	if ple.Result.Limit != capItems {
		t.Errorf("Limit = %d, want %d", ple.Result.Limit, capItems)
	}
	// Dave's day-71 ruling: the refusal names how many would land vs the
	// limit. Requested is that count; Current stays the destination's own
	// count, which is zero because the workspace never committed.
	if ple.Result.Requested != len(bundle.Items) {
		t.Errorf("Requested = %d, want %d (the bundle's item count)", ple.Result.Requested, len(bundle.Items))
	}
	if ple.Result.Current != 0 {
		t.Errorf("Current = %d, want 0 — Current is what the destination HOLDS, not what the bundle would add", ple.Result.Current)
	}

	if ws != nil {
		t.Errorf("ImportWorkspace returned a workspace (%s) alongside the refusal", ws.ID)
	}
	// The husk assertion, and the reason this test is first (BUG-2892).
	if workspaceExists(t, s, dstName) {
		t.Error("the refused import left a workspace row behind; it holds the slug, so the retry that succeeds lands on name-2")
	}
}

// TestImportWorkspace_AtItemCap_Succeeds is the control leg. Without it, a
// refusal that fired unconditionally would pass the test above.
func TestImportWorkspace_AtItemCap_Succeeds(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const capItems = 3
	owner, bundle := importFixture(t, s, "at", capItems, capItems)

	ws, err := s.ImportWorkspace(bundle, "bug3103-dst-at", owner, "api", WithPlanLimit())
	if err != nil {
		t.Fatalf("ImportWorkspace = %v, want success: %d items into a %d-item cap", err, len(bundle.Items), capItems)
	}
	if ws == nil {
		t.Fatal("ImportWorkspace returned no workspace and no error")
	}
	got, err := s.featureCountOn(s.db, ws.ID, owner, "items_per_workspace")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != capItems {
		t.Errorf("imported workspace holds %d items, want %d", got, capItems)
	}
}

// TestImportWorkspace_OverItemCap_UnlimitedWithoutMintOption pins the exemption
// rather than assuming it: the CLI database import (cmd/pad/cmd_db.go) and
// migrations pass no MintOption and must stay unlimited. Asserted because an
// enforcement added in the wrong place — outside the option, or in a shared
// helper every caller reaches — would break a self-hosted migration, and the
// CLI path has no test of its own that would notice.
func TestImportWorkspace_OverItemCap_UnlimitedWithoutMintOption(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	const capItems = 3
	owner, bundle := importFixture(t, s, "unlimited", capItems, capItems+1)

	ws, err := s.ImportWorkspace(bundle, "bug3103-dst-unlimited", owner, "cli")
	if err != nil {
		t.Fatalf("ImportWorkspace without WithPlanLimit = %v, want success (the CLI/migration path is exempt)", err)
	}
	if ws == nil {
		t.Fatal("ImportWorkspace returned no workspace and no error")
	}
	got, err := s.featureCountOn(s.db, ws.ID, owner, "items_per_workspace")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != capItems+1 {
		t.Errorf("imported workspace holds %d items, want %d — the unlimited path must land the whole bundle", got, capItems+1)
	}
}

// TestImportWorkspace_OrphanedItemsDoNotCountTowardTheCap is the leg that
// discriminates the check's PLACEMENT, and without it the simpler wrong
// implementation passes everything else.
//
// The loop skips an item whose collection is missing from the bundle
// (`newCollID == ""` → `continue`), so len(data.Items) is NOT how many items
// land. Counting the bundle's length up front would refuse this import even
// though the number that actually lands is exactly at the cap — and Dave's
// ruling asks the refusal to name how many WOULD LAND, which is a different
// number from how many were offered.
//
// Construction: export a workspace whose items live in two collections, then
// drop one collection from the bundle. Its items become orphans on the way in.
func TestImportWorkspace_OrphanedItemsDoNotCountTowardTheCap(t *testing.T) {
	t.Parallel()
	s := testStore(t)

	owner, err := s.CreateUser(models.UserCreate{
		Email: "bug3103-orphan@example.com", Name: "owner", Password: "pw-bug3103-12345",
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := s.SetUserPlan(owner.ID, "free", ""); err != nil {
		t.Fatalf("SetUserPlan: %v", err)
	}
	src, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "bug3103-src-orphan", OwnerID: owner.ID})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if err := s.AddWorkspaceMember(src.ID, owner.ID, "owner"); err != nil {
		t.Fatalf("AddWorkspaceMember: %v", err)
	}
	if err := s.SeedCollectionsFromTemplate(src.ID, ""); err != nil {
		t.Fatalf("SeedCollectionsFromTemplate: %v", err)
	}
	keep, err := s.CreateCollection(src.ID, models.CollectionCreate{Name: "Keep", Slug: "keep", Prefix: "KEEP"})
	if err != nil {
		t.Fatalf("CreateCollection(keep): %v", err)
	}
	drop, err := s.CreateCollection(src.ID, models.CollectionCreate{Name: "Drop", Slug: "drop", Prefix: "DROP"})
	if err != nil {
		t.Fatalf("CreateCollection(drop): %v", err)
	}

	const capItems = 2
	const orphans = 3
	for i := 0; i < capItems; i++ {
		if _, err := s.CreateItem(src.ID, keep.ID, models.ItemCreate{Title: fmt.Sprintf("keep %d", i)}); err != nil {
			t.Fatalf("CreateItem(keep %d): %v", i, err)
		}
	}
	for i := 0; i < orphans; i++ {
		if _, err := s.CreateItem(src.ID, drop.ID, models.ItemCreate{Title: fmt.Sprintf("drop %d", i)}); err != nil {
			t.Fatalf("CreateItem(drop %d): %v", i, err)
		}
	}

	bundle, err := s.ExportWorkspace(src.Slug)
	if err != nil {
		t.Fatalf("ExportWorkspace: %v", err)
	}

	// Drop the collection, keep its items: they arrive as orphans.
	kept := bundle.Collections[:0]
	for _, c := range bundle.Collections {
		if c.ID != drop.ID {
			kept = append(kept, c)
		}
	}
	bundle.Collections = kept
	if len(bundle.Items) != capItems+orphans {
		t.Fatalf("bundle carries %d items, want %d", len(bundle.Items), capItems+orphans)
	}

	if err := s.SetUserPlanOverrides(owner.ID, fmt.Sprintf(`{%q:%d}`, "items_per_workspace", capItems)); err != nil {
		t.Fatalf("SetUserPlanOverrides: %v", err)
	}

	ws, err := s.ImportWorkspace(bundle, "bug3103-dst-orphan", owner.ID, "api", WithPlanLimit())
	if err != nil {
		t.Fatalf("ImportWorkspace = %v, want success: %d items offered but only %d land, cap %d — the check must count what LANDS, not len(data.Items)",
			err, len(bundle.Items), capItems, capItems)
	}
	if ws == nil {
		t.Fatal("ImportWorkspace returned no workspace and no error")
	}
	got, err := s.featureCountOn(s.db, ws.ID, owner.ID, "items_per_workspace")
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != capItems {
		t.Errorf("imported workspace holds %d items, want %d — the premise of this test is that the orphans did not land", got, capItems)
	}
}
