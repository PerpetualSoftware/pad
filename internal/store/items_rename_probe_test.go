package store

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-2804 PROBE. Not a regression test and not a guard — a measurement
// instrument, kept in the tree so the numbers on the trail can be reproduced
// and re-run against any proposed cap.
//
// It measures the ITEM-side rename cascade (wiki_links.go::cascadeTitleRename),
// which BUG-2798 did not touch. BUG-2798's arithmetic is explicitly
// NON-TRANSFERABLE per that filing and the dispatch: different selection (an
// indexed id equality, not a LIKE), a different rewriter (RewriteBracketAt at a
// recorded offset, not ReplaceTitle over the whole body), and — as this probe
// exists to establish — a different retention structure.
//
// INSTRUMENT: runtime.MemStats.TotalAlloc delta, the same counter
// documents_rename_bounds_test.go uses, so the two paths' figures are directly
// comparable. TotalAlloc is CUMULATIVE ALLOCATION, not peak residency: it is
// deterministic and GC-independent, which is what makes it usable as an
// assertion. Peak retained bytes are sampled separately below and are
// explicitly the noisier of the two.
//
// Every figure this file prints is measured on the machine that ran it. Nothing
// here is arithmetic presented as a measurement.

// renameProbeResult is one measured cell.
type renameProbeResult struct {
	linkers      int
	bracketsEach int
	bodyBytes    int
	totalAlloc   uint64
	peakHeap     uint64
	baseHeap     uint64
}

func (r renameProbeResult) String() string {
	return fmt.Sprintf(
		"k=%-4d brackets=%-5d body=%-9d | TotalAlloc=%-12d peakHeapDelta=%-12d",
		r.linkers, r.bracketsEach, r.bodyBytes, r.totalAlloc, r.peakHeap-r.baseHeap)
}

// probeBody builds a linker body of approximately bodyBytes containing exactly
// `brackets` occurrences of [[title]], padded with filler that contains no
// bracket characters so the parser sees exactly the intended link count.
func probeBody(t *testing.T, title string, brackets, bodyBytes int) string {
	t.Helper()
	link := "[[" + title + "]]"
	linkBytes := len(link) * brackets
	if linkBytes > bodyBytes {
		t.Fatalf("probe misconfigured: %d brackets of %q need %d bytes, body is %d",
			brackets, link, linkBytes, bodyBytes)
	}
	// Distribute the filler evenly between brackets.
	filler := bodyBytes - linkBytes
	perGap := filler / brackets
	var b strings.Builder
	b.Grow(bodyBytes + perGap)
	for i := 0; i < brackets; i++ {
		b.WriteString(link)
		b.WriteString(strings.Repeat("x", perGap))
	}
	return b.String()
}

