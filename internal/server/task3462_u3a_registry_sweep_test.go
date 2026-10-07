package server

import (
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
)

func task3462ListBuiltins(t *testing.T, srv *Server, ws string) []builtinListEntry {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+ws+"/builtins", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET builtins: %d %s", rr.Code, rr.Body.String())
	}
	var out []builtinListEntry
	parseJSON(t, rr, &out)
	return out
}

// TASK-3462 U3a (night-43 review note): the registry's hash must equal what
// every seeding door STORES, or a workspace made today would offer an
// "update" to text it already holds. Swept over every door: each template's
// seeds, and an activation of every library entry, all read current at once.
func TestTASK3462U3a_EverySeedAndActivationReadsCurrent(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		for _, tmpl := range collections.ListTemplates() {
			slug, _ := task3462Workspace(t, srv, "Sweep "+tmpl.Name, tmpl.Name)
			entries := task3462ListBuiltins(t, srv, slug)
			if len(entries) == 0 {
				t.Errorf("template %s: seeded no built-in (every template seeds onboard)", tmpl.Name)
			}
			for _, e := range entries {
				if e.State != collections.BuiltinCurrent {
					t.Errorf("template %s: %s (%s) reads %s straight after seeding", tmpl.Name, e.Key, e.Title, e.State)
				}
			}
		}

		slug, _ := task3462Workspace(t, srv, "Sweep activations", "blank")
		seeded := map[string]bool{}
		for _, e := range task3462ListBuiltins(t, srv, slug) {
			seeded[e.Key] = true
		}
		var keys []string
		for _, c := range collections.ConventionLibrary() {
			for _, e := range c.Conventions {
				keys = append(keys, e.Key)
			}
		}
		for _, c := range collections.PlaybookLibrary() {
			for _, e := range c.Playbooks {
				keys = append(keys, e.Key)
			}
		}
		activated := 0
		for _, key := range keys {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/library/activate", map[string]string{"key": key})
			switch {
			case rr.Code == http.StatusCreated:
				activated++
			case rr.Code == http.StatusConflict && seeded[key]:
				// Already seeded with the same invocation slug: refused as
				// a duplicate, which is the slug's uniqueness, not a defect.
			default:
				t.Errorf("activate %s: %d %s", key, rr.Code, rr.Body.String())
			}
		}
		if activated == 0 {
			t.Fatal("no library entry activated")
		}
		for _, e := range task3462ListBuiltins(t, srv, slug) {
			if e.State != collections.BuiltinCurrent {
				t.Errorf("activated %s (%s) reads %s straight after activation", e.Key, e.Title, e.State)
			}
		}
	})
}
