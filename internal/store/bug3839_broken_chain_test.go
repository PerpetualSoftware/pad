package store_test

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/diff"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3839: when a version row fails to apply, History keeps the body it had
// and applies the next older row over it, a base that row was not made from.
// go-diff panics on that for some multi-byte text, and the request was a 500.
// This walk has done that since the initial release. All text here is valid
// UTF-8; the older row reads as patch_failed instead.
func TestBUG3839_HistoryPastABrokenRowDoesNotPanic(t *testing.T) {
	recorded, older, current := "a\né+abb😀", "%a+a😀ab", "é😀ab😀+%"
	for _, backend := range contentBackends() {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)
			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BUG-3839"})
			if err != nil {
				t.Fatal(err)
			}
			col, err := s.CreateCollection(ws.ID, models.CollectionCreate{Name: "Docs", Schema: `{"fields":[]}`})
			if err != nil {
				t.Fatal(err)
			}
			item, err := s.CreateItem(ws.ID, col.ID, models.ItemCreate{Title: "Doc", Content: current, Fields: `{}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.DB().Exec(`DELETE FROM item_versions WHERE item_id = $1`, item.ID); err != nil {
				t.Fatal(err)
			}
			for _, r := range []struct {
				id, content, at string
				seq             int
			}{
				// The newest row's patch does not apply to the current body.
				{"r1", "@@ -1,3 +1,3 @@\n-zzz\n+yyy\n", "2026-10-02T00:00:00Z", 2},
				// The older row was made from the body r1 recorded.
				{"r0", diff.CreatePatch(recorded, older), "2026-10-01T00:00:00Z", 1},
			} {
				if _, err := s.DB().Exec(`INSERT INTO item_versions (id, item_id, content, is_diff, created_at, version_seq) VALUES ($1, $2, $3, $4, $5, $6)`,
					r.id, item.ID, r.content, true, r.at, r.seq); err != nil {
					t.Fatal(err)
				}
			}
			vs, err := s.ListItemVersionsResolved(item.ID, current)
			if err != nil || len(vs) != 2 {
				t.Fatalf("versions: %d %v", len(vs), err)
			}
			for _, v := range vs {
				if !v.Unreconstructable {
					t.Fatalf("a row resolved over a base it was not made from: %q", v.Content)
				}
			}
		})
	}
}
