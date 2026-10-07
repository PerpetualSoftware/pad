package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/collections"
	"github.com/PerpetualSoftware/pad/internal/models"
)

// TASK-3462 U1: every door that brings a built-in convention or playbook into
// a workspace records which one it is and which version of its text, and the
// record follows the item through its lifecycle as the lead's bar names it:
// soft delete and restore keep it, a move keeps it, a copy (user-made) gets
// none, export/import round-trips it, and a request body cannot claim one.

func task3462Workspace(t *testing.T, srv *Server, name, template string) (slug, wsID string) {
	t.Helper()
	rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": name, "template": template})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create workspace %s: %d %s", name, rr.Code, rr.Body.String())
	}
	var ws models.Workspace
	parseJSON(t, rr, &ws)
	return ws.Slug, ws.ID
}

func task3462ItemByTitle(t *testing.T, srv *Server, wsSlug, coll, title string) models.Item {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/collections/"+coll+"/items", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list %s: %d %s", coll, rr.Code, rr.Body.String())
	}
	var items []models.Item
	parseJSON(t, rr, &items)
	for _, it := range items {
		if it.Title == title {
			return it
		}
	}
	t.Fatalf("%s has no item titled %q", coll, title)
	return models.Item{}
}

func task3462ItemBySlug(t *testing.T, srv *Server, wsSlug, itemSlug string) models.Item {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+wsSlug+"/items/"+itemSlug, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("get %s: %d %s", itemSlug, rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	return it
}

func task3462Origin(t *testing.T, srv *Server, itemID string) *models.BuiltinOrigin {
	t.Helper()
	o, err := srv.store.GetItemBuiltinOrigin(itemID)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// assertCarriesEntry checks the item records key at the entry's current hash
// AND still holds that entry's text, so the recorded hash is true of it.
func assertCarriesEntry(t *testing.T, srv *Server, it models.Item, key string) {
	t.Helper()
	e, ok := collections.LookupBuiltin(key)
	if !ok {
		t.Fatalf("%s is not registered", key)
	}
	o := task3462Origin(t, srv, it.ID)
	if o == nil || o.Key != key || o.SeedHash != e.Hash() {
		t.Fatalf("%s: origin %+v, want key %s hash %s", it.Title, o, key, e.Hash())
	}
	h, err := e.ItemStateHash(it.Content, it.Fields)
	if err != nil {
		t.Fatal(err)
	}
	if h != e.Hash() {
		t.Fatalf("%s: the stored item does not hold the text its origin records (fields %s)", it.Title, it.Fields)
	}
}

// Template seeding records the origin of every seeded convention and
// playbook, and none for a sample item.
func TestTASK3462_TemplateSeedRecordsOrigin(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Seeded 3462", "startup")
		assertCarriesEntry(t, srv, task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks"), "playbook/ship")
		assertCarriesEntry(t, srv, task3462ItemByTitle(t, srv, slug, "playbooks", "Onboard a workspace"), "playbook/onboard")
		assertCarriesEntry(t, srv, task3462ItemByTitle(t, srv, slug, "conventions", "Conventional commit format"), "convention/conventional-commit-format")

		origins, err := srv.store.WorkspaceBuiltinOrigins(wsID)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := collections.GetTemplate("startup")
		if want := len(tmpl.Conventions) + len(tmpl.Playbooks) + 1; len(origins) != want { // + onboard
			t.Errorf("%d origins, want one per seeded convention and playbook (%d)", len(origins), want)
		}

		hslug, _ := task3462Workspace(t, srv, "Hiring 3462", "hiring")
		assertCarriesEntry(t, srv, task3462ItemByTitle(t, srv, hslug, "playbooks", "Advance a Candidate"), "hiring/playbook/advance-a-candidate")
		sample := task3462ItemByTitle(t, srv, hslug, "requisitions", "Example Requisition: Senior Backend Engineer")
		if o := task3462Origin(t, srv, sample.ID); o != nil {
			t.Errorf("a sample item records origin %+v", o)
		}
	})
}

// The activate door records the origin, by key or by title, and stores what
// a template seed of the same entry stores.
func TestTASK3462_ActivateRecordsOriginAndMatchesTheSeed(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Blank 3462", "blank")
		for _, body := range []map[string]string{
			{"key": "convention/conventional-commit-format"},
			{"title": "Plan a new initiative"},
		} {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/library/activate", body)
			if rr.Code != http.StatusCreated {
				t.Fatalf("activate %v: %d %s", body, rr.Code, rr.Body.String())
			}
		}
		activated := task3462ItemByTitle(t, srv, slug, "conventions", "Conventional commit format")
		assertCarriesEntry(t, srv, activated, "convention/conventional-commit-format")
		assertCarriesEntry(t, srv, task3462ItemByTitle(t, srv, slug, "playbooks", "Plan a new initiative"), "playbook/plan")

		// Seeded and activated copies of one entry store the same fields.
		sslug, _ := task3462Workspace(t, srv, "Seeded twin 3462", "startup")
		seeded := task3462ItemByTitle(t, srv, sslug, "conventions", "Conventional commit format")
		var a, b map[string]any
		_ = json.Unmarshal([]byte(activated.Fields), &a)
		_ = json.Unmarshal([]byte(seeded.Fields), &b)
		aj, _ := json.Marshal(a)
		bj, _ := json.Marshal(b)
		if string(aj) != string(bj) {
			t.Errorf("activated and seeded copies differ:\n activated %s\n seeded    %s", aj, bj)
		}

		for _, bad := range []map[string]string{
			{},
			{"key": "playbook/ship", "title": "Ship tasks"},
		} {
			if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/library/activate", bad); rr.Code != http.StatusBadRequest {
				t.Errorf("activate %v: %d, want 400", bad, rr.Code)
			}
		}
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/library/activate", map[string]string{"title": "No such entry"})
		if rr.Code != http.StatusNotFound {
			t.Errorf("unknown title: %d, want 404", rr.Code)
		}
	})
}

