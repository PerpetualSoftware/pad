package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3195: an ABANDONED child (a terminal value that closes without
// delivering) is left out of both a parent's done count and its total. Before,
// one done task and one cancelled task read 2/2, 100%.
//
// Two child collections, so both halves of the resolver are exercised:
//   - works declares abandoned_options ["cancelled"];
//   - fixes declares none, so the NegativeTerminals fallback makes "wontfix"
//     abandoned while "fixed" stays delivered.
//
// planMixed: done, cancelled, open (works) + fixed, wontfix (fixes)
//
//	→ total 3 (done, open, fixed), done 2 (done, fixed).
//
// planAbandoned: cancelled only → 0/0, which callers render as no progress.
//
// Every door that computes progress is driven through the router, for a
// member and for a guest: the guest takes the restricted, Go-side branches
// (handleGetItemProgress and collectionChildrenProgress), which are separate
// code from the SQL the member reads (CONVE-19).
func TestProgressLeavesAbandonedChildrenOut(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		ws, err := srv.store.GetWorkspaceBySlug(slug)
		if err != nil || ws == nil {
			t.Fatalf("GetWorkspaceBySlug: %v", err)
		}
		for _, c := range []map[string]any{
			{
				"name": "Works", "slug": "works", "prefix": "WRK",
				"schema": `{"fields":[{"key":"status","type":"select","options":["open","done","cancelled"],"terminal_options":["done","cancelled"],"abandoned_options":["cancelled"],"default":"open"}]}`,
			},
			{
				"name": "Fixes", "slug": "fixes", "prefix": "FIX",
				"schema": `{"fields":[{"key":"status","type":"select","options":["open","fixed","wontfix"],"terminal_options":["fixed","wontfix"],"default":"open"}]}`,
			},
		} {
			if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", c); rr.Code != http.StatusCreated {
				t.Fatalf("create collection %v: %d %s", c["slug"], rr.Code, rr.Body.String())
			}
		}

		planMixed := createItem(t, srv, slug, "plans", map[string]any{"title": "Mixed plan", "fields": `{"status":"active"}`})
		planAbandoned := createItem(t, srv, slug, "plans", map[string]any{"title": "Abandoned plan", "fields": `{"status":"active"}`})
		link := func(parent models.Item, coll, status string) models.Item {
			child := createItem(t, srv, slug, coll, map[string]any{"title": coll + " " + status, "fields": `{"status":"` + status + `"}`})
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+child.Slug+"/links", map[string]any{
				"target_id": parent.ID,
				"link_type": models.ItemLinkTypeParent,
			})
			if rr.Code != http.StatusCreated {
				t.Fatalf("link %s: %d %s", child.Title, rr.Code, rr.Body.String())
			}
			return child
		}
		link(planMixed, "works", "done")
		link(planMixed, "works", "cancelled")
		link(planMixed, "works", "open")
		link(planMixed, "fixes", "fixed")
		link(planMixed, "fixes", "wontfix")
		link(planAbandoned, "works", "cancelled")

		want := map[string][2]int{planMixed.ID: {3, 2}, planAbandoned.ID: {0, 0}}

		// A member (owner), and a guest whose collection grants cover
		// everything, so the guest's numbers must match the member's while the
		// code path differs.
		granter, err := srv.store.CreateUser(models.UserCreate{Email: "granter-3195@example.com", Name: "Granter", Username: "granter-3195", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser granter: %v", err)
		}
		guest, err := srv.store.CreateUser(models.UserCreate{Email: "guest-3195@example.com", Name: "Guest", Username: "guest-3195", Password: "pw-test-12345"})
		if err != nil {
			t.Fatalf("CreateUser guest: %v", err)
		}
		for _, cs := range []string{"plans", "works", "fixes"} {
			coll, err := srv.store.GetCollectionBySlug(ws.ID, cs)
			if err != nil || coll == nil {
				t.Fatalf("GetCollectionBySlug %s: %v", cs, err)
			}
			if _, err := srv.store.CreateCollectionGrant(ws.ID, coll.ID, guest.ID, "view", granter.ID); err != nil {
				t.Fatalf("CreateCollectionGrant %s: %v", cs, err)
			}
		}
		if err := srv.store.AddWorkspaceMember(ws.ID, granter.ID, "owner"); err != nil {
			t.Fatalf("AddWorkspaceMember: %v", err)
		}
		tokens := map[string]string{}
		for caller, id := range map[string]string{"member": granter.ID, "guest": guest.ID} {
			tok, err := srv.store.CreateSession(id, "go-test", "192.0.2.1", "go-test", 24*time.Hour)
			if err != nil {
				t.Fatalf("CreateSession %s: %v", caller, err)
			}
			tokens[caller] = tok
		}

		get := func(caller, path string) *httptest.ResponseRecorder {
			return doRequestWithCookie(srv, "GET", path, nil, tokens[caller])
		}

		for _, caller := range []string{"member", "guest"} {
			for _, p := range []models.Item{planMixed, planAbandoned} {
				rr := get(caller, "/api/v1/workspaces/"+slug+"/items/"+p.Slug+"/progress")
				if rr.Code != http.StatusOK {
					t.Fatalf("%s /progress %s: %d %s", caller, p.Title, rr.Code, rr.Body.String())
				}
				var got struct {
					Total, Done, Percentage int
				}
				parseJSON(t, rr, &got)
				w := want[p.ID]
				wantPct := 0
				if w[0] > 0 {
					wantPct = w[1] * 100 / w[0]
				}
				if got.Total != w[0] || got.Done != w[1] || got.Percentage != wantPct {
					t.Errorf("%s /progress %s = %d/%d %d%%, want %d/%d %d%%", caller, p.Title, got.Done, got.Total, got.Percentage, w[1], w[0], wantPct)
				}
			}

			rr := get(caller, "/api/v1/workspaces/"+slug+"/collections/plans/child-progress")
			if rr.Code != http.StatusOK {
				t.Fatalf("%s child-progress: %d %s", caller, rr.Code, rr.Body.String())
			}
			var rows []struct {
				ItemID string `json:"item_id"`
				Total  int    `json:"total"`
				Done   int    `json:"done"`
			}
			parseJSON(t, rr, &rows)
			seen := 0
			for _, row := range rows {
				w, ok := want[row.ItemID]
				if !ok {
					continue
				}
				seen++
				if row.Total != w[0] || row.Done != w[1] {
					t.Errorf("%s child-progress row %s = %d/%d, want %d/%d", caller, row.ItemID, row.Done, row.Total, w[1], w[0])
				}
			}
			if seen != len(want) {
				t.Errorf("%s child-progress returned %d of the %d fixture parents", caller, seen, len(want))
			}

			rr = get(caller, "/api/v1/workspaces/"+slug+"/dashboard")
			if rr.Code != http.StatusOK {
				t.Fatalf("%s dashboard: %d %s", caller, rr.Code, rr.Body.String())
			}
			var dash DashboardResponse
			parseJSON(t, rr, &dash)
			found := map[string]bool{}
			for _, dp := range dash.ActivePlans {
				switch dp.Slug {
				case planMixed.Slug:
					found[dp.Slug] = true
					if dp.TaskCount != 3 || dp.DoneCount != 2 || dp.Progress != 66 {
						t.Errorf("%s dashboard %s = %d/%d %d%%, want 2/3 66%%", caller, dp.Slug, dp.DoneCount, dp.TaskCount, dp.Progress)
					}
				case planAbandoned.Slug:
					found[dp.Slug] = true
					// Nothing counted: the plan's own progress field decides,
					// which is what a plan with no children gets.
					if dp.TaskCount != 0 || dp.DoneCount != 0 {
						t.Errorf("%s dashboard %s = %d/%d, want 0/0", caller, dp.Slug, dp.DoneCount, dp.TaskCount)
					}
				}
			}
			if len(found) != 2 {
				t.Errorf("%s dashboard listed %d of the 2 fixture plans", caller, len(found))
			}
		}
	})
}
