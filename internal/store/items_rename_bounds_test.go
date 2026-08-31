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
	// Body size chosen so perLinker does NOT divide the cap evenly. With an
	// exact division the assertion below cannot discriminate: refusing after
	// the build would total exactly the cap rather than exceeding it. It also
	// must not assume a precise admitted count, because the scan charges the
	// same budget first (codex R4) and that shifts the boundary by a source.
	const body = 1536 << 10 // 1.5 MiB
	perLinker := int64(body) * 2
	linkers := int(MaxItemRenameCascadeBytes/perLinker) + 3

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

	// Everything the cascade BUILT must fit inside the budget. Building the
	// source that crosses the cap is exactly the defect, and it shows up here
	// as a total that exceeds the cap by one body.
	//
	// Stated as a total rather than an exact count on purpose: the admitted
	// count depends on what the scan charged first, which is a detail of the
	// fixture rather than the property under test.
	if built == 0 {
		t.Fatalf("cascade built nothing — the fixture refused before the rewrite loop ran, so this " +
			"test is not exercising the loop's ordering at all")
	}
	if total := int64(built) * perLinker; total > MaxItemRenameCascadeBytes {
		t.Errorf("cascade built %d bodies totalling %d charged bytes, over the %d cap — the "+
			"projection is running AFTER the body it was supposed to prevent",
			built, total, int64(MaxItemRenameCascadeBytes))
	}
	t.Logf("built %d bodies (%d charged bytes) before refusing; cap %d",
		built, int64(built)*perLinker, int64(MaxItemRenameCascadeBytes))
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