// A request body cannot claim an origin for its own text.
func TestTASK3462_CreateBodyCannotClaimOrigin(t *testing.T) {
	srv := testServer(t)
	slug, _ := task3462Workspace(t, srv, "Claim 3462", "blank")
	rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/playbooks/items", map[string]any{
		"title":          "My playbook",
		"content":        "mine",
		"fields":         `{"status":"active","trigger":"manual","scope":"all"}`,
		"builtin_origin": map[string]string{"key": "playbook/ship"},
		"BuiltinOrigin":  map[string]string{"Key": "playbook/ship"},
	})
	if rr.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var it models.Item
	parseJSON(t, rr, &it)
	if o := task3462Origin(t, srv, it.ID); o != nil {
		t.Fatalf("a request body claimed origin %+v", o)
	}
}

// The lifecycle the lead's bar names.
func TestTASK3462_OriginThroughTheItemLifecycle(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Life 3462", "startup")
		ship := task3462ItemByTitle(t, srv, slug, "playbooks", "Ship tasks")
		want := task3462Origin(t, srv, ship.ID)
		if want == nil {
			t.Fatal("fixture: the seeded ship playbook has no origin")
		}

		// Soft delete keeps it (and the workspace listing stops naming the
		// item); restore brings both back.
		if rr := doRequest(srv, "DELETE", "/api/v1/workspaces/"+slug+"/items/"+ship.Slug, nil); rr.Code != http.StatusNoContent && rr.Code != http.StatusOK {
			t.Fatalf("delete: %d %s", rr.Code, rr.Body.String())
		}
		if o := task3462Origin(t, srv, ship.ID); o == nil || *o != *want {
			t.Errorf("after soft delete: %+v, want %+v", o, want)
		}
		listed, _ := srv.store.WorkspaceBuiltinOrigins(ship.WorkspaceID)
		if _, ok := listed[ship.ID]; ok {
			t.Error("a soft-deleted item is still listed")
		}
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+ship.Slug+"/restore", nil); rr.Code != http.StatusOK {
			t.Fatalf("restore: %d %s", rr.Code, rr.Body.String())
		}
		if o := task3462Origin(t, srv, ship.ID); o == nil || *o != *want {
			t.Errorf("after restore: %+v, want %+v", o, want)
		}

		// A copy is a new, user-made item: within the workspace and across.
		other, _ := task3462Workspace(t, srv, "Other 3462", "startup")
		for _, dst := range []string{slug, other} {
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+ship.Slug+"/copy", map[string]any{
				"target_workspace":  dst,
				"target_collection": "playbooks",
				"field_overrides":   map[string]any{"invocation_slug": "ship-copy-" + dst},
			})
			if rr.Code != http.StatusCreated && rr.Code != http.StatusOK {
				t.Fatalf("copy to %s: %d %s", dst, rr.Code, rr.Body.String())
			}
			var res struct {
				Item *models.Item `json:"item"`
			}
			parseJSON(t, rr, &res)
			if res.Item == nil {
				t.Fatalf("copy to %s: no item in %s", dst, rr.Body.String())
			}
			id := res.Item.ID
			if id == "" || id == ship.ID {
				t.Fatalf("copy to %s: no new item id in %s", dst, rr.Body.String())
			}
			if o := task3462Origin(t, srv, id); o != nil {
				t.Errorf("a copy to %s carries origin %+v", dst, o)
			}
		}

		// A collection move keeps the item, so it keeps the origin.
		conv := task3462ItemByTitle(t, srv, slug, "conventions", "Conventional commit format")
		convOrigin := task3462Origin(t, srv, conv.ID)
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/"+conv.Slug+"/move", map[string]any{
			"target_collection": "docs",
		}); rr.Code != http.StatusOK {
			t.Fatalf("move: %d %s", rr.Code, rr.Body.String())
		}
		if o := task3462Origin(t, srv, conv.ID); o == nil || *o != *convOrigin {
			t.Errorf("after a move: %+v, want %+v", o, convOrigin)
		}

		// Export and import round-trip it onto the imported item.
		rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/export", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("export: %d", rr.Code)
		}
		var bundle models.WorkspaceExport
		parseJSON(t, rr, &bundle)
		bundle.Workspace.Name = "Imported 3462"
		b, _ := json.Marshal(bundle)
		rr = rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(b))
		if rr.Code != http.StatusCreated {
			t.Fatalf("import: %d %s", rr.Code, rr.Body.String())
		}
		var imported models.Workspace
		parseJSON(t, rr, &imported)
		// By slug: the copy above is also titled "Ship tasks" and, being
		// user-made, rightly has no origin.
		got := task3462Origin(t, srv, task3462ItemBySlug(t, srv, imported.Slug, ship.Slug).ID)
		if got == nil || *got != *want {
			t.Errorf("after export/import: %+v, want %+v", got, want)
		}

		// A malformed origin in a bundle is skipped, not fatal.
		for i := range bundle.Items {
			if bundle.Items[i].BuiltinOrigin != nil {
				bundle.Items[i].BuiltinOrigin = &models.BuiltinOrigin{Key: "Not A Key", SeedHash: "zz"}
			}
		}
		bundle.Workspace.Name = "Imported bad 3462"
		b, _ = json.Marshal(bundle)
		rr = rawJSONRequest(srv, "POST", "/api/v1/workspaces/import", string(b))
		if rr.Code != http.StatusCreated {
			t.Fatalf("import with malformed origins: %d %s", rr.Code, rr.Body.String())
		}
		parseJSON(t, rr, &imported)
		if o := task3462Origin(t, srv, task3462ItemBySlug(t, srv, imported.Slug, ship.Slug).ID); o != nil {
			t.Errorf("a malformed origin was imported: %+v", o)
		}
	})
}

// The activate door lands an entry in the collection that DECLARES its
// artifact kind, whatever that collection is called (BUG-2702, whose MCP-side
// coverage moved here when the server took the resolution over), and only
// falls back to the canonical slug when nothing declares it.
func TestTASK3462_ActivateFollowsTheDeclaredKind(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, wsID := task3462Workspace(t, srv, "Renamed 3462", "blank")
		conv, err := srv.store.GetCollectionBySlug(wsID, "conventions")
		if err != nil || conv == nil {
			t.Fatalf("conventions: %v", err)
		}
		if _, err := srv.store.DB().Exec(srv.store.D().Rebind(`UPDATE collections SET slug = 'house-rules' WHERE id = ?`), conv.ID); err != nil {
			t.Fatal(err)
		}
		rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/library/activate", map[string]string{"title": "Conventional commit format"})
		if rr.Code != http.StatusCreated {
			t.Fatalf("activate into a renamed collection: %d %s", rr.Code, rr.Body.String())
		}
		var it models.Item
		parseJSON(t, rr, &it)
		if it.CollectionID != conv.ID {
			t.Fatalf("landed in collection %s, want the declaring one %s", it.CollectionID, conv.ID)
		}
	})
}
