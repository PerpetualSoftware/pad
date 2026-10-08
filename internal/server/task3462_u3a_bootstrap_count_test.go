package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// task3462BootstrapUpdates reads the bootstrap's builtin_updates, and whether
// the key was present at all (it is omitempty: absent means none).
func task3462BootstrapUpdates(t *testing.T, srv *Server, ws string) (int, bool) {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/agent/bootstrap", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("bootstrap: %d %s", rr.Code, rr.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	v, ok := raw["builtin_updates"]
	if !ok {
		return 0, false
	}
	var n int
	if err := json.Unmarshal(v, &n); err != nil {
		t.Fatalf("builtin_updates %s: %v", v, err)
	}
	return n, true
}

// TASK-3462 U3a: the bootstrap counts the items an update is on offer for
// (update_available and diverged), so an agent can mention it once. Dave's
// ruling: a nudge on the library page and item pane, plus this count; no
// dashboard entry, and nothing ever updates on its own.
func TestTASK3462U3a_BootstrapCountsBuiltinUpdates(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Count 3462", "startup")

		// Fresh from the template: every built-in is current, and the key is
		// absent (omitempty), so a workspace with nothing to offer is
		// byte-identical to before.
		if n, present := task3462BootstrapUpdates(t, srv, slug); present {
			t.Fatalf("fresh workspace: builtin_updates present (%d), want absent", n)
		}

		// An unedited item whose library text changed: update_available.
		ship := task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks")
		stageOldSeed(t, srv, ship, "playbook/ship", "the old ship body", nil)
		if st := task3462GetState(t, srv, slug, ship.Slug); st.State != collections.BuiltinUpdateAvailable {
			t.Fatalf("ship: state %s, want update_available", st.State)
		}
		if n, _ := task3462BootstrapUpdates(t, srv, slug); n != 1 {
			t.Fatalf("one update available: builtin_updates %d, want 1", n)
		}

		// An edited one whose library text also changed: diverged, which is
		// on offer too.
		plan := task3462ItemByTitle(t, srv, slug, "playbooks", "Plan a new initiative")
		staged := stageOldSeed(t, srv, plan, "playbook/plan", "the old plan body", nil)
		edited := "the old plan body, edited by the user"
		if _, err := srv.store.UpdateItem(staged.ID, models.ItemUpdate{Content: &edited}); err != nil {
			t.Fatal(err)
		}
		if st := task3462GetState(t, srv, slug, plan.Slug); st.State != collections.BuiltinDiverged {
			t.Fatalf("plan: state %s, want diverged", st.State)
		}
		if n, _ := task3462BootstrapUpdates(t, srv, slug); n != 2 {
			t.Fatalf("one available + one diverged: builtin_updates %d, want 2", n)
		}
	})
}

// TASK-3462 U3a (night-43 review note): version history keeps BODIES only, so
// an accepted update that replaces an edited trigger, scope or arguments left
// no record of the values it replaced. The update's change summary now names
// each field whose value it changed, with the value it replaced.
func TestTASK3462U3a_UpdateRecordsReplacedFields(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Replaced 3462", "startup")
		plan := task3462ItemByTitle(t, srv, slug, "playbooks", "Plan a new initiative")
		// Seeded with an older trigger the library has since changed, and the
		// body edited too: diverged.
		old := stageOldSeed(t, srv, plan, "playbook/plan", "the old plan body", map[string]any{"trigger": "on-release"})
		edited := "the old plan body, edited"
		if _, err := srv.store.UpdateItem(old.ID, models.ItemUpdate{Content: &edited}); err != nil {
			t.Fatal(err)
		}
		st := task3462GetState(t, srv, slug, plan.Slug)
		if st.State != collections.BuiltinDiverged {
			t.Fatalf("state %s, want diverged", st.State)
		}
		if r := task3462Update(srv, slug, plan.Slug, map[string]any{"expected_seq": st.Seq}); r.code != http.StatusOK {
			t.Fatalf("update: %d %s", r.code, r.body)
		}
		current, _ := srv.store.GetItem(plan.ID)
		versions, err := srv.store.ListItemVersionsResolved(plan.ID, current.Content)
		if err != nil {
			t.Fatal(err)
		}
		var summary string
		for _, v := range versions {
			if strings.HasPrefix(v.ChangeSummary, "Updated from Pad's built-in") {
				summary = v.ChangeSummary
				break
			}
		}
		if summary == "" {
			t.Fatal("no version row carries the update's change summary")
		}
		if !strings.Contains(summary, "trigger") || !strings.Contains(summary, "on-release") {
			t.Fatalf("the change summary does not record the replaced trigger value: %q", summary)
		}
		// A field the update did not change is not listed: the record is of
		// what was replaced, not of every field the library writes.
		if strings.Contains(summary, "invocation_slug") {
			t.Fatalf("the change summary lists a field whose value did not change: %q", summary)
		}
	})
}
