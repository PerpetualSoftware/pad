package store_test

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3033 — backlink snippets, the door codex round 2 found and this unit's
// sweep missed because its vantage point was internal/server, internal/mcp and
// internal/cli. This shape is built in the STORE, so nothing reachable from
// those three packages could see it.
//
// A snippet is a window onto the source item's body, not metadata about it, so
// a stale body makes a stale snippet — the same reasoning that marks an
// ItemSummary preview and a playbook summary.
//
// Both directions on one item, absence first, on both backends: the predicate
// here is SQL with a non-default table alias, which is precisely where the two
// dialects can diverge.
func TestBacklinkSnippetsCarryTheMarkerBothWays(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(t *testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: "BL", Slug: "bl"})
			if err != nil {
				t.Fatalf("CreateWorkspace: %v", err)
			}
			if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			colls, err := s.ListCollections(ws.ID)
			if err != nil {
				t.Fatalf("ListCollections: %v", err)
			}
			var tasks string
			for _, c := range colls {
				if c.Slug == "tasks" {
					tasks = c.ID
				}
			}
			if tasks == "" {
				t.Fatal("no tasks collection")
			}

			target, err := s.CreateItem(ws.ID, tasks, models.ItemCreate{Title: "Target item"})
			if err != nil {
				t.Fatalf("create target: %v", err)
			}
			source, err := s.CreateItem(ws.ID, tasks, models.ItemCreate{
				Title:   "Source item",
				Content: "Some prose that links to [[Target item]] in the middle of a body.",
			})
			if err != nil {
				t.Fatalf("create source: %v", err)
			}

			read := func(t *testing.T) models.Backlink {
				t.Helper()
				bls, err := s.GetBacklinks(target.ID, ws.ID, 50, 0, store.BacklinksVisibility{Unrestricted: true})
				if err != nil {
					t.Fatalf("GetBacklinks: %v", err)
				}
				for _, bl := range bls {
					if bl.SourceItemID == source.ID {
						return bl
					}
				}
				t.Fatalf("the source item produced no backlink; this leg measured nothing (got %d)", len(bls))
				return models.Backlink{}
			}

			// ABSENCE FIRST, with the premise that a snippet was actually
			// produced — a marker on an empty snippet would be a claim about
			// text nobody was served.
			bl := read(t)
			if bl.Snippet == "" {
				t.Fatal("no snippet was produced, so everything below is about nothing")
			}
			if bl.ContentState != "" {
				t.Fatalf("a current source item's snippet is marked %q", bl.ContentState)
			}

			// Put the source's document ahead of its row — the state an
			// applier-path write leaves.
			if _, err := s.AppendYjsUpdate(source.ID, []byte{1, 2, 3}, "1"); err != nil {
				t.Fatalf("AppendYjsUpdate: %v", err)
			}

			bl = read(t)
			if bl.ContentState != models.ContentOutcomeAppliedPendingFlush {
				t.Errorf("backlink content_state = %q, want %q — the snippet is cut from a body that has moved on",
					bl.ContentState, models.ContentOutcomeAppliedPendingFlush)
			}
			if bl.Snippet == "" {
				t.Error("the marked backlink carries no snippet; the marker qualifies the text, it does not replace it")
			}

			// The marker describes the SOURCE item, never the target the caller
			// asked about. Marking the target's row must change nothing here,
			// which is the assertion that keeps the two from being conflated.
			if _, err := s.AppendYjsUpdate(target.ID, []byte{9}, "1"); err != nil {
				t.Fatalf("AppendYjsUpdate(target): %v", err)
			}
			if got := read(t); got.ContentState != models.ContentOutcomeAppliedPendingFlush {
				t.Errorf("after marking the TARGET too, content_state = %q — it must still describe the source",
					got.ContentState)
			}
		})
	}
}
