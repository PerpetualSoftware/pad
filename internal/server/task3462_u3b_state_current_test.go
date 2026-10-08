package server

import (
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462 U3b (codex r5): the state response carries the item's CURRENT
// text, read in the same request as its seq, whenever an update is on offer.
// A client comparing against its own copy of the item could pair a fresh seq
// with a stale body, and accept text it never showed.
func TestTASK3462U3b_StateCarriesCurrentText(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Current 3462", "startup")
		plan := task3462ItemByTitle(t, srv, slug, "playbooks", "Plan a new initiative")

		// Nothing on offer: nothing to compare, so no current text.
		if st := task3462GetState(t, srv, slug, plan.Slug); st.Current != nil {
			t.Fatalf("current item carries current text: %+v", st.Current)
		}

		staged := stageOldSeed(t, srv, plan, "playbook/plan", "the old plan body", map[string]any{"trigger": "on-release"})
		edited := "the old plan body, edited just now"
		if _, err := srv.store.UpdateItem(staged.ID, models.ItemUpdate{Content: &edited}); err != nil {
			t.Fatal(err)
		}
		st := task3462GetState(t, srv, slug, plan.Slug)
		if st.State != collections.BuiltinDiverged {
			t.Fatalf("state %s, want diverged", st.State)
		}
		if st.Current == nil || st.Current.Content != edited {
			t.Fatalf("current text missing or stale: %+v", st.Current)
		}
		if st.Current.Fields["trigger"] != "on-release" {
			t.Fatalf("current fields missing the item's trigger: %+v", st.Current.Fields)
		}
		if _, has := st.Current.Fields["status"]; has {
			t.Fatal("current fields carry status, which an update never touches")
		}
		item, _ := srv.store.GetItem(plan.ID)
		if st.Seq != item.Seq {
			t.Fatalf("seq %d does not match the item's %d", st.Seq, item.Seq)
		}
	})
}