// measureRename runs one cascade and returns its allocation profile.
//
// The sampler goroutine that tracks peak HeapAlloc is deliberately secondary:
// it can miss a peak between samples, so it is reported as a FLOOR on the peak
// and never used for an assertion. TotalAlloc carries the load-bearing claims.
func measureRename(t *testing.T, linkers, brackets, bodyBytes int) renameProbeResult {
	t.Helper()

	const oldTitle = "Old Probe Title"
	s := testStore(t)
	ws := createTestWorkspace(t, s, "RenameProbe")
	col := createTestCollection(t, s, ws.ID, "Tasks")

	target := createTestItem(t, s, ws.ID, col.ID, oldTitle, "the item being renamed")
	body := probeBody(t, oldTitle, brackets, bodyBytes)
	for i := 0; i < linkers; i++ {
		createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Linker %d", i), body)
	}

	// Confirm the premise before measuring it: the cascade only does work if
	// the index actually recorded these as title-kind links to the target. A
	// probe that measures an empty cascade would report a flat line and read
	// as "no amplification".
	var indexed int
	if err := s.db.QueryRow(s.q(`
		SELECT COUNT(*) FROM item_wiki_links
		WHERE target_item_id = ? AND target_kind = 'title'
	`), target.ID).Scan(&indexed); err != nil {
		t.Fatalf("probe: count indexed links: %v", err)
	}
	if want := linkers * brackets; indexed != want {
		t.Fatalf("probe precondition: %d indexed title links, want %d — "+
			"the cascade would not be exercised", indexed, want)
	}

	newTitle := "New Probe Title"

	// The sampler MUST sleep between reads. runtime.ReadMemStats stops the
	// world on every call, so a spin loop does not merely add overhead — it
	// serialises the operation it is measuring against a global pause and
	// turned a sub-second cascade into minutes of 130%-CPU wall clock on the
	// first run of this probe. TotalAlloc's VALUE survives that (it counts
	// bytes allocated, which STW does not change), which is exactly why the
	// load-bearing claims are pinned to TotalAlloc and not to this sampler.
	//
	// At 1ms the sampler is a coarse FLOOR on peak residency and is reported
	// as such — never as the peak itself, and never used for an assertion.
	stop := make(chan struct{})
	done := make(chan uint64)
	go func() {
		var peak uint64
		var ms runtime.MemStats
		for {
			select {
			case <-stop:
				done <- peak
				return
			default:
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > peak {
					peak = ms.HeapAlloc
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
		close(stop)
		<-done
		t.Fatalf("probe rename: %v", err)
	}
	runtime.ReadMemStats(&after)
	close(stop)
	peak := <-done

	return renameProbeResult{
		linkers:      linkers,
		bracketsEach: brackets,
		bodyBytes:    bodyBytes,
		totalAlloc:   after.TotalAlloc - before.TotalAlloc,
		peakHeap:     peak,
		baseHeap:     before.HeapAlloc,
	}
}

// TestProbeBUG2804_ScalesWithLinkerCount sweeps the number of linking items at
// a fixed body size, establishing whether the cascade's cost is linear in k and
// what the per-linker slope actually is.
//
// NEGATIVE CONTROL: k=0 (a rename nothing links to) is measured first. Without
// it, a flat line across k could mean "bounded" or could mean "the cascade
// never ran", and the probe could not tell them apart.
func TestProbeBUG2804_ScalesWithLinkerCount(t *testing.T) {
	const body = 256 << 10 // 256 KiB per linker
	const brackets = 8

	var results []renameProbeResult
	for _, k := range []int{0, 1, 2, 4, 8, 16} {
		if k == 0 {
			// Control: same shape, no linkers. probeBody is not called.
			s := testStore(t)
			ws := createTestWorkspace(t, s, "RenameProbeControl")
			col := createTestCollection(t, s, ws.ID, "Tasks")
			target := createTestItem(t, s, ws.ID, col.ID, "Old Probe Title", "the item being renamed")
			newTitle := "New Probe Title"
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
				t.Fatalf("control rename: %v", err)
			}
			runtime.ReadMemStats(&after)
			r := renameProbeResult{linkers: 0, bracketsEach: 0, bodyBytes: 0,
				totalAlloc: after.TotalAlloc - before.TotalAlloc, peakHeap: 0, baseHeap: 0}
			results = append(results, r)
			t.Logf("CONTROL (no linkers): TotalAlloc=%d", r.totalAlloc)
			continue
		}
		r := measureRename(t, k, brackets, body)
		results = append(results, r)
		t.Logf("%s", r)
	}

	t.Logf("--- per-linker slope, body=%d brackets=%d ---", body, brackets)
	for i := 2; i < len(results); i++ {
		prev, cur := results[i-1], results[i]
		dk := cur.linkers - prev.linkers
		if dk <= 0 {
			continue
		}
		d := int64(cur.totalAlloc) - int64(prev.totalAlloc)
		t.Logf("k %d->%d: dTotalAlloc=%d, per added linker=%d (=%.1fx body)",
			prev.linkers, cur.linkers, d, d/int64(dk), float64(d)/float64(dk)/float64(body))
	}
}

// TestProbeBUG2804_ScalesWithBracketCount sweeps the number of brackets inside
// ONE linker at a fixed body size. This isolates the per-bracket rescan: the
// rewrite loop calls links.RewriteBracketAt once per recorded row, and each
// call takes the whole body and returns a new one.
//
// A flat line here means the rewriter is cheap per bracket; a linear one means
// a single linker with many links to the renamed item amplifies on its own,
// independently of how many linkers exist.
func TestProbeBUG2804_ScalesWithBracketCount(t *testing.T) {
	const body = 256 << 10

	var results []renameProbeResult
	for _, n := range []int{1, 8, 64, 512, 4096} {
		r := measureRename(t, 1, n, body)
		results = append(results, r)
		t.Logf("%s", r)
	}

	t.Logf("--- per-bracket slope, k=1 body=%d ---", body)
	for i := 1; i < len(results); i++ {
		prev, cur := results[i-1], results[i]
		dn := cur.bracketsEach - prev.bracketsEach
		d := int64(cur.totalAlloc) - int64(prev.totalAlloc)
		t.Logf("brackets %d->%d: dTotalAlloc=%d, per added bracket=%d (=%.2fx body)",
			prev.bracketsEach, cur.bracketsEach, d, d/int64(dn),
			float64(d)/float64(dn)/float64(body))
	}
}

// TestProbeBUG2804_QuadraticInOneBodySize is the load-bearing measurement.
//
// The bracket count is not an independent knob in an attack — it is a FUNCTION
// of the body size, because the cheapest link is 5 bytes (`[[A]]`) and nothing
// caps how many an item may contain (verified: no per-item wiki-link limit
// exists in internal/). So a body of C bytes carries C/5 brackets, and the
// rewrite loop calls RewriteBracketAt once per bracket, each call rebuilding
// the whole body (links.go:148 — `content[:position] + ... + content[end:]`).
//
// That makes cumulative allocation O(C^2) for a SINGLE linker in a SINGLE
// request, with no accumulation across requests required. This test measures
// the exponent rather than asserting it: doubling C should roughly QUADRUPLE
// TotalAlloc. Anything near 2x instead of 4x refutes the quadratic reading.
//
// Sizes are kept small deliberately — the point is the exponent, and the
// extrapolation to a full 2 MiB body is stated on the trail AS an
// extrapolation, never as a measurement.
func TestProbeBUG2804_QuadraticInOneBodySize(t *testing.T) {
	type cell struct {
		c     int
		alloc uint64
	}
	var cells []cell
	for _, c := range []int{8 << 10, 16 << 10, 32 << 10, 64 << 10} {
		brackets := c / len("[[A]]")
		s := testStore(t)
		ws := createTestWorkspace(t, s, "RenameProbeQuad")
		col := createTestCollection(t, s, ws.ID, "Tasks")
		target := createTestItem(t, s, ws.ID, col.ID, "A", "the item being renamed")
		createTestItem(t, s, ws.ID, col.ID, "Linker", strings.Repeat("[[A]]", brackets))

		var indexed int
		if err := s.db.QueryRow(s.q(`
			SELECT COUNT(*) FROM item_wiki_links
			WHERE target_item_id = ? AND target_kind = 'title'
		`), target.ID).Scan(&indexed); err != nil {
			t.Fatalf("count indexed: %v", err)
		}
		if indexed != brackets {
			t.Fatalf("precondition: %d indexed links, want %d", indexed, brackets)
		}

		newTitle := "B"
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
			t.Fatalf("rename at C=%d: %v", c, err)
		}
		runtime.ReadMemStats(&after)
		a := after.TotalAlloc - before.TotalAlloc
		cells = append(cells, cell{c: c, alloc: a})
		t.Logf("C=%-8d brackets=%-6d TotalAlloc=%-14d (=%.0fx body)",
			c, brackets, a, float64(a)/float64(c))
	}

	t.Logf("--- growth factor per doubling of C (2.0 = linear, 4.0 = quadratic) ---")
	for i := 1; i < len(cells); i++ {
		t.Logf("C %d->%d: alloc x%.2f",
			cells[i-1].c, cells[i].c,
			float64(cells[i].alloc)/float64(cells[i-1].alloc))
	}
}

// TestProbeBUG2804_OutboxPayloadCarriesEveryBody isolates the third copy.
//
// The peak-residency figures are consistent with three simultaneous copies of
// the linker set (works + member snapshots + marshalled JSON), but "consistent
// with" is not a measurement of any one of them. This measures the outbox
// share directly and deterministically, by reading the row the cascade wrote:
// no sampler, no GC timing, no inference.
//
// It also measures a cost the memory figures miss entirely — the payload is
// PERSISTED, so a rename cascading k linkers writes k bodies to the outbox
// table as one row, on disk, per rename.
//
// emitBulkItemEventTx's own doc comment says this is deliberate and unbounded
// in v1, and invites exactly this measurement:
//
//	"PAYLOAD SIZE IS DELIBERATELY UNBOUNDED IN V1 ... If a real workspace shows
//	 this producing unreasonable rows, bounding it is a measured follow-up, not
//	 a guess made here."
func TestProbeBUG2804_OutboxPayloadCarriesEveryBody(t *testing.T) {
	const body = 256 << 10
	const brackets = 4

	for _, k := range []int{1, 4, 16} {
		s := testStore(t)
		ws := createTestWorkspace(t, s, "RenameProbeOutbox")
		col := createTestCollection(t, s, ws.ID, "Tasks")
		target := createTestItem(t, s, ws.ID, col.ID, "Old Probe Title", "the item being renamed")
		linkerBody := probeBody(t, "Old Probe Title", brackets, body)
		for i := 0; i < k; i++ {
			createTestItem(t, s, ws.ID, col.ID, fmt.Sprintf("Linker %d", i), linkerBody)
		}

		// Baseline: outbox bytes already present before the rename, so the
		// delta attributes only what the cascade itself wrote.
		var beforeBytes int64
		if err := s.db.QueryRow(s.q(
			`SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM event_outbox`)).Scan(&beforeBytes); err != nil {
			t.Skipf("outbox table not readable in this configuration: %v", err)
		}

		newTitle := "New Probe Title"
		if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &newTitle}); err != nil {
			t.Fatalf("rename: %v", err)
		}

		var afterBytes int64
		if err := s.db.QueryRow(s.q(
			`SELECT COALESCE(SUM(LENGTH(payload)), 0) FROM event_outbox`)).Scan(&afterBytes); err != nil {
			t.Fatalf("read outbox after: %v", err)
		}
		var maxRow int64
		if err := s.db.QueryRow(s.q(
			`SELECT COALESCE(MAX(LENGTH(payload)), 0) FROM event_outbox`)).Scan(&maxRow); err != nil {
			t.Fatalf("read max outbox row: %v", err)
		}

		bodySet := int64(body) * int64(k)
		t.Logf("k=%-3d bodySet=%-10d outboxDelta=%-11d largestSingleRow=%-11d (=%.2fx the body set)",
			k, bodySet, afterBytes-beforeBytes, maxRow,
			float64(afterBytes-beforeBytes)/float64(bodySet))
	}
}

