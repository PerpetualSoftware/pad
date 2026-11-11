package store

import (
	"os"
	"reflect"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

func TestTASK4476_ComponentAccess(t *testing.T) {
	for _, backend := range []string{"sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var s *Store
			var ws *models.Workspace
			var items []*models.Item
			var scs []*models.ItemSchedule
			var access models.CommitmentAccess
			if backend == "postgres" {
				if os.Getenv("PAD_TEST_POSTGRES_URL") == "" {
					t.Skip("PAD_TEST_POSTGRES_URL not set")
				}
				f := newLinkPG(t, 9)
				s, ws, items, scs, access = f.s, f.ws, f.items, f.scs, f.access()
			} else {
				f := newLinkFix(t, 9)
				s, ws, items, scs, access = f.s, f.ws, f.items, f.scheds, f.allEdit()
			}
			for _, pair := range [][2]int{{0, 1}, {1, 2}, {3, 4}} {
				_, err := s.CreateScheduleLink(ws.ID, models.ScheduleLinkCreate{ParentItemID: items[pair[0]].ID, ChildItemID: items[pair[1]].ID, ParentRole: "target", ChildRole: "target", Offset: oneDay}, access)
				if err != nil {
					t.Fatal(err)
				}
			}
			// Gates extend the link component, including an item with no schedule.
			if _, err := s.DeleteSchedule(ws.ID, items[5].ID); err != nil {
				t.Fatal(err)
			}
			for _, pair := range [][2]int{{5, 2}, {6, 5}} {
				if _, err := s.CreateScheduleGate(ws.ID, items[pair[0]].ID, items[pair[1]].ID, access); err != nil {
					t.Fatal(err)
				}
			}
			// Only the seed item's lineage extends the set, not another component.
			for _, i := range []int{0, 7, 8} {
				if _, err := s.db.Exec(s.q(`INSERT INTO schedule_changes (id,workspace_id,item_id,schedule_id,change_type,root_change_id,recorded_at) VALUES (?,?,?,?,'create',?,'2026-01-01T00:00:00Z')`), "lineage-"+items[i].ID, ws.ID, items[i].ID, scs[i].ID, "lineage-"+items[i].ID); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec(s.q(`UPDATE schedule_changes SET root_change_id=?,parent_change_id=? WHERE item_id=?`), "lineage-"+items[7].ID, "lineage-"+items[8].ID, items[0].ID); err != nil {
				t.Fatal(err)
			}
			for _, seed := range []int{0, 2, 5, 6} {
				rows, err := s.CommitmentAccessItemsQ(s.Q(), ws.ID, items[seed].ID)
				if err != nil {
					t.Fatal(err)
				}
				var got, want []string
				for _, r := range rows {
					got = append(got, r.ID)
				}
				for _, i := range []int{0, 1, 2, 5, 6} {
					want = append(want, items[i].ID)
				}
				if seed == 0 {
					want = append(want, items[7].ID, items[8].ID)
				}
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d: component has %d items, want %d; got %v want %v", seed, len(got), len(want), got, want)
				}
			}
		})
	}
}
