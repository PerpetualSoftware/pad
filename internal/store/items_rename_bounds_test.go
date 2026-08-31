package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2804 M3 guards: the bound on how much linking-item content ONE item
// rename may process.
//
// These are regression tests, not probes — each was run against the code
// WITHOUT the cap and fails there. The mutation matrix is on the trail.

// bigLinker builds a body of approximately bodyBytes carrying exactly one
// [[title]] link, so the cascade sees one row per source and the cost is
// dominated by the body rather than by link count.
func bigLinker(t *testing.T, title string, bodyBytes int) string {
	t.Helper()
	return probeBody(t, title, 1, bodyBytes)
}

// TestItemRenameCascade_RefusesWhenTheCascadeExceedsTheBound is the core guard.
//
// Each linker is charged its body as read PLUS the body the rewrite will
// build, so a 2 MiB linker costs ~4 MiB against a 64 MiB cap and ~17 of them
// cross it. Sizes are derived from MaxItemRenameCascadeBytes rather than
// hardcoded, so the test follows the constant if it is ever retuned.
func TestItemRenameCascade_RefusesWhenTheCascadeExceedsTheBound(t *testing.T) {
	const body = 2 << 20 // the largest body one JSON request can carry
	perLinker := int64(body) * 2
	linkers := int(MaxItemRenameCascadeBytes/perLinker) + 2

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ItemRenameBound")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	target := createTestItem(t, s, ws.ID, col.ID, "Old", "the item being renamed")
	linkerBody := bigLinker(t, "Old", body)
	for i := 0; i < linkers; i++ {
		createTestItem(t, s, ws.ID, col.ID, "Linker "+strings.Repeat("x", i%3)+itoa(i), linkerBody)
	}

	newTitle := "New"
	_, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle})
	if !errors.Is(err, ErrItemRenameCascadeTooLarge) {
		t.Fatalf("rename over the bound: got %v, want ErrItemRenameCascadeTooLarge", err)
	}

	var typed *ItemRenameCascadeTooLargeError
	if !errors.As(err, &typed) {
		t.Fatalf("error does not carry the typed figures: %T", err)
	}
	if typed.NewTitle != newTitle {
		t.Errorf("NewTitle = %q, want %q — the caller needs to see which rename was refused", typed.NewTitle, newTitle)
	}
	if typed.Max != MaxItemRenameCascadeBytes {
		t.Errorf("Max = %d, want %d", typed.Max, MaxItemRenameCascadeBytes)
	}
	if typed.Processed <= MaxItemRenameCascadeBytes {
		t.Errorf("Processed = %d, must exceed the cap %d at the moment of refusal", typed.Processed, MaxItemRenameCascadeBytes)
	}

	// The refusal must roll the whole rename back — the title is unchanged and
	// no linker was rewritten. Without this leg the test would pass on an
	// implementation that refused AFTER mutating half the workspace.
	got, err := s.GetItem(target.ID)
	if err != nil {
		t.Fatalf("re-read target: %v", err)
	}
	if got.Title != "Old" {
		t.Errorf("title = %q after a refused rename, want %q — the refusal did not roll back", got.Title, "Old")
	}
	var rewritten int
	if err := s.db.QueryRow(s.q(
		`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND content LIKE '%[[New]]%'`),
		ws.ID).Scan(&rewritten); err != nil {
		t.Fatalf("count rewritten: %v", err)
	}
	if rewritten != 0 {
		t.Errorf("%d linkers were rewritten despite the refusal", rewritten)
	}
}

// TestItemRenameCascade_AllowsARealisticCascade is the counterfactual: a
// cascade the size real workspaces actually produce must NOT be refused.
//
// Sized from the live-workspace measurement recorded on
// MaxItemRenameCascadeBytes: the worst ACTUAL cascade there was 173,378 bytes
// across 23 sources. This uses 30 sources at the measured p99.9 body size
// (51,000 bytes), which is ~9x that worst case and still ~21x under the cap.
func TestItemRenameCascade_AllowsARealisticCascade(t *testing.T) {
	const p999Body = 51000
	const sources = 30

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ItemRenameOrdinary")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	target := createTestItem(t, s, ws.ID, col.ID, "Old", "the item being renamed")
	linkerBody := bigLinker(t, "Old", p999Body)
	for i := 0; i < sources; i++ {
		createTestItem(t, s, ws.ID, col.ID, "Linker "+itoa(i), linkerBody)
	}

	newTitle := "New"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("a realistic cascade was refused: %v", err)
	}

	var rewritten int
	if err := s.db.QueryRow(s.q(
		`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND content LIKE '%[[New]]%'`),
		ws.ID).Scan(&rewritten); err != nil {
		t.Fatalf("count rewritten: %v", err)
	}
	if rewritten != sources {
		t.Errorf("%d of %d linkers rewritten — the cascade did not complete", rewritten, sources)
	}
}

