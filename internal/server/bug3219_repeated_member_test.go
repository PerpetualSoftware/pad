package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// BUG-3219. BUG-3202 moved fields_patch and field_overrides from a plain map
// decode, which MERGED a repeated member, to a raw decode, where the LAST
// occurrence wins. A request repeating one of them answered 200 having dropped
// the first. Each such request is now refused with a 400 naming the member,
// and writes nothing.
//
// Every door is driven end to end and asserted on stored state, with a
// single-member control through the same door, so a 400 cannot come from the
// request being malformed in some other way.
func TestRepeatedRequestMemberIsRefused(t *testing.T) {
	// Three spellings of a repeat. The case-variant one is a repeat because
	// encoding/json decodes `FIELDS_PATCH` into the fields_patch field. The
	// null-first one drops nothing, but it is still two members for one field,
	// and the ruling is to refuse a repeat outright.
	repeats := []struct {
		name  string
		build func(member, first, second string) string
	}{
		{"exact", func(m, a, b string) string { return `"` + m + `":` + a + `,"` + m + `":` + b }},
		{"case variant", func(m, a, b string) string { return `"` + m + `":` + a + `,"` + strings.ToUpper(m) + `":` + b }},
		{"null first", func(m, a, b string) string { return `"` + m + `":null,"` + m + `":` + b }},
	}

	setup := func(t *testing.T) (*Server, string, *models.Item) {
		t.Helper()
		srv := testServer(t)
		ws := createWSWithCollections(t, srv)
		wsm, _ := srv.store.GetWorkspaceBySlug(ws)
		for _, name := range []string{"Things", "Things2"} {
			schema := `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]},` +
				`{"key":"priority","type":"select","options":["low","high"]}]}`
			rr := doRequest(srv, "POST", "/api/v1/workspaces/"+ws+"/collections", map[string]any{"name": name, "schema": schema})
			if rr.Code != http.StatusCreated {
				t.Fatalf("coll %d %s", rr.Code, rr.Body.String())
			}
		}
		coll, _ := srv.store.GetCollectionBySlug(wsm.ID, "things")
		it, err := srv.store.CreateItem(wsm.ID, coll.ID, models.ItemCreate{Title: "x", Fields: `{"status":"open","priority":"low"}`})
		if err != nil {
			t.Fatal(err)
		}
		return srv, ws, it
	}
	itemPath := func(ws string, it *models.Item) string { return "/api/v1/workspaces/" + ws + "/items/" + it.Slug }
	refused := func(t *testing.T, code int, body, member string) {
		t.Helper()
		if code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d %s", code, body)
		}
		if !strings.Contains(body, member+" appears more than once") {
			t.Fatalf("400 does not name the repeated member %s: %s", member, body)
		}
		if strings.Contains(body, "invalid JSON") {
			t.Fatalf("the refusal kept the decode wrapper: %s", body)
		}
	}

	t.Run("PATCH fields_patch", func(t *testing.T) {
		t.Run("control: one member", func(t *testing.T) {
			srv, ws, it := setup(t)
			rr := rawJSONRequest(srv, "PATCH", itemPath(ws, it), `{"fields_patch":{"status":"done"}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			if raw := mustGetItemFields(t, srv, it.ID); !strings.Contains(raw, `"status":"done"`) {
				t.Fatalf("control write did not land: %s", raw)
			}
		})
		for _, rp := range repeats {
			t.Run(rp.name, func(t *testing.T) {
				srv, ws, it := setup(t)
				before := mustGetItemFields(t, srv, it.ID)
				body := `{` + rp.build("fields_patch", `{"status":"done"}`, `{"priority":"high"}`) + `}`
				rr := rawJSONRequest(srv, "PATCH", itemPath(ws, it), body)
				refused(t, rr.Code, rr.Body.String(), "fields_patch")
				if after := mustGetItemFields(t, srv, it.ID); after != before {
					t.Fatalf("a refused write changed the row: %s -> %s", before, after)
				}
			})
		}
	})

	t.Run("move field_overrides", func(t *testing.T) {
		t.Run("control: one member", func(t *testing.T) {
			srv, ws, it := setup(t)
			rr := rawJSONRequest(srv, "POST", itemPath(ws, it)+"/move", `{"target_collection":"things2","field_overrides":{"priority":"high"}}`)
			if rr.Code != http.StatusOK {
				t.Fatalf("%d %s", rr.Code, rr.Body.String())
			}
			if moved, _ := srv.store.GetItem(it.ID); moved == nil || moved.CollectionID == it.CollectionID {
				t.Fatal("control move did not land")
			}
		})
		for _, rp := range repeats {
			t.Run(rp.name, func(t *testing.T) {
				srv, ws, it := setup(t)
				body := `{"target_collection":"things2",` + rp.build("field_overrides", `{"status":"done"}`, `{"priority":"high"}`) + `}`
				rr := rawJSONRequest(srv, "POST", itemPath(ws, it)+"/move", body)
				refused(t, rr.Code, rr.Body.String(), "field_overrides")
				if moved, _ := srv.store.GetItem(it.ID); moved == nil || moved.CollectionID != it.CollectionID {
					t.Fatal("a refused move moved the item")
				}
			})
		}
	})

	// The copy and its preflight share one request struct and one decode.
	// Both are driven, because sharing is a claim about wiring.
	for _, door := range []string{"copy/preflight", "copy"} {
		t.Run(door+" field_overrides", func(t *testing.T) {
			build := func(t *testing.T) (*Server, string, string, *models.Item) {
				srv, ws, it := setup(t)
				rr := doRequest(srv, "POST", "/api/v1/workspaces", map[string]string{"name": "Dest"})
				var w models.Workspace
				parseJSON(t, rr, &w)
				rr = doRequest(srv, "POST", "/api/v1/workspaces/"+w.Slug+"/collections", map[string]any{"name": "Things",
					"schema": `{"fields":[{"key":"status","type":"select","options":["open","done"],"terminal_options":["done"]},{"key":"priority","type":"select","options":["low","high"]}]}`})
				if rr.Code != http.StatusCreated {
					t.Fatalf("dest coll %d %s", rr.Code, rr.Body.String())
				}
				return srv, ws, w.Slug, it
			}
			destCount := func(t *testing.T, srv *Server, dest string) int {
				t.Helper()
				wsm, _ := srv.store.GetWorkspaceBySlug(dest)
				items, err := srv.store.ListItems(wsm.ID, models.ItemListParams{})
				if err != nil {
					t.Fatal(err)
				}
				n := 0
				for _, i := range items {
					if i.Title == "x" {
						n++
					}
				}
				return n
			}
			want := http.StatusOK
			if door == "copy" {
				want = http.StatusCreated
			}
			t.Run("control: one member", func(t *testing.T) {
				srv, ws, dest, it := build(t)
				rr := rawJSONRequest(srv, "POST", itemPath(ws, it)+"/"+door,
					`{"target_workspace":"`+dest+`","target_collection":"things","field_overrides":{"priority":"high"}}`)
				if rr.Code != want {
					t.Fatalf("%d %s", rr.Code, rr.Body.String())
				}
				if door == "copy" && destCount(t, srv, dest) != 1 {
					t.Fatal("control copy did not land")
				}
			})
			for _, rp := range repeats {
				t.Run(rp.name, func(t *testing.T) {
					srv, ws, dest, it := build(t)
					body := `{"target_workspace":"` + dest + `","target_collection":"things",` +
						rp.build("field_overrides", `{"status":"done"}`, `{"priority":"high"}`) + `}`
					rr := rawJSONRequest(srv, "POST", itemPath(ws, it)+"/"+door, body)
					refused(t, rr.Code, rr.Body.String(), "field_overrides")
					if n := destCount(t, srv, dest); n != 0 {
						t.Fatalf("a refused %s created %d item(s) in the destination", door, n)
					}
				})
			}
		})
	}
}
