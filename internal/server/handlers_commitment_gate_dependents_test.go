package server

import (
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/google/uuid"
)

// The gate view of an item carries its dependents: the gates it blocks, so the
// items its success releases (TASK-4611). Only live dependents the reader can
// read appear.

// dependentFix is item A (f.item, with a commitment) blocking item B.
func newDependentFix(t *testing.T) (*graphHTTP, *models.ScheduleGate) {
	t.Helper()
	f := newGraphHTTP(t, models.CommitmentRolesInput{}, models.CommitmentRolesInput{})
	g, err := f.srv.store.CreateScheduleGate(f.ws.ID, f.items[1].ID, f.items[0].ID, f.access)
	if err != nil {
		t.Fatal(err)
	}
	return f, g
}

// gateViews reads the gate view of A from GET A/gates and GET A/commitment.
func (f *graphHTTP) gateViews(tok string) map[string]map[string]any {
	f.t.Helper()
	out := map[string]map[string]any{}
	rr := f.as(tok, "GET", f.path(f.items[0])+"/gates", nil)
	if rr.Code != 200 {
		f.t.Fatalf("GET gates: %d %s", rr.Code, rr.Body.String())
	}
	out["gates"] = schedMap(f.t, rr)
	out["gates"]["_raw"] = rr.Body.String()
	rr = f.as(tok, "GET", f.path(f.items[0]), nil)
	if rr.Code != 200 {
		f.t.Fatalf("GET commitment: %d %s", rr.Code, rr.Body.String())
	}
	view, _ := schedMap(f.t, rr)["gates"].(map[string]any)
	if view == nil {
		f.t.Fatalf("GET commitment has no gate view: %s", rr.Body.String())
	}
	view["_raw"] = rr.Body.String()
	out["commitment"] = view
	return out
}

func expectNoDependents(t *testing.T, what string, views map[string]map[string]any, ids ...string) {
	t.Helper()
	for name, v := range views {
		if _, ok := v["dependents"]; ok {
			t.Fatalf("%s: %s has a dependents key: %s", what, name, v["_raw"])
		}
		for _, id := range ids {
			if strings.Contains(v["_raw"].(string), id) {
				t.Fatalf("%s: %s names %s: %s", what, name, id, v["_raw"])
			}
		}
	}
}

func TestOwnerSeesADependentOnEveryGateView(t *testing.T) {
	f, g := newDependentFix(t)
	for name, v := range f.gateViews(f.token(f.owner, "")) {
		deps, _ := v["dependents"].([]any)
		if len(deps) != 1 {
			t.Fatalf("%s: dependents=%v: %s", name, v["dependents"], v["_raw"])
		}
		d := deps[0].(map[string]any)
		if d["id"] != g.ID || d["blocked_item_id"] != f.items[1].ID || d["blocker_item_id"] != f.items[0].ID || d["outcome"] != models.GateOutcomePending {
			t.Fatalf("%s: dependent %v", name, d)
		}
		// State still describes only the gates holding A, which has none.
		if v["state"] != models.GateStateReleased {
			t.Fatalf("%s: state=%v, want released", name, v["state"])
		}
		if gates, _ := v["gates"].([]any); len(gates) != 0 {
			t.Fatalf("%s: gates=%v, want none", name, gates)
		}
	}
}

func TestAMemberDoesNotSeeADependentInAHiddenCollection(t *testing.T) {
	f, g := newDependentFix(t)
	hidden := mustCollection(t, f.srv, f.ws.ID, "Hidden")
	if _, err := f.srv.store.DB().Exec(`UPDATE items SET collection_id=? WHERE id=?`, hidden.ID, f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	u := f.member("dependent-member@example.com", "editor")
	if err := f.srv.store.SetMemberCollectionAccess(f.ws.ID, u.ID, "specific", []string{f.coll.ID}); err != nil {
		t.Fatal(err)
	}
	expectNoDependents(t, "hidden collection", f.gateViews(f.token(u, "")), f.items[1].ID, g.ID)
}

func TestASoftDeletedDependentIsOmitted(t *testing.T) {
	f, g := newDependentFix(t)
	if _, err := f.srv.store.DB().Exec(`UPDATE items SET deleted_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), f.items[1].ID); err != nil {
		t.Fatal(err)
	}
	expectNoDependents(t, "soft-deleted", f.gateViews(f.token(f.owner, "")), f.items[1].ID, g.ID)
}

func TestADependentInAnotherWorkspaceIsOmitted(t *testing.T) {
	f := newGraphHTTP(t, models.CommitmentRolesInput{})
	ws := mustWorkspace(t, f.srv, "Other", f.owner.ID)
	c := mustCollection(t, f.srv, ws.ID, "Other tasks")
	foreign := mustItem(t, f.srv, ws.ID, c.ID, "Foreign")
	id := uuid.NewString()
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(`INSERT INTO schedule_gates (id,workspace_id,blocked_item_id,blocker_item_id,created_at) VALUES (?,?,?,?,?)`),
		id, f.ws.ID, foreign.ID, f.items[0].ID, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	expectNoDependents(t, "other workspace", f.gateViews(f.token(f.owner, "")), foreign.ID, id)
}
