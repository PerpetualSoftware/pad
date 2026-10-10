package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// PLAN-3535 PR 2, the behavioural guard: the items of a REFERENCE collection
// count toward no open-work or progress surface, and still count on the
// surfaces that keep totals.
//
// One fixture, measured in three phases over the same items:
//
//	W: the Notes collection tracks work (as every collection did before);
//	R: Notes is reference;
//	D: Notes is reference and its items are deleted.
//
// Every EXCLUDE surface must read the same in R as in D (the reference items
// changed nothing) and differently in W (so the fixture really reaches that
// surface: a check that cannot tell W from D proves nothing). Every KEEP
// surface must read the same in R as in W, and differently in D.
//
// A surface added to the exclude list later is one entry in measure().
func TestPLAN3535_ReferenceItemsCountTowardNoOpenWork(t *testing.T) {
	t.Parallel()
	srv := testServer(t)
	ws := createWSForTest(t, srv)
	base := "/api/v1/workspaces/" + ws

	rr := doRequest(srv, "POST", base+"/collections", map[string]any{
		"name":        "Notes",
		"tracks_work": true,
		"schema": `{"fields":[` +
			`{"key":"status","type":"select","options":["open","in-progress","done"],"terminal_options":["done"],"default":"open"},` +
			`{"key":"priority","type":"select","options":["low","medium","high","critical"]},` +
			`{"key":"due_date","type":"date"}]}`,
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create notes: %d %s", rr.Code, rr.Body.String())
	}

	plan := createItem(t, srv, ws, "plans", map[string]any{"title": "The plan", "fields": map[string]any{"status": "active"}})
	createItem(t, srv, ws, "tasks", map[string]any{"title": "Shipped task", "fields": map[string]any{"status": "done", "parent": plan.Ref}})
	blocker := createItem(t, srv, ws, "tasks", map[string]any{"title": "Blocker", "fields": map[string]any{"status": "open"}})
	// N1 is open work in every way a dashboard notices: a child of the active
	// plan, in progress, high priority, overdue, blocked, and stalled.
	n1 := createItem(t, srv, ws, "notes", map[string]any{"title": "Open note", "fields": map[string]any{
		"status": "in-progress", "priority": "high", "due_date": "2020-01-01", "parent": plan.Ref,
	}})
	// N2 is finished: completed work for standup and changelog, done progress.
	n2 := createItem(t, srv, ws, "notes", map[string]any{"title": "Done note", "fields": map[string]any{"status": "done", "parent": plan.Ref}})
	// N3 is unblocked work in progress, which suggested_next would offer.
	n3 := createItem(t, srv, ws, "notes", map[string]any{"title": "Wip note", "fields": map[string]any{"status": "in-progress", "parent": plan.Ref}})
	createBlocksLink(t, srv, ws, blocker.Slug, n1.ID)

	// The close-guard probe closes and reopens a parent, and a close is a
	// completion the report counts. So the probe's parent lives in its own
	// reference collection, which a default report leaves out, and its only
	// child is N4, an open note.
	if rr := doRequest(srv, "POST", base+"/collections", map[string]any{
		"name": "Specs", "tracks_work": false,
		"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"],"default":"open"}]}`,
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create specs: %d %s", rr.Code, rr.Body.String())
	}
	spec := createItem(t, srv, ws, "specs", map[string]any{"title": "Guard parent", "fields": map[string]any{"status": "open"}})
	n4 := createItem(t, srv, ws, "notes", map[string]any{"title": "Guarding note", "fields": map[string]any{"status": "open", "parent": spec.Ref}})
	stale := time.Now().UTC().Add(-10 * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
	if _, err := srv.store.DB().Exec(`UPDATE items SET updated_at = ? WHERE id = ?`, stale, n1.ID); err != nil {
		t.Fatal(err)
	}

	setNotes := func(work bool) {
		t.Helper()
		if rr := doRequest(srv, "PATCH", base+"/collections/notes", map[string]any{"tracks_work": work}); rr.Code != http.StatusOK {
			t.Fatalf("set tracks_work=%v: %d %s", work, rr.Code, rr.Body.String())
		}
	}

	type surface struct {
		name    string
		exclude bool // false: a KEEP surface
		read    func(t *testing.T) string
	}
	get := func(t *testing.T, path string) []byte {
		t.Helper()
		rr := doRequest(srv, "GET", base+path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", path, rr.Code, rr.Body.String())
		}
		return rr.Body.Bytes()
	}
	// One dashboard read per phase: the sections below all read it, and a
	// read per section trips the API rate limit.
	var phaseDash *DashboardResponse
	dash := func(t *testing.T) DashboardResponse {
		if phaseDash == nil {
			d := getDashboard(t, srv, ws)
			phaseDash = &d
		}
		return *phaseDash
	}
	refs := func(xs []string) string { sort.Strings(xs); return strings.Join(xs, ",") }

	surfaces := []surface{
		{"item progress", true, func(t *testing.T) string { return string(get(t, "/items/"+plan.Slug+"/progress")) }},
		{"plans-progress", true, func(t *testing.T) string { return string(get(t, "/plans-progress")) }},
		{"child-progress", true, func(t *testing.T) string { return string(get(t, "/collections/plans/child-progress")) }},
		{"dashboard active_plans", true, func(t *testing.T) string {
			var out []string
			for _, p := range dash(t).ActivePlans {
				out = append(out, fmt.Sprintf("%s %d/%d %d%%", p.Slug, p.DoneCount, p.TaskCount, p.Progress))
			}
			return refs(out)
		}},
		{"dashboard active_items", true, func(t *testing.T) string {
			var out []string
			for _, a := range dash(t).ActiveItems {
				out = append(out, a.Slug)
			}
			return refs(out)
		}},
		{"dashboard attention", true, func(t *testing.T) string {
			var out []string
			for _, a := range dash(t).Attention {
				out = append(out, a.Type+":"+a.ItemSlug)
			}
			return refs(out)
		}},
		{"dashboard suggested_next", true, func(t *testing.T) string {
			var out []string
			for _, s := range dash(t).SuggestedNext {
				out = append(out, s.ItemSlug)
			}
			return refs(out)
		}},
		{"dashboard by_role", true, func(t *testing.T) string {
			var out []string
			for _, r := range dash(t).ByRole {
				out = append(out, fmt.Sprintf("%s=%d", r.RoleSlug, r.ItemCount))
			}
			return refs(out)
		}},
		{"next", true, func(t *testing.T) string { return string(get(t, "/next")) }},
		{"standup", true, func(t *testing.T) string {
			var s StandupResponse
			if err := json.Unmarshal(get(t, "/standup?days=30"), &s); err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, i := range s.Completed {
				out = append(out, "done:"+i.Ref)
			}
			for _, i := range s.InProgress {
				out = append(out, "wip:"+i.Ref)
			}
			for _, i := range s.SuggestedNext {
				out = append(out, "next:"+i.Ref)
			}
			return refs(out)
		}},
		{"changelog", true, func(t *testing.T) string {
			var c ChangelogResponse
			if err := json.Unmarshal(get(t, "/changelog?days=30"), &c); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%d %v", c.Total, c.Groups)
		}},
		{"report", true, func(t *testing.T) string {
			var r struct {
				Collections []string `json:"collections"`
				Totals      struct {
					Created   int `json:"created"`
					Completed int `json:"completed"`
				} `json:"totals"`
				WIP struct {
					OpenCount int `json:"open_count"`
				} `json:"wip"`
			}
			if err := json.Unmarshal(get(t, "/report?window=month"), &r); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf("%v created=%d completed=%d wip=%d", r.Collections, r.Totals.Created, r.Totals.Completed, r.WIP.OpenCount)
		}},
		{"close guard", true, func(t *testing.T) string {
			rr := doRequest(srv, "PATCH", base+"/items/"+spec.Slug, map[string]any{"fields_patch": map[string]any{"status": "done"}})
			if rr.Code == http.StatusOK {
				back := doRequest(srv, "PATCH", base+"/items/"+spec.Slug, map[string]any{"fields_patch": map[string]any{"status": "open"}})
				if back.Code != http.StatusOK {
					t.Fatalf("reopen the plan: %d %s", back.Code, back.Body.String())
				}
			}
			return fmt.Sprint(rr.Code)
		}},

		// KEEP: totals still count every item.
		{"sidebar item_count", false, func(t *testing.T) string {
			var colls []models.Collection
			if err := json.Unmarshal(get(t, "/collections"), &colls); err != nil {
				t.Fatal(err)
			}
			for _, c := range colls {
				if c.Slug == "notes" {
					return fmt.Sprintf("%d/%d", c.ActiveItemCount, c.ItemCount)
				}
			}
			t.Fatal("no notes collection")
			return ""
		}},
		{"dashboard summary", false, func(t *testing.T) string {
			d := dash(t)
			return fmt.Sprintf("%d %v", d.Summary.TotalItems, d.Summary.ByCollection["notes"])
		}},
		{"item list (open)", false, func(t *testing.T) string {
			var items []models.Item
			if err := json.Unmarshal(get(t, "/collections/notes/items"), &items); err != nil {
				t.Fatal(err)
			}
			return fmt.Sprint(len(items))
		}},
	}

	measure := func() map[string]string {
		phaseDash = nil
		out := make(map[string]string, len(surfaces))
		for _, s := range surfaces {
			out[s.name] = s.read(t)
		}
		return out
	}

	w := measure()
	setNotes(false)
	r := measure()
	for _, it := range []models.Item{n1, n2, n3, n4} {
		if rr := doRequest(srv, "DELETE", base+"/items/"+it.Slug, nil); rr.Code != http.StatusOK && rr.Code != http.StatusNoContent {
			t.Fatalf("delete %s: %d %s", it.Slug, rr.Code, rr.Body.String())
		}
	}
	d := measure()

	for _, s := range surfaces {
		if s.exclude {
			if r[s.name] != d[s.name] {
				t.Errorf("%s: the reference items still count.\n  reference: %s\n  deleted:   %s", s.name, r[s.name], d[s.name])
			}
			if w[s.name] == d[s.name] {
				t.Errorf("%s: the fixture does not reach this surface (as work it reads the same as with the items deleted: %s)", s.name, w[s.name])
			}
			continue
		}
		if r[s.name] != w[s.name] {
			t.Errorf("%s (keep): reference changed it.\n  work:      %s\n  reference: %s", s.name, w[s.name], r[s.name])
		}
		if r[s.name] == d[s.name] {
			t.Errorf("%s (keep): the fixture does not reach this surface (%s)", s.name, r[s.name])
		}
	}
}
