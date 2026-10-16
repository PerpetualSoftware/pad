package store_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
	"github.com/PerpetualSoftware/pad/internal/store/storetest"
)

// BUG-3776: a link row is scoped by its own workspace_id, but the readers that
// enrich or count it joined its endpoint items without asking where THEY
// live. No door writes a cross-workspace row (the link handler refuses a
// target in another workspace), so one exists only as a legacy or malformed
// row, which this test plants directly. Each reader must ignore it: a row
// counts only when both endpoints live in the link row's workspace.
//
// Every assertion that a foreign row is absent sits next to a legitimate row
// in the same reader that must be present, so a reader that returned nothing
// at all could not pass.
func TestBUG3776ForeignEndpointLinkRowsAreIgnored(t *testing.T) {
	for _, backend := range []struct {
		name string
		open func(t *testing.T) *store.Store
	}{
		{"SQLite", storetest.NewSQLite},
		{"Postgres", storetest.NewPostgres}, // skips unless PAD_TEST_POSTGRES_URL is set
	} {
		t.Run(backend.name, func(t *testing.T) {
			s := backend.open(t)

			mkWS := func(slug string) (string, string) {
				t.Helper()
				ws, err := s.CreateWorkspace(models.WorkspaceCreate{Name: slug, Slug: slug})
				if err != nil {
					t.Fatalf("CreateWorkspace %s: %v", slug, err)
				}
				if err := s.SeedCollectionsFromTemplate(ws.ID, "startup"); err != nil {
					t.Fatalf("seed %s: %v", slug, err)
				}
				colls, err := s.ListCollections(ws.ID)
				if err != nil {
					t.Fatalf("ListCollections: %v", err)
				}
				for _, c := range colls {
					if c.Slug == "tasks" {
						return ws.ID, c.ID
					}
				}
				t.Fatalf("no tasks collection in %s", slug)
				return "", ""
			}
			wsA, tasksA := mkWS("bug3776-a")
			wsB, tasksB := mkWS("bug3776-b")
			item := func(ws, coll, title string) *models.Item {
				t.Helper()
				it, err := s.CreateItem(ws, coll, models.ItemCreate{Title: title})
				if err != nil {
					t.Fatalf("create %s: %v", title, err)
				}
				return it
			}
			a1 := item(wsA, tasksA, "A1 foreign parent")
			a2 := item(wsA, tasksA, "A2 local parent")
			a3 := item(wsA, tasksA, "A3 local child")
			a4 := item(wsA, tasksA, "A4 foreign child only")
			a5 := item(wsA, tasksA, "A5 child under a row filed in B")
			a6 := item(wsA, tasksA, "A6 parent under a row filed in B")
			b1 := item(wsB, tasksB, "B1 SECRET TITLE")

			// Legitimate rows, through the store's own doors: the controls.
			if _, err := s.SetParentLinkAs(wsA, a3.ID, a2.ID, "user", ""); err != nil {
				t.Fatalf("legit parent: %v", err)
			}
			if _, err := s.CreateItemLink(wsA, models.ItemLinkCreate{TargetID: a1.ID, LinkType: "blocks"}, a3.ID); err != nil {
				t.Fatalf("legit blocks: %v", err)
			}

			// Malformed rows: workspace A's rows with one endpoint in B.
			ts := time.Now().UTC().Format(time.RFC3339)
			plant := func(id, ws, src, tgt, typ string) {
				t.Helper()
				if _, err := s.DB().Exec(fmt.Sprintf(
					`INSERT INTO item_links (id, workspace_id, source_id, target_id, link_type, created_at) VALUES ('%s', '%s', '%s', '%s', '%s', '%s')`,
					id, ws, src, tgt, typ, ts)); err != nil {
					t.Fatalf("plant %s: %v", id, err)
				}
			}
			plant("00000000-0000-0000-0000-000000037761", wsA, a1.ID, b1.ID, "parent") // A item under a B parent
			plant("00000000-0000-0000-0000-000000037762", wsA, b1.ID, a4.ID, "parent") // a B child under an A item
			plant("00000000-0000-0000-0000-000000037763", wsA, b1.ID, a2.ID, "blocks") // a B blocker of an A item
			// The other malformed shape: both endpoints in A, the ROW filed in B.
			// The workspace-scoped readers never see it; the ones reached by an
			// item id or a parent id must not either.
			plant("00000000-0000-0000-0000-000000037764", wsB, a5.ID, a6.ID, "parent")

			t.Run("GetParentMap", func(t *testing.T) {
				m, err := s.GetParentMap(wsA)
				if err != nil {
					t.Fatal(err)
				}
				if m[a3.ID] != a2.ID {
					t.Fatalf("control: A3's parent should be A2, map %v", m)
				}
				if p, ok := m[a1.ID]; ok {
					t.Errorf("A1 maps to a parent in workspace B (%s)", p)
				}
				if _, ok := m[b1.ID]; ok {
					t.Errorf("a workspace-B item is in workspace A's parent map")
				}
			})

			t.Run("ListWorkspaceGraphLinks", func(t *testing.T) {
				links, err := s.ListWorkspaceGraphLinks(wsA)
				if err != nil {
					t.Fatal(err)
				}
				var control bool
				for _, l := range links {
					if l.SourceID == b1.ID || l.TargetID == b1.ID {
						t.Errorf("graph edge to workspace B: %+v", l)
					}
					if l.SourceID == a3.ID && l.TargetID == a2.ID {
						control = true
					}
				}
				if !control {
					t.Fatalf("control: the A3→A2 edge is missing from %+v", links)
				}
			})

			t.Run("GetBlocksEdges", func(t *testing.T) {
				edges, err := s.GetBlocksEdges(wsA)
				if err != nil {
					t.Fatal(err)
				}
				var control bool
				for _, e := range edges {
					if e.SourceID == b1.ID {
						t.Errorf("blocks edge from workspace B surfaces %q", e.SourceTitle)
					}
					if e.SourceID == a3.ID && e.TargetID == a1.ID {
						control = true
					}
				}
				if !control {
					t.Fatalf("control: the A3 blocks A1 edge is missing from %+v", edges)
				}
			})

			t.Run("GetItemLinks", func(t *testing.T) {
				for _, it := range []*models.Item{a1, a2, a4} {
					links, err := s.GetItemLinks(it.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, l := range links {
						if l.SourceID == b1.ID || l.TargetID == b1.ID {
							t.Errorf("%s: link to workspace B surfaces %q / %q", it.Title, l.SourceTitle, l.TargetTitle)
						}
					}
				}
				links, err := s.GetItemLinks(a2.ID)
				if err != nil {
					t.Fatal(err)
				}
				var control bool
				for _, l := range links {
					if l.SourceID == a3.ID {
						control = true
					}
				}
				if !control {
					t.Fatalf("control: A2's child link from A3 is missing from %+v", links)
				}
			})

			t.Run("GetParentForItem", func(t *testing.T) {
				p, err := s.GetParentForItem(a1.ID)
				if err != nil {
					t.Fatal(err)
				}
				if p != nil {
					t.Errorf("A1's parent resolves to workspace B: %q", p.TargetTitle)
				}
				p, err = s.GetParentForItem(a3.ID)
				if err != nil {
					t.Fatal(err)
				}
				if p == nil || p.TargetID != a2.ID {
					t.Fatalf("control: A3's parent should be A2, got %+v", p)
				}
			})

			t.Run("GetAllItemProgress", func(t *testing.T) {
				prog, err := s.GetAllItemProgress(wsA, "tasks", false)
				if err != nil {
					t.Fatal(err)
				}
				got := map[string]int{}
				for _, p := range prog {
					got[p.ItemID] = p.Total
				}
				if got[a2.ID] != 1 {
					t.Fatalf("control: A2 has one child (A3), progress total %d", got[a2.ID])
				}
				if got[a4.ID] != 0 {
					t.Errorf("A4 counts a workspace-B child: total %d", got[a4.ID])
				}
			})

			t.Run("PopulateHasChildren", func(t *testing.T) {
				items := []models.Item{*a2, *a4}
				s.PopulateHasChildren(items)
				if !items[0].HasChildren {
					t.Fatal("control: A2 has a child")
				}
				if items[1].HasChildren {
					t.Error("A4 has_children from a workspace-B child")
				}
			})

			t.Run("a row filed in another workspace", func(t *testing.T) {
				links, err := s.GetItemLinks(a6.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, l := range links {
					t.Errorf("A6 shows a link filed in workspace B: %q → %q", l.SourceTitle, l.TargetTitle)
				}
				p, err := s.GetParentForItem(a5.ID)
				if err != nil {
					t.Fatal(err)
				}
				if p != nil {
					t.Errorf("A5's parent comes from a row filed in workspace B: %q", p.TargetTitle)
				}
				prog, err := s.GetAllItemProgress(wsA, "tasks", false)
				if err != nil {
					t.Fatal(err)
				}
				for _, pr := range prog {
					if pr.ItemID == a6.ID && pr.Total != 0 {
						t.Errorf("A6 counts a child through a row filed in B: total %d", pr.Total)
					}
				}
				items := []models.Item{*a6}
				s.PopulateHasChildren(items)
				if items[0].HasChildren {
					t.Error("A6 has_children through a row filed in B")
				}
				got, err := s.ListItems(wsA, models.ItemListParams{ParentLinkID: a6.ID})
				if err != nil {
					t.Fatal(err)
				}
				for _, it := range got {
					t.Errorf("A6 lists %q as a child through a row filed in B", it.Title)
				}
				// The controls for these readers are in their own subtests above.
			})

			t.Run("ListItems parent filter", func(t *testing.T) {
				// The filter takes a raw id, so a caller can name an item in
				// another workspace (BUG-734's note on raw-UUID input).
				got, err := s.ListItems(wsA, models.ItemListParams{ParentLinkID: b1.ID})
				if err != nil {
					t.Fatal(err)
				}
				for _, it := range got {
					t.Errorf("workspace A lists %q under a workspace-B parent", it.Title)
				}
				got, err = s.ListItems(wsA, models.ItemListParams{ParentLinkID: a2.ID})
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != 1 || got[0].ID != a3.ID {
					t.Fatalf("control: A2's children should be [A3], got %d items", len(got))
				}
			})
		})
	}
}