// TestItemRenameCascade_BoundsTheScanNotJustTheRewrite closes codex R4's P1.
//
// The rewrite loop's cap charges CONTENT bytes. The scan that feeds it retains
// one entry per matching link ROW, each carrying that row's target_title.
//
// So the attack needs no large content at all: give the renamed item a large
// title, link it from enough rows, and the cascade retains rows x title bytes
// before reading a single body. A content-bytes cap cannot see it, because the
// content is a few dozen bytes per linker.
//
// This comment used to say item titles have no length bound. Since
// BUG-2833 / BUG-2831 they do — models.MaxItemTitleRunes — and the fixture
// below is built as LEGACY data for exactly that reason. The guard is not
// thereby obsolete: the bound is non-retroactive, this scan reads STORED
// titles rather than the one being written, and at the bound the same pressure
// arrives with ~24,000 link rows instead of 64.
//
// This is the case my own doc comment on MaxItemRenameCascadeBytes previously
// claimed was impossible — it asserted the cascade's retention was O(1) in the
// linker count and attributed the remaining k-linear growth to the outbox
// (BUG-2827), which is a different vector entirely.
func TestItemRenameCascade_BoundsTheScanNotJustTheRewrite(t *testing.T) {
	// The largest title one JSON request can deliver (server.defaultJSONBodyLimit
	// is 2 MiB). This is no longer reachable through the ordinary create path —
	// models.MaxItemTitleRunes refuses it — so the fixture is seeded as legacy
	// data below. The size is kept because it is what keeps the ROW COUNT down,
	// and this test's runtime is dominated by row count.
	const titleBytes = 2 << 20
	hugeTitle := strings.Repeat("T", titleBytes)

	// THREE TIMES the rows needed to cross the cap, not just a few over.
	//
	// The margin is the instrument. A scan that refuses the moment it crosses
	// stops after ~cap bytes; one that accumulates the whole result set and only
	// discovers the problem later reports ~3x that. A fixture only a few rows
	// over cannot tell those apart, which is how the first version of this test
	// let the defect survive.
	rowsToCross := int(MaxItemRenameCascadeBytes / int64(titleBytes))
	rowsNeeded := rowsToCross * 2
	linkBody := "[[" + hugeTitle + "]]"

	s := testStore(t)
	// SQLITE ONLY, and the reason is measured rather than assumed.
	//
	// On Postgres this fixture cannot be built at all: `items` carries
	// UNIQUE(workspace_id, slug), the slug is derived from the title with no
	// truncation (store.go::slugify), and a btree index tuple has a size cap.
	// Creating the target fails with
	//
	//	insert item: ERROR: index row requires 24064 bytes, maximum size is 8191 (SQLSTATE 54000)
	//
	// — 24,064 rather than 2 MiB because the repetitive title compresses hard
	// before indexing. So the huge-title shape is UNREACHABLE on Postgres, and
	// the LEGACY-DATA seeding this test now uses cannot manufacture it either:
	// the row goes in through the same UNIQUE index.
	//
	// That divergence WAS a defect in its own right (same input accepted on one
	// backend, refused with a 500-shaped error on the other — the BUG-2782 /
	// BUG-2784 family). It was filed as BUG-2831 and is now FIXED: item titles
	// are bounded at models.MaxItemTitleRunes on both backends, so the input
	// this comment describes is refused identically either side. What remains
	// dialect-specific is only that a pre-bound legacy ROW can exist on SQLite
	// and not on Postgres, which is why the skip below stays.
	//
	// (The cap that actually fires first is 2704 bytes, not 8191 — measured;
	// see models.MaxItemTitleRunes. The reading quoted above is from the
	// hard-ceiling arm and is left verbatim because it is what BUG-2804's run
	// actually printed.)
	//
	// What it does NOT mean is that the scan bound is SQLite-only. Row count is
	// unbounded on both backends: at Postgres's practical title ceiling the
	// same attack needs roughly 24,000 link rows instead of 64, which a popular
	// item accumulates over time. This test picks the cheap end of that
	// trade — 64 rows instead of 24,000 — and pays for it by being
	// dialect-scoped.
	if s.dialect.Driver() == DriverPostgres {
		t.Skip("huge-title fixture is unreachable on Postgres: UNIQUE(workspace_id, slug) btree caps the index tuple, so even a legacy-seeded row cannot carry this slug")
	}
	ws := createTestWorkspace(t, s, "ItemRenameScanBound")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	// BUG-2833 / BUG-2831: a 2 MiB title is no longer reachable through
	// CreateItem — models.MaxItemTitleRunes refuses it at the door, on both
	// dialects. The scan bound it guards is NOT thereby obsolete: the charge is
	// against each link row's STORED target_title, so a legacy row carrying a
	// pre-bound title still drives exactly this pressure, and even an in-bound
	// title reaches the cap at ~24,000 rows (the number this fixture trades
	// away for 64). Built as legacy data so the test keeps measuring the guard
	// rather than the new door in front of it.
	target := createLegacyTitledItem(t, s, ws.ID, col.ID, hugeTitle, "the item being renamed")

	contentBytes := 0
	for i := 0; i < rowsNeeded; i++ {
		createTestItem(t, s, ws.ID, col.ID, "Linker "+itoa(i), linkBody)
		contentBytes += len(linkBody)
	}

	// FIXTURE PRECONDITION, and the first version of this was the wrong one.
	// I originally required the content to stay small, on the theory that a
	// large-content fixture would trip the rewrite loop's half of the budget
	// instead. That is not achievable and the guard correctly failed the test:
	// a link to a T-byte title costs T bytes of body text, so content and
	// retained-title bytes are COUPLED and content is always the larger.
	//
	// The property that actually distinguishes the two halves is WHEN the
	// refusal fires. Scan bytes alone must cross the cap, so the scan is the
	// binding constraint and the refusal lands before the rewrite loop runs at
	// all — which `built == 0` below then measures. Without the scan charge the
	// same fixture still refuses, but only after the loop has read and built
	// roughly a dozen bodies.
	if scanBytes := int64(rowsNeeded) * int64(titleBytes); scanBytes <= MaxItemRenameCascadeBytes {
		t.Fatalf("fixture: retained title bytes total %d, under the %d cap — the scan would not "+
			"be the binding constraint and this test could not show it is bounded",
			scanBytes, int64(MaxItemRenameCascadeBytes))
	}
	_ = contentBytes

	// And the cascade must not have built anything before refusing.
	var built int
	s.onItemCascadeBodyBuilt = func(int) { built++ }
	defer func() { s.onItemCascadeBodyBuilt = nil }()

	newTitle := "New"
	_, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle})
	if !errors.Is(err, ErrItemRenameCascadeTooLarge) {
		t.Fatalf("rename with %d link rows carrying %d-byte titles: got %v, want "+
			"ErrItemRenameCascadeTooLarge — the scan is unbounded", rowsNeeded, titleBytes, err)
	}
	if built != 0 {
		t.Errorf("cascade built %d bodies, want 0 — the refusal must fire during the SCAN, "+
			"before the rewrite loop reads anything", built)
	}

	// WHERE the refusal fired, which is the part `built == 0` cannot see.
	//
	// Charging during the scan but only CHECKING afterwards would still report
	// zero builds — the accumulated total trips on the loop's very first source
	// — while the scan had already materialised every row. The figure
	// distinguishes them: refusing on crossing stops within ONE ROW of the cap;
	// finishing the scan first reports roughly the whole result set.
	var typed *ItemRenameCascadeTooLargeError
	if !errors.As(err, &typed) {
		t.Fatalf("error does not carry the typed figures: %T", err)
	}
	ceiling := int64(MaxItemRenameCascadeBytes) + int64(titleBytes) + cascadeRowOverheadBytes + cascadeSourceOverheadBytes + 128
	if typed.Processed > ceiling {
		t.Errorf("refused at %d bytes, but crossing the %d cap should stop within one row of it "+
			"(<= %d) — the scan ran to completion and only then noticed, so every row was "+
			"materialised before anything refused", typed.Processed, int64(MaxItemRenameCascadeBytes), ceiling)
	}
	t.Logf("refused at %d bytes (cap %d, one-row ceiling %d) with %d bodies built; fixture carried "+
		"%d rows of %d-byte titles", typed.Processed, int64(MaxItemRenameCascadeBytes), ceiling, built,
		rowsNeeded, titleBytes)

	// Rolled back.
	got, gerr := s.GetItem(target.ID)
	if gerr != nil {
		t.Fatalf("re-read target: %v", gerr)
	}
	if got.Title != hugeTitle {
		t.Errorf("title changed despite the refusal")
	}
}

