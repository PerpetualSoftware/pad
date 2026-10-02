package store

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3327: a collab-snapshot flush skips the version of a body the newest
// version already holds ONLY when that newest row is recognised as the
// applier edit's: content_flushed_at present and not newer than it. A NULL
// content_flushed_at (migration 052 leaves it so on items with op-log rows)
// is no evidence, so the flush versions (codex review, round 4).
func TestBUG3327_FlushDedupeNeedsTheFlushTimestamp(t *testing.T) {
	for _, tc := range []struct {
		name        string
		nullFlushed bool
		wantDelta   int
	}{
		{"applier row recognised: flush skips the duplicate", false, 0},
		{"no content_flushed_at: no evidence, flush versions", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ws := createTestWorkspace(t, s, "Dedupe")
			coll := createTestCollection(t, s, ws.ID, "Tasks")
			item := createTestItem(t, s, ws.ID, coll.ID, "Subject", "body A")

			// The applier-path edit: versions body A as a full body, the row
			// keeps body A.
			next := "body B"
			if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{
				ExternalContent: &next, LastModifiedBy: "agent", VersionSource: "cli",
			}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if tc.nullFlushed {
				if _, err := s.db.Exec(s.q(`UPDATE items SET content_flushed_at = NULL WHERE id = ?`), item.ID); err != nil {
					t.Fatal(err)
				}
			}
			before, err := s.ListItemVersions(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.UpdateItemWithParentLink(item.ID, models.ItemUpdate{
				Content: &next, LastModifiedBy: "user", VersionSource: "collab-snapshot",
			}, nil, nil); err != nil {
				t.Fatal(err)
			}
			after, err := s.ListItemVersions(item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(after) - len(before); got != tc.wantDelta {
				t.Fatalf("flush added %d version rows, want %d", got, tc.wantDelta)
			}
		})
	}
}
