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