// TestProbeBUG2804_ConstantDependsOnTitleLengthParity checks whether the
// measured 2.01x-body-per-bracket constant is really TWO O(C) operations per
// bracket, as the code suggests: the concatenation in RewriteBracketAt, plus
// the `rewritten != newContent` full-body string comparison the cascade uses
// to set `mutated` (wiki_links.go:679).
//
// Go compares string LENGTHS first, so that comparison is O(1) when the
// rewrite changed the body's length and O(C) when it did not. Both earlier
// sweeps used same-length titles ("A"->"B", "Old Probe Title"->"New Probe
// Title"), which is the O(C) case — so the 2.01x constant may be an artifact
// of that choice rather than a property of the path.
//
// This runs the SAME shape with a different-length new title. If the constant
// falls to ~1x, the two-operations reading is confirmed and the honest
// statement of the constant is "1x or 2x depending on title-length parity".
// If it stays at 2x, that reading is wrong and something else is allocating.
//
// Either way the QUADRATIC is unaffected: the concatenation alone is O(C) per
// bracket. This measures the constant, not the exponent.
func TestProbeBUG2804_ConstantDependsOnTitleLengthParity(t *testing.T) {
	const body = 256 << 10
	const brackets = 512

	for _, tc := range []struct {
		name     string
		newTitle string
	}{
		{"same-length (comparison is O(C))", "B"},
		{"longer     (comparison is O(1))", "BBBBBBBBBBBBBBBB"},
	} {
		s := testStore(t)
		ws := createTestWorkspace(t, s, "RenameProbeParity")
		col := createTestCollection(t, s, ws.ID, "Tasks")
		target := createTestItem(t, s, ws.ID, col.ID, "A", "the item being renamed")
		createTestItem(t, s, ws.ID, col.ID, "Linker", probeBody(t, "A", brackets, body))

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if _, err := s.UpdateItem(target.ID, models.ItemUpdate{Title: &tc.newTitle}); err != nil {
			t.Fatalf("rename %s: %v", tc.name, err)
		}
		runtime.ReadMemStats(&after)
		a := after.TotalAlloc - before.TotalAlloc
		t.Logf("%-34s TotalAlloc=%-12d per bracket=%-9d (=%.2fx body)",
			tc.name, a, a/brackets, float64(a)/float64(brackets)/float64(body))
	}
}

