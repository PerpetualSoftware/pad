package server

import (
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// TestCollapseAutosaveBursts guards the BUG-1612 clutter fix: the web editor
// flushes a collab-snapshot version every ~5s while typing, so an uninterrupted
// burst should collapse to its single newest entry in the item timeline. Manual
// saves (web/cli) and any non-autosave event between two autosaves break the run.
func autosaveEntry(id string, at time.Time) models.TimelineEntry {
	return models.TimelineEntry{
		ID:        id,
		Kind:      "version",
		CreatedAt: at,
		Source:    "collab-snapshot",
		Version:   &models.Version{ID: id, Source: "collab-snapshot", IsDiff: true},
	}
}

func versionEntry(id string, at time.Time, source string) models.TimelineEntry {
	return models.TimelineEntry{
		ID:        id,
		Kind:      "version",
		CreatedAt: at,
		Source:    source,
		Version:   &models.Version{ID: id, Source: source},
	}
}

func commentEntry(id string, at time.Time) models.TimelineEntry {
	return models.TimelineEntry{ID: id, Kind: "comment", CreatedAt: at, Source: "web"}
}

func idsOf(entries []models.TimelineEntry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.ID
	}
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestCollapseAutosaveBursts(t *testing.T) {
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		// entries are newest-first, mirroring buildTimeline's post-sort order.
		in   []models.TimelineEntry
		want []string
	}{
		{
			name: "uninterrupted burst collapses to newest",
			in: []models.TimelineEntry{
				autosaveEntry("a3", base),
				autosaveEntry("a2", base.Add(-5*time.Second)),
				autosaveEntry("a1", base.Add(-10*time.Second)),
			},
			want: []string{"a3"},
		},
		{
			name: "non-autosave event between autosaves breaks the run",
			in: []models.TimelineEntry{
				autosaveEntry("a2", base),
				commentEntry("c1", base.Add(-30*time.Second)),
				autosaveEntry("a1", base.Add(-60*time.Second)),
			},
			want: []string{"a2", "c1", "a1"},
		},
		{
			name: "autosaves more than the window apart are kept separately",
			in: []models.TimelineEntry{
				autosaveEntry("a2", base),
				autosaveEntry("a1", base.Add(-11*time.Minute)),
			},
			want: []string{"a2", "a1"},
		},
		{
			name: "manual web saves are never collapsed",
			in: []models.TimelineEntry{
				versionEntry("v2", base, "web"),
				versionEntry("v1", base.Add(-5*time.Second), "web"),
			},
			want: []string{"v2", "v1"},
		},
		{
			name: "long burst chains across the window from its newest edge",
			in: []models.TimelineEntry{
				autosaveEntry("a4", base),
				autosaveEntry("a3", base.Add(-5*time.Second)),
				autosaveEntry("a2", base.Add(-10*time.Second)),
				autosaveEntry("a1", base.Add(-15*time.Second)),
			},
			want: []string{"a4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := idsOf(collapseAutosaveBursts(tc.in))
			if !equalIDs(got, tc.want) {
				t.Fatalf("collapseAutosaveBursts() = %v, want %v", got, tc.want)
			}
		})
	}
}

func autosaveBy(id, user string, at time.Time, added, removed *int) models.TimelineEntry {
	e := autosaveEntry(id, at)
	e.Version.UserID = user
	e.Version.LinesAdded = added
	e.Version.LinesRemoved = removed
	return e
}

func intp(v int) *int { return &v }