// TestItemRenameCascade_RefusesBeforeBuildingTheRewrittenBody closes the hole
// every other oversize test leaves open: moving the projection below the
// rewrite would keep them all green while destroying the point of the guard,
// which is that the amplified string is never built.
//
// THE INSTRUMENT IS A COUNT, NOT AN ALLOCATION CEILING, and that is a
// correction rather than a preference. The first version of this test used a
// TotalAlloc ceiling, copied in spirit from BUG-2798's equivalent. Two things
// went wrong with it, in this order:
//
//  1. With 16 MiB of slack the check-after-build mutant SURVIVED. BUG-2798
//     could afford a generous ceiling because its rewrite was amplified 51.8x;
//     M2 removed the amplification, so the defect is worth exactly one body.
//  2. Tightened to cap+3 MiB it killed the mutant on SQLite — and then FAILED
//     on Postgres, where the same refusal allocates ~242 MB against SQLite's
//     ~69 MB because the driver copies row bytes differently. The ceiling was
//     measuring the database driver, not the guard.
//
// Counting bodies BUILT is exact, identical on both dialects, and discriminates
// by one whole unit: refusing before the build yields N, refusing after yields
// N+1. Found by make test-pg, which is the whole reason that gate exists.
func TestItemRenameCascade_RefusesBeforeBuildingTheRewrittenBody(t *testing.T) {
	const body = 2 << 20
	perLinker := int64(body) * 2
	// The cascade admits floor(cap/perLinker) sources; the NEXT one crosses.
	admits := int(MaxItemRenameCascadeBytes / perLinker)
	linkers := admits + 2

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ItemRenameNoBuild")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	target := createTestItem(t, s, ws.ID, col.ID, "Old", "the item being renamed")
	linkerBody := bigLinker(t, "Old", body)
	for i := 0; i < linkers; i++ {
		createTestItem(t, s, ws.ID, col.ID, "Linker "+itoa(i), linkerBody)
	}

	var built int
	s.onItemCascadeBodyBuilt = func(int) { built++ }
	defer func() { s.onItemCascadeBodyBuilt = nil }()

	newTitle := "New"
	_, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle})
	if !errors.Is(err, ErrItemRenameCascadeTooLarge) {
		t.Fatalf("rename: got %v, want ErrItemRenameCascadeTooLarge", err)
	}

	// The source that CROSSES the cap must not have had its body built. Every
	// source admitted before it legitimately did.
	if built != admits {
		t.Errorf("cascade built %d rewritten bodies before refusing, want %d — the projection is "+
			"running AFTER the body it was supposed to prevent (one extra build is exactly the defect)",
			built, admits)
	}
	t.Logf("built %d rewritten bodies before refusing; cap admits %d sources of %d bytes each",
		built, admits, body)
}

// TestItemRenameCascade_DoesNotChargeForRewritesItWillNotPerform pins the
// asymmetry that keeps the bound honest in the direction that matters.
//
// A source whose recorded offsets no longer point at a matching bracket is
// charged for the body it had to READ — unavoidable, it is already in memory —
// but NOT for a rewritten copy that will never be built. Charging for both
// would let content the rewriter can never touch push a legitimate rename over
// the cap, which is the class BUG-2798's codex round 7 caught on the document
// path in a different costume.
//
// Drift is simulated the only way it occurs in production: the content changes
// without the index being re-parsed. The test writes items.content directly so
// replaceWikiLinks does not run.
func TestItemRenameCascade_DoesNotChargeForRewritesItWillNotPerform(t *testing.T) {
	const body = 2 << 20
	live := 12
	drifted := 6

	// The fixture is sized so the two charging rules give OPPOSITE answers, and
	// it asserts that rather than assuming it. Without this the test could
	// silently stop discriminating if the cap or the body size is ever retuned
	// — it would still pass, while proving nothing.
	honest := int64(live)*int64(body)*2 + int64(drifted)*int64(body)
	overcharged := int64(live+drifted) * int64(body) * 2
	if honest > MaxItemRenameCascadeBytes {
		t.Fatalf("fixture: honest charging is %d, over the %d cap — this test would pass for the wrong reason",
			honest, int64(MaxItemRenameCascadeBytes))
	}
	if overcharged <= MaxItemRenameCascadeBytes {
		t.Fatalf("fixture: charging the drifted sources for rewrites too is %d, still under the %d cap — "+
			"this test cannot detect the defect it exists for", overcharged, int64(MaxItemRenameCascadeBytes))
	}

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ItemRenameDrift")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	target := createTestItem(t, s, ws.ID, col.ID, "Old", "the item being renamed")
	linkerBody := bigLinker(t, "Old", body)

	for i := 0; i < live; i++ {
		createTestItem(t, s, ws.ID, col.ID, "Live "+itoa(i), linkerBody)
	}
	// The drifted ones: created with a real link so index rows exist, then
	// their content is overwritten behind the index's back.
	for i := 0; i < drifted; i++ {
		it := createTestItem(t, s, ws.ID, col.ID, "Drifted "+itoa(i), linkerBody)
		if _, err := s.db.Exec(s.q(`UPDATE items SET content = ? WHERE id = ?`),
			strings.Repeat("z", body), it.ID); err != nil {
			t.Fatalf("simulate drift: %v", err)
		}
	}

	newTitle := "New"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("cascade refused, but the drifted sources should only have been charged "+
			"for their reads: %v", err)
	}
}