// TestItemRenameCascade_ScanBoundAllowsRealisticTitles is the counterfactual.
// Without it, charging an absurd per-row overhead — or refusing outright —
// would satisfy the test above while breaking every ordinary rename.
//
// Sized from the live-workspace measurement on MaxItemRenameCascadeBytes: the
// worst real cascade was 23 sources, and item titles there are ordinary
// sentence-length strings.
func TestItemRenameCascade_ScanBoundAllowsRealisticTitles(t *testing.T) {
	const sources = 50
	title := "A Perfectly Ordinary Item Title Of The Kind Real Workspaces Contain"

	s := testStore(t)
	ws := createTestWorkspace(t, s, "ItemRenameScanOrdinary")
	col := createTestCollection(t, s, ws.ID, "Tasks")
	target := createTestItem(t, s, ws.ID, col.ID, title, "the item being renamed")

	// Several links per source, so the row count is well above the source count.
	body := strings.Repeat("see [["+title+"]] and also [["+title+"]]. ", 5)
	for i := 0; i < sources; i++ {
		createTestItem(t, s, ws.ID, col.ID, "Linker "+itoa(i), body)
	}

	newTitle := "A Perfectly Ordinary Renamed Title"
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		t.Fatalf("a realistic cascade was refused by the scan bound: %v", err)
	}

	var rewritten int
	if err := s.db.QueryRow(s.q(
		`SELECT COUNT(*) FROM items WHERE workspace_id = ? AND content LIKE '%' || ? || '%'`),
		ws.ID, "[["+newTitle+"]]").Scan(&rewritten); err != nil {
		t.Fatalf("count rewritten: %v", err)
	}
	if rewritten != sources {
		t.Errorf("%d of %d linkers rewritten — the cascade did not complete", rewritten, sources)
	}
}