// PLAN-2348 U3: the kept row says what it stands for, and two writers'
// autosaves are two runs.
func TestCollapseAutosaveBursts_RunDescribesDroppedRows(t *testing.T) {
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	in := []models.TimelineEntry{
		autosaveBy("a3", "dave", base.Add(2*time.Minute), intp(3), intp(1)),
		autosaveBy("a2", "dave", base.Add(time.Minute), intp(5), intp(0)),
		autosaveBy("a1", "dave", base, intp(10), intp(5)),
	}
	out := collapseAutosaveBursts(in)
	if got := idsOf(out); !equalIDs(got, []string{"a3"}) {
		t.Fatalf("ids = %v, want [a3]", got)
	}
	run := out[0].AutosaveRun
	if run == nil {
		t.Fatal("the collapsed row carries no autosave_run")
	}
	if run.Count != 3 || run.OldestVersionID != "a1" || !run.FirstAt.Equal(base) {
		t.Fatalf("run = %+v, want count 3, oldest a1, first_at %v", run, base)
	}
	if run.LinesAdded == nil || *run.LinesAdded != 18 || run.LinesRemoved == nil || *run.LinesRemoved != 6 {
		t.Fatalf("run lines = %v/%v, want +18 -6", run.LinesAdded, run.LinesRemoved)
	}
	// The input rows are not mutated: the entries share Version pointers
	// with the store's slice, and the run lives on the entry only.
	if in[0].AutosaveRun != nil {
		t.Fatal("collapse mutated its input entry")
	}
}

func TestCollapseAutosaveBursts_SplitsByWriter(t *testing.T) {
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	in := []models.TimelineEntry{
		autosaveBy("b2", "ann", base.Add(3*time.Minute), intp(1), intp(0)),
		autosaveBy("b1", "ann", base.Add(2*time.Minute), intp(1), intp(0)),
		autosaveBy("a2", "dave", base.Add(time.Minute), intp(1), intp(0)),
		autosaveBy("a1", "dave", base, intp(1), intp(0)),
	}
	out := collapseAutosaveBursts(in)
	if got := idsOf(out); !equalIDs(got, []string{"b2", "a2"}) {
		t.Fatalf("ids = %v, want [b2 a2]", got)
	}
	if out[0].AutosaveRun.Count != 2 || out[1].AutosaveRun.Count != 2 {
		t.Fatalf("counts = %d/%d, want 2/2", out[0].AutosaveRun.Count, out[1].AutosaveRun.Count)
	}
}

func TestCollapseAutosaveBursts_UnknownCountsLeaveRunUncounted(t *testing.T) {
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	out := collapseAutosaveBursts([]models.TimelineEntry{
		autosaveBy("a2", "dave", base.Add(time.Minute), intp(4), intp(1)),
		autosaveBy("a1", "dave", base, nil, nil),
	})
	if run := out[0].AutosaveRun; run == nil || run.Count != 2 || run.LinesAdded != nil || run.LinesRemoved != nil {
		t.Fatalf("run = %+v, want count 2 and no line counts", run)
	}
}

func TestCollapseAutosaveBursts_LoneAutosaveHasNoRun(t *testing.T) {
	base := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)
	out := collapseAutosaveBursts([]models.TimelineEntry{autosaveBy("a1", "dave", base, intp(1), intp(0))})
	if out[0].AutosaveRun != nil {
		t.Fatalf("a lone autosave carries a run: %+v", out[0].AutosaveRun)
	}
}

// BUG-3379: an imported autosave never folds into a native one or back. Both
// usually carry no user id, so the user check alone let them merge, and the
// run keeps only its head's provenance.
func TestCollapseAutosaveBursts_ImportedNeverFoldsWithNative(t *testing.T) {
	base := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	native := autosaveEntry("native", base)
	imported := autosaveEntry("imported", base.Add(-time.Minute))
	imported.Version.Imported = true
	if got := idsOf(collapseAutosaveBursts([]models.TimelineEntry{native, imported})); !equalIDs(got, []string{"native", "imported"}) {
		t.Errorf("collapsed to %v, want both kept", got)
	}
	// Control: two native autosaves in the same window still fold.
	if got := idsOf(collapseAutosaveBursts([]models.TimelineEntry{native, autosaveEntry("older", base.Add(-time.Minute))})); !equalIDs(got, []string{"native"}) {
		t.Errorf("control collapsed to %v, want [native]", got)
	}
}