// TestProbeBUG2804_SelectMaterialisesContentPerLinkRow isolates the SECOND
// O(C)-per-bracket cost, which is not in the rewriter at all.
//
// links.RewriteBracketAt allocates 1.00x the body per call, measured in
// isolation (internal/links/probe_alloc_test.go). The cascade's marginal cost
// is 2.01x per bracket. This measures where the other 1.00x lives.
//
// cascadeTitleRename's SELECT (wiki_links.go:616) joins item_wiki_links to
// items and projects s.content, so it returns ONE ROW PER LINK, each carrying
// a full copy of the source body. The scan loop de-duplicates into `works` by
// source id — but the de-duplication happens AFTER rows.Scan has already
// allocated a fresh string for every row.
//
// So a single source with N brackets makes the driver materialise N full
// bodies to build one `works` entry. That is a second quadratic, independent
// of the rewriter: making the rewrite single-pass would leave it untouched.
//
// The probe runs the cascade's own SELECT verbatim at increasing bracket
// counts against ONE source, so any growth is per-ROW cost and not per-source.
func TestProbeBUG2804_SelectMaterialisesContentPerLinkRow(t *testing.T) {
	const body = 256 << 10

	for _, brackets := range []int{8, 64, 512, 4096} {
		s := testStore(t)
		ws := createTestWorkspace(t, s, "RenameProbeSelect")
		col := createTestCollection(t, s, ws.ID, "Tasks")
		target := createTestItem(t, s, ws.ID, col.ID, "A", "the item being renamed")
		createTestItem(t, s, ws.ID, col.ID, "Linker", probeBody(t, "A", brackets, body))

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		rows, err := s.db.Query(s.q(`
			SELECT s.id, s.content, s.workspace_id, wl.position, wl.target_title
			FROM item_wiki_links wl
			JOIN items s ON s.id = wl.source_item_id
			WHERE wl.target_kind = 'title'
			  AND wl.target_workspace_id IS NULL
			  AND wl.target_item_id = ?
			  AND s.deleted_at IS NULL
			ORDER BY s.id, wl.position DESC
		`), target.ID)
		if err != nil {
			t.Fatalf("select: %v", err)
		}
		n := 0
		distinct := map[string]bool{}
		for rows.Next() {
			var id, content, wsID, targetTitle string
			var position int
			if err := rows.Scan(&id, &content, &wsID, &position, &targetTitle); err != nil {
				rows.Close()
				t.Fatalf("scan: %v", err)
			}
			distinct[id] = true
			n++
		}
		rows.Close()
		runtime.ReadMemStats(&after)

		a := after.TotalAlloc - before.TotalAlloc
		t.Logf("brackets=%-6d rows=%-6d distinctSources=%-3d SELECT+scan alloc=%-12d per row=%-9d (=%.2fx body)",
			brackets, n, len(distinct), a, a/uint64(n), float64(a)/float64(n)/float64(body))
	}
}
